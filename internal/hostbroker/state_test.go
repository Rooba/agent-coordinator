package hostbroker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

// Credential files must stay owner-only; Windows just cannot report that.
func restrictedFileMode(mode os.FileMode) bool {
	perm := mode.Perm()
	if perm == 0o600 {
		return true
	}
	return runtime.GOOS == "windows" && perm == 0o666
}

type testProtector struct{}

func (testProtector) Seal(value []byte) ([]byte, error) {
	result := append([]byte("sealed:"), value...)
	for index := len("sealed:"); index < len(result); index++ {
		result[index] ^= 0x55
	}
	return result, nil
}

func (testProtector) Open(value []byte) ([]byte, error) {
	if !bytes.HasPrefix(value, []byte("sealed:")) {
		return nil, errors.New("not sealed")
	}
	result := append([]byte(nil), value[len("sealed:"):]...)
	for index := range result {
		result[index] ^= 0x55
	}
	return result, nil
}

func TestPairAndCredentialFileNeverStorePlainToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	store, err := NewFileCredentialStore(path, testProtector{})
	if err != nil {
		t.Fatal(err)
	}
	if err := Pair(context.Background(), store, strings.NewReader(testToken+"\n")); err != nil {
		t.Fatal(err)
	}
	credential, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if credential.Token != testToken || !strings.HasPrefix(credential.LauncherSession, "launcher-") || len(credential.SessionSecret) != 64 {
		t.Fatalf("credential = %+v", credential)
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte(testToken)) {
		t.Fatal("credential token stored in plaintext")
	}
	if info, _ := os.Stat(path); !restrictedFileMode(info.Mode()) {
		t.Fatalf("credential mode = %o", info.Mode().Perm())
	}
	if err := store.Save(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	if err := Pair(context.Background(), store, strings.NewReader(testToken)); err != nil {
		t.Fatal(err)
	}
	repaired, _ := store.Load(context.Background())
	if repaired.LauncherSession != credential.LauncherSession || repaired.SessionSecret != credential.SessionSecret {
		t.Fatal("pairing replaced stable launcher identity")
	}
	if err := Pair(context.Background(), store, strings.NewReader(strings.Repeat("c", 64))); err != nil {
		t.Fatal(err)
	}
	rotated, _ := store.Load(context.Background())
	if rotated.LauncherSession != repaired.LauncherSession || rotated.SessionSecret != repaired.SessionSecret {
		t.Fatal("relay token rotation replaced stable launcher identity")
	}
}

func TestPairRejectsInvalidToken(t *testing.T) {
	store := &memoryCredentials{}
	for _, token := range []string{"short", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("x", 64), strings.Repeat("a", 65)} {
		if err := Pair(context.Background(), store, strings.NewReader(token)); err == nil {
			t.Fatalf("accepted token %q", token[:min(len(token), 16)])
		}
	}
}

func TestCredentialRequiresStrictPremintedSecret(t *testing.T) {
	credential := Credential{Token: testToken, LauncherSession: "launcher-fixed", SessionSecret: testLauncherSecret}
	for _, secret := range []string{"", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		credential.SessionSecret = secret
		if err := credential.Validate(); err == nil {
			t.Fatalf("accepted launcher secret %q", secret[:min(len(secret), 8)])
		}
	}
}

func TestFileJournalSealsAndRestoresTerminalState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal")
	journal, err := NewFileJournal(path, testProtector{})
	if err != nil {
		t.Fatal(err)
	}
	launch := launchMessage("task-00000000000a")
	record := TaskRecord{
		Launch: launch, Child: protocol.AgentRef{Name: "eyes", Scope: launch.Scope}, ChildSession: "eyes-" + launch.TaskID,
		ChildSecret: testChildSecret, State: recordTerminal, Terminal: failedBody(launch.TaskID, "failed"),
	}
	if err := journal.Put(record); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte(testChildSecret)) || bytes.Contains(raw, []byte(launch.Brief)) {
		t.Fatal("journal stored sensitive state in plaintext")
	}
	reopened, err := NewFileJournal(path, testProtector{})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get(launch.TaskID)
	if !ok || got.ChildSecret != record.ChildSecret || !bytes.Equal(got.Terminal, record.Terminal) {
		t.Fatalf("restored record = %+v", got)
	}
	if err := reopened.Put(TaskRecord{Launch: launch, State: recordDelivered}); err != nil {
		t.Fatal(err)
	}
	tombstone, ok := reopened.Get(launch.TaskID)
	if !ok || tombstone.State != recordDelivered || tombstone.ChildSecret != "" || tombstone.Launch.Brief != "" || len(tombstone.Terminal) != 0 {
		t.Fatalf("delivered tombstone retained task data: %+v", tombstone)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileJournal(path, testProtector{}); err == nil {
		t.Fatal("symlink journal accepted")
	}
}

func TestFileJournalBoundsRecordsByEvictingDeliveredTombstone(t *testing.T) {
	records := make(map[string]TaskRecord, maxJournalRecords)
	for index := range maxJournalRecords {
		id := fmt.Sprintf("task-%012x", index)
		records[id] = TaskRecord{Launch: protocol.TaskLaunchMsg{Type: protocol.TaskLaunch, TaskID: id}, State: recordDelivered}
	}
	journal := &FileJournal{path: filepath.Join(t.TempDir(), "journal"), protector: testProtector{}, records: records}
	launch := launchMessage("task-ffffffffffff")
	if err := journal.Put(TaskRecord{Launch: launch, State: recordReceived}); err != nil {
		t.Fatal(err)
	}
	if len(journal.Records()) != maxJournalRecords {
		t.Fatalf("journal records = %d", len(journal.Records()))
	}
	if _, exists := journal.Get("task-000000000000"); exists {
		t.Fatal("oldest delivered tombstone was not evicted")
	}
	if record, exists := journal.Get(launch.TaskID); !exists || record.State != recordReceived {
		t.Fatalf("new record = %+v, exists=%v", record, exists)
	}
}

func TestFileJournalEvictsDeliveredTombstonesUnderBytePressure(t *testing.T) {
	records := make(map[string]TaskRecord)
	for index := range 8 {
		id := fmt.Sprintf("task-%012x", index)
		records[id] = TaskRecord{Launch: protocol.TaskLaunchMsg{Type: protocol.TaskLaunch, TaskID: id}, State: recordDelivered}
	}
	initial := len(records)
	journal := &FileJournal{path: filepath.Join(t.TempDir(), "journal"), protector: testProtector{}, records: records, limit: 300}
	launch := protocol.TaskLaunchMsg{Type: protocol.TaskLaunch, TaskID: "task-ffffffffffff"}
	if err := journal.Put(TaskRecord{Launch: launch, State: recordReceived}); err != nil {
		t.Fatal(err)
	}
	if len(journal.Records()) >= initial+1 {
		t.Fatal("journal did not evict delivered tombstones to satisfy its byte limit")
	}
	if record, exists := journal.Get(launch.TaskID); !exists || record.State != recordReceived {
		t.Fatalf("new record = %+v, exists=%v", record, exists)
	}
	sealed, err := os.ReadFile(journal.path)
	if err != nil || len(sealed) > journal.limit {
		t.Fatalf("sealed journal size = %d, err = %v", len(sealed), err)
	}
}

func TestFileJournalNeverEvictsUndeliveredWorkForSpace(t *testing.T) {
	existing := protocol.TaskLaunchMsg{Type: protocol.TaskLaunch, TaskID: "task-000000000001"}
	journal := &FileJournal{
		path: filepath.Join(t.TempDir(), "journal"), protector: testProtector{}, limit: 1,
		records: map[string]TaskRecord{existing.TaskID: {Launch: existing, State: recordReceived}},
	}
	incoming := protocol.TaskLaunchMsg{Type: protocol.TaskLaunch, TaskID: "task-000000000002"}
	if err := journal.Put(TaskRecord{Launch: incoming, State: recordReceived}); err == nil {
		t.Fatal("oversized undelivered journal was accepted")
	}
	if _, exists := journal.Get(existing.TaskID); !exists {
		t.Fatal("undelivered record was evicted")
	}
	if _, exists := journal.Get(incoming.TaskID); exists {
		t.Fatal("failed incoming record was retained")
	}
}

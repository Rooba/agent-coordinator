package hostbroker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

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
	if credential.Token != testToken || !strings.HasPrefix(credential.LauncherSession, "launcher-") {
		t.Fatalf("credential = %+v", credential)
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte(testToken)) {
		t.Fatal("credential token stored in plaintext")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %o", info.Mode().Perm())
	}
	credential.SessionSecret = testLauncherSecret
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
	if err := Pair(context.Background(), store, strings.NewReader(strings.Repeat("z", 64))); err != nil {
		t.Fatal(err)
	}
	rotated, _ := store.Load(context.Background())
	if rotated.LauncherSession != repaired.LauncherSession || rotated.SessionSecret != repaired.SessionSecret {
		t.Fatal("relay token rotation replaced stable launcher identity")
	}
}

func TestPairRejectsInvalidToken(t *testing.T) {
	store := &memoryCredentials{}
	for _, token := range []string{"short", strings.Repeat("x", 32) + " bad", strings.Repeat("x", 513)} {
		if err := Pair(context.Background(), store, strings.NewReader(token)); err == nil {
			t.Fatalf("accepted token %q", token[:min(len(token), 16)])
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

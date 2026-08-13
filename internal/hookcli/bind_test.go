package hookcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/scope"
)

// Bind files must never leak into the real state dir from tests: disabled by
// default, individual tests opt in via stubBind.
func TestMain(m *testing.M) {
	bindDirFn = func() (string, error) { return "", errors.New("bind disabled in tests") }
	PidAlive = func(int) bool { return true } // real /proc liveness is stubbed per test
	os.Exit(m.Run())
}

func stubBind(t *testing.T, chain []int) string {
	t.Helper()
	dir := t.TempDir()
	oldDir, oldAnc := bindDirFn, ancestryFn
	bindDirFn = func() (string, error) { return dir, nil }
	ancestryFn = func() []int { return chain }
	t.Cleanup(func() { bindDirFn, ancestryFn = oldDir, oldAnc })
	return dir
}

func readBind(t *testing.T, dir string, anchor string) Bind {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, anchor+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var b Bind
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// The fixture session's scope, canonicalized the same way Run resolves it.
var fixtureScope = scope.Resolve("/home/user/agent-coordinator-go")

func TestSessionStartWritesBindFile(t *testing.T) {
	dir := stubBind(t, []int{1234, 42})
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "amber-fox"})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "session_start.json")), &out, sock)
	if len(*got) != 1 || (*got)[0].Source != "hook" {
		t.Fatalf("register must carry source=hook: %+v", *got)
	}
	b := readBind(t, dir, "1234")
	if b.SessionID != "3cdc4c5b-e78d-4fbb-9859-f18d8dc2b200" || b.Scope != fixtureScope ||
		b.Name != "amber-fox" || len(b.Pids) != 2 || b.Pids[0] != 1234 || b.TS == 0 {
		t.Fatalf("bind file: %+v", b)
	}
}

func TestEventRefreshesMissingBindFile(t *testing.T) {
	dir := stubBind(t, []int{1234, 42})
	sock, _ := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "post_read.json")), &out, sock)
	b := readBind(t, dir, "1234")
	if b.SessionID != "3cdc4c5b-e78d-4fbb-9859-f18d8dc2b200" || b.Scope != fixtureScope {
		t.Fatalf("refreshed bind: %+v", b)
	}
}

func TestEventKeepsExistingBindFile(t *testing.T) {
	dir := stubBind(t, []int{1234, 42})
	WriteBind(dir, Bind{SessionID: "3cdc4c5b-e78d-4fbb-9859-f18d8dc2b200", Scope: fixtureScope,
		Name: "amber-fox", Pids: []int{1234, 42}, TS: time.Now().Unix()})
	sock, _ := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "post_read.json")), &out, sock)
	if b := readBind(t, dir, "1234"); b.Name != "amber-fox" {
		t.Fatalf("refresh must not overwrite an existing bind: %+v", b)
	}
}

func TestSessionEndRemovesBindFile(t *testing.T) {
	dir := stubBind(t, []int{1234, 42})
	WriteBind(dir, Bind{SessionID: "x", Scope: fixtureScope, Pids: []int{1234}, TS: time.Now().Unix()})
	sock, _ := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "session_end.json")), &out, sock)
	if _, err := os.Stat(filepath.Join(dir, "1234.json")); !os.IsNotExist(err) {
		t.Fatalf("bind file must be deleted on SessionEnd: %v", err)
	}
}

func TestWriteBindCleansStaleFiles(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-72 * time.Hour).Unix()
	if err := WriteBind(dir, Bind{SessionID: "old", Scope: "/r", Pids: []int{99}, TS: old}); err != nil {
		t.Fatal(err)
	}
	if err := WriteBind(dir, Bind{SessionID: "new", Scope: "/r", Pids: []int{100}, TS: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "99.json")); !os.IsNotExist(err) {
		t.Fatalf("stale bind must be cleaned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "100.json")); err != nil {
		t.Fatalf("fresh bind must survive: %v", err)
	}
}

func TestBindFailOpenWithoutAncestry(t *testing.T) {
	stubBind(t, nil) // no discoverable ancestors: bind is skipped, Run still works
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "amber-fox"})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "session_start.json")), &out, sock)
	if len(*got) != 1 {
		t.Fatalf("register must still happen: %+v", *got)
	}
}

func TestMatchBind(t *testing.T) {
	dir := t.TempDir()
	WriteBind(dir, Bind{SessionID: "sess-1", Scope: "/r", Name: "amber-fox",
		Pids: []int{500, 400}, TS: time.Now().Unix()})
	if b, ok := MatchBind(dir, "/r", []int{999, 500}); !ok || b.SessionID != "sess-1" {
		t.Fatalf("want match via pid 500: %+v ok=%v", b, ok)
	}
	if _, ok := MatchBind(dir, "/other", []int{999, 500}); ok {
		t.Fatal("scope mismatch must not match")
	}
	if _, ok := MatchBind(dir, "/r", []int{7, 8}); ok {
		t.Fatal("disjoint pids must not match")
	}
	if _, ok := MatchBind(dir, "/r", nil); ok {
		t.Fatal("empty ancestry must not match")
	}
	// pid 1 is everyone's ancestor and must never bind.
	WriteBind(dir, Bind{SessionID: "sess-2", Scope: "/q", Pids: []int{1}, TS: time.Now().Unix()})
	if _, ok := MatchBind(dir, "/q", []int{1}); ok {
		t.Fatal("pid 1 must be excluded from matching")
	}
}

// A bind matching only via a deep shared ancestor (tmux/systemd) must lose to
// a bind anchored nearer the caller.
func TestMatchBindPrefersNearestAncestor(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Unix()
	WriteBind(dir, Bind{SessionID: "shared", Scope: "/r", Pids: []int{300}, TS: now})
	WriteBind(dir, Bind{SessionID: "near", Scope: "/r", Pids: []int{200}, TS: now - 1000})
	if b, ok := MatchBind(dir, "/r", []int{100, 200, 300}); !ok || b.SessionID != "near" {
		t.Fatalf("nearest ancestor must win: %+v ok=%v", b, ok)
	}
}

func TestMatchBindCapsAncestorDepth(t *testing.T) {
	dir := t.TempDir()
	WriteBind(dir, Bind{SessionID: "deep", Scope: "/r", Pids: []int{400}, TS: time.Now().Unix()})
	if _, ok := MatchBind(dir, "/r", []int{100, 200, 300, 400}); ok {
		t.Fatal("ancestors beyond the depth cap must not match")
	}
	if b, ok := MatchBind(dir, "/r", []int{100, 200, 400}); !ok || b.SessionID != "deep" {
		t.Fatalf("third ancestor must still match: %+v ok=%v", b, ok)
	}
}

func TestMatchBindRejectsDeadAnchor(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Unix()
	WriteBind(dir, Bind{SessionID: "dead", Scope: "/r", Pids: []int{500, 300}, TS: now})
	WriteBind(dir, Bind{SessionID: "live", Scope: "/r", Pids: []int{600, 300}, TS: now})
	old := PidAlive
	t.Cleanup(func() { PidAlive = old })
	PidAlive = func(pid int) bool { return pid != 500 }
	// Both match via shared ancestor 300; the dead-anchored one is out.
	if b, ok := MatchBind(dir, "/r", []int{999, 300}); !ok || b.SessionID != "live" {
		t.Fatalf("dead anchor must be rejected: %+v ok=%v", b, ok)
	}
	PidAlive = func(int) bool { return false }
	if _, ok := MatchBind(dir, "/r", []int{999, 300}); ok {
		t.Fatal("no live anchors must mean no match")
	}
}

func TestMatchBindRejectsStaleTS(t *testing.T) {
	dir := t.TempDir()
	WriteBind(dir, Bind{SessionID: "stale", Scope: "/r", Pids: []int{500},
		TS: time.Now().Add(-49 * time.Hour).Unix()})
	if _, ok := MatchBind(dir, "/r", []int{500}); ok {
		t.Fatal("stale bind must not match")
	}
}

func TestMatchBindTieBreaksOnNewestTS(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Unix()
	WriteBind(dir, Bind{SessionID: "older", Scope: "/r", Pids: []int{700, 500}, TS: now - 3600})
	WriteBind(dir, Bind{SessionID: "newer", Scope: "/r", Pids: []int{800, 500}, TS: now})
	if b, ok := MatchBind(dir, "/r", []int{999, 500}); !ok || b.SessionID != "newer" {
		t.Fatalf("newest TS must win the tie: %+v ok=%v", b, ok)
	}
}

func TestAncestryStartsAtParent(t *testing.T) {
	chain := Ancestry()
	if len(chain) == 0 || chain[0] != os.Getppid() {
		t.Fatalf("ancestry: %v (ppid %d)", chain, os.Getppid())
	}
	if len(chain) > 6 {
		t.Fatalf("ancestry too deep: %v", chain)
	}
	for _, pid := range chain {
		if pid <= 1 {
			t.Fatalf("chain contains pid <= 1: %v", chain)
		}
	}
}

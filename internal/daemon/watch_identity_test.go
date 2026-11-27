package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/store"
)

const watchScope = "/watch-identity"

func watchStore(t *testing.T) (*store.Store, func(time.Duration)) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "watch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Unix(1_800_000_000, 0)
	st.Now = func() time.Time { return now }
	return st, func(d time.Duration) { now = now.Add(d) }
}

func watchCall(t *testing.T, st *store.Store, req protocol.Request) protocol.Response {
	t.Helper()
	req.Scope = watchScope
	resp := dispatch(st, req)
	if !resp.OK {
		t.Fatalf("%s failed: %+v", req.Op, resp)
	}
	return resp
}

func watchAgent(t *testing.T, st *store.Store, name string) protocol.AgentInfo {
	t.Helper()
	for _, a := range watchCall(t, st, protocol.Request{Op: protocol.OpBoard, IncludeGone: true}).Agents {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("board omitted %s", name)
	return protocol.AgentInfo{}
}

// A monitor bound to session A keeps peeking A's inbox after A is purged and
// its name is minted again for session B: the probe fails instead of reading
// (or stamping) B, and follows A again once A re-registers under a new name.
func TestWatcherPeekFollowsBoundSessionAcrossNameReuse(t *testing.T) {
	st, advance := watchStore(t)
	const sessionA, sessionB = "watch-session-a", "watch-session-b115" // same friendly name
	a := watchCall(t, st, protocol.Request{Op: protocol.OpRegister, SessionID: sessionA, Source: "hook"})
	watchCall(t, st, protocol.Request{Op: protocol.OpRegister, SessionID: "sender", Source: "hook"})
	watchCall(t, st, protocol.Request{Op: protocol.OpSend, SessionID: "sender", To: a.Name, Body: "for A"})
	probe := protocol.Request{Op: protocol.OpPeek, SessionID: sessionA, Watcher: true}
	if r := watchCall(t, st, probe); r.Unread != 1 {
		t.Fatalf("bound probe must see A's mail: %+v", r)
	}

	advance(3 * time.Hour)
	if err := st.Housekeep(); err != nil {
		t.Fatal(err)
	}
	b := watchCall(t, st, protocol.Request{Op: protocol.OpRegister, SessionID: sessionB, Source: "hook"})
	if b.Name != a.Name {
		t.Fatalf("test setup: B got %q, want A's recycled name %q", b.Name, a.Name)
	}
	watchCall(t, st, protocol.Request{Op: protocol.OpRegister, SessionID: "sender", Source: "hook"})
	watchCall(t, st, protocol.Request{Op: protocol.OpSend, SessionID: "sender", To: b.Name, Body: "for B"})
	probe.Scope = watchScope
	if r := dispatch(st, probe); r.OK {
		t.Fatalf("probe for purged session A must fail, not read B's inbox: %+v", r)
	}
	if w := watchCall(t, st, protocol.Request{Op: protocol.OpWhoami, SessionID: sessionB}).Wake; w != "manual" {
		t.Fatalf("B must not be stamped by A's monitor: wake=%q", w)
	}

	a2 := watchCall(t, st, protocol.Request{Op: protocol.OpRegister, SessionID: sessionA, Source: "hook"})
	if a2.Name == b.Name {
		t.Fatalf("re-registered A must get a fresh name, got %q", a2.Name)
	}
	if r := watchCall(t, st, probe); r.Unread != 0 {
		t.Fatalf("probe must follow session A, not the name: %+v", r)
	}
	if w := watchCall(t, st, protocol.Request{Op: protocol.OpWhoami, SessionID: sessionA}).Wake; w != "monitor" {
		t.Fatalf("A's monitor must stamp A: wake=%q", w)
	}
	if w := watchCall(t, st, protocol.Request{Op: protocol.OpWhoami, SessionID: sessionB}).Wake; w != "manual" {
		t.Fatalf("B must stay unstamped: wake=%q", w)
	}
}

// A watcher peek is the monitor polling, not the agent working: it must not
// refresh last_seen or lift idle, while an ordinary identified peek still does.
func TestWatcherPeekIsNotAHeartbeat(t *testing.T) {
	st, advance := watchStore(t)
	const session = "watch-heartbeat"
	a := watchCall(t, st, protocol.Request{Op: protocol.OpRegister, SessionID: session, Source: "hook"})
	watchCall(t, st, protocol.Request{Op: protocol.OpIdle, SessionID: session})
	before := watchAgent(t, st, a.Name)
	if before.Status != "idle" {
		t.Fatalf("test setup: status=%q, want idle", before.Status)
	}

	advance(30 * time.Second)
	watchCall(t, st, protocol.Request{Op: protocol.OpPeek, SessionID: session, Watcher: true})
	if got := watchAgent(t, st, a.Name); got.Status != "idle" || got.LastSeen != before.LastSeen {
		t.Fatalf("watcher peek refreshed presence: status=%q last_seen=%d (was %d)", got.Status, got.LastSeen, before.LastSeen)
	}

	watchCall(t, st, protocol.Request{Op: protocol.OpPeek, SessionID: session})
	if got := watchAgent(t, st, a.Name); got.Status != "active" || got.LastSeen <= before.LastSeen {
		t.Fatalf("plain peek must heartbeat: status=%q last_seen=%d (was %d)", got.Status, got.LastSeen, before.LastSeen)
	}
}

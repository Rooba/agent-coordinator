package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/store"
)

func monitorContractStore(t *testing.T) (*store.Store, func(time.Duration)) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Unix(1_800_000_000, 0)
	st.Now = func() time.Time { return now }
	return st, func(d time.Duration) { now = now.Add(d) }
}

func monitorContractCall(t *testing.T, st *store.Store, req protocol.Request) protocol.Response {
	t.Helper()
	req.Scope = "/monitor-contract"
	resp := dispatch(st, req)
	if !resp.OK {
		t.Fatalf("%s failed: %+v", req.Op, resp)
	}
	return resp
}

func assertMonitorContractWake(t *testing.T, st *store.Store, session, want string) {
	t.Helper()
	identity := monitorContractCall(t, st, protocol.Request{Op: protocol.OpWhoami, SessionID: session})
	if identity.Wake != want {
		t.Errorf("whoami(%s).wake = %q, want %q", identity.Name, identity.Wake, want)
	}
	board := monitorContractCall(t, st, protocol.Request{Op: protocol.OpBoard})
	for _, agent := range board.Agents {
		if agent.Name == identity.Name {
			if agent.Wake != want {
				t.Errorf("status_board(%s).wake = %q, want %q", agent.Name, agent.Wake, want)
			}
			return
		}
	}
	t.Fatalf("status_board omitted %s", identity.Name)
}

func TestMonitorContractRequiresWatcherPeek(t *testing.T) {
	st, _ := monitorContractStore(t)
	owner := monitorContractCall(t, st, protocol.Request{Op: protocol.OpRegister, SessionID: "owner", Source: "hook"})
	monitorContractCall(t, st, protocol.Request{Op: protocol.OpRegister, SessionID: "sender", Source: "hook"})
	monitorContractCall(t, st, protocol.Request{Op: protocol.OpSend, SessionID: "sender", To: owner.Name, Body: "keep this unread"})
	assertMonitorContractWake(t, st, "owner", "manual")

	peek := monitorContractCall(t, st, protocol.Request{Op: protocol.OpPeek, SessionID: "owner"})
	if peek.Unread != 1 {
		t.Fatalf("ordinary peek unread = %d, want 1", peek.Unread)
	}
	// A marker on another operation is not evidence of a running watcher.
	monitorContractCall(t, st, protocol.Request{Op: protocol.OpWhoami, SessionID: "owner", Watcher: true})
	assertMonitorContractWake(t, st, "owner", "manual")

	peek = monitorContractCall(t, st, protocol.Request{Op: protocol.OpPeek, SessionID: "owner", Watcher: true})
	if peek.Unread != 1 {
		t.Fatalf("watcher peek unread = %d, want 1", peek.Unread)
	}
	assertMonitorContractWake(t, st, "owner", "monitor")
	assertMonitorContractWake(t, st, "sender", "manual")
	read := monitorContractCall(t, st, protocol.Request{Op: protocol.OpRead, SessionID: "owner"})
	if len(read.Messages) != 1 || read.Messages[0].Body != "keep this unread" {
		t.Fatalf("watcher peek must not consume mail: %+v", read.Messages)
	}
}

func TestMonitorContractExpiresUnlessWatcherRefreshes(t *testing.T) {
	st, advance := monitorContractStore(t)
	monitorContractCall(t, st, protocol.Request{Op: protocol.OpRegister, SessionID: "owner", Source: "hook"})
	watcher := protocol.Request{Op: protocol.OpPeek, SessionID: "owner", Watcher: true}
	monitorContractCall(t, st, watcher)

	advance(9 * time.Second)
	monitorContractCall(t, st, protocol.Request{Op: protocol.OpPeek, SessionID: "owner"})
	assertMonitorContractWake(t, st, "owner", "monitor")
	advance(2 * time.Second)
	assertMonitorContractWake(t, st, "owner", "manual")

	monitorContractCall(t, st, watcher)
	assertMonitorContractWake(t, st, "owner", "monitor")
	advance(9 * time.Second)
	monitorContractCall(t, st, watcher)
	advance(9 * time.Second)
	assertMonitorContractWake(t, st, "owner", "monitor")
	advance(2 * time.Second)
	assertMonitorContractWake(t, st, "owner", "manual")
}

func TestMonitorContractWatcherPreservesIdle(t *testing.T) {
	st, advance := monitorContractStore(t)
	monitorContractCall(t, st, protocol.Request{Op: protocol.OpRegister, SessionID: "owner", Source: "hook"})
	monitorContractCall(t, st, protocol.Request{Op: protocol.OpIdle, SessionID: "owner", Source: "presence"})
	board := monitorContractCall(t, st, protocol.Request{Op: protocol.OpBoard})
	if len(board.Agents) != 1 || board.Agents[0].Status != "idle" {
		t.Fatalf("expected one idle agent before watching: %+v", board.Agents)
	}
	before := board.Agents[0]

	advance(5 * time.Second)
	monitorContractCall(t, st, protocol.Request{Op: protocol.OpPeek, SessionID: "owner", Watcher: true})
	board = monitorContractCall(t, st, protocol.Request{Op: protocol.OpBoard})
	if len(board.Agents) != 1 {
		t.Fatalf("expected one agent after watcher peek: %+v", board.Agents)
	}
	if after := board.Agents[0]; after.Status != "idle" || after.LastSeen != before.LastSeen || after.Wake != "monitor" {
		t.Fatalf("watcher must arm without changing agent activity: before=%+v after=%+v", before, after)
	}

	// A normal identified call still represents agent activity.
	advance(5 * time.Second)
	monitorContractCall(t, st, protocol.Request{Op: protocol.OpPeek, SessionID: "owner"})
	board = monitorContractCall(t, st, protocol.Request{Op: protocol.OpBoard})
	if len(board.Agents) != 1 || board.Agents[0].Status != "active" || board.Agents[0].LastSeen != st.Now().Unix() {
		t.Fatalf("ordinary peek must still refresh activity: %+v", board.Agents)
	}
}

func TestMonitorContractRejectsForeignWatcher(t *testing.T) {
	st, _ := monitorContractStore(t)
	owner := monitorContractCall(t, st, protocol.Request{Op: protocol.OpRegister, SessionID: "owner", Source: "hook"})
	peer := monitorContractCall(t, st, protocol.Request{Op: protocol.OpRegister, SessionID: "peer", Source: "hook"})
	denied := dispatch(st, protocol.Request{Op: protocol.OpPeek, Scope: "/monitor-contract",
		SessionID: "owner", From: peer.Name, Watcher: true})
	if denied.OK || denied.Error != "foreign session" {
		t.Fatalf("foreign watcher must be refused: %+v", denied)
	}
	assertMonitorContractWake(t, st, "owner", "manual")
	assertMonitorContractWake(t, st, "peer", "manual")

	// Explicit-name local CLI callers retain the same actor resolution as wait.
	monitorContractCall(t, st, protocol.Request{Op: protocol.OpPeek, From: owner.Name, Watcher: true})
	assertMonitorContractWake(t, st, "owner", "monitor")
	assertMonitorContractWake(t, st, "peer", "manual")
}

func TestMonitorContractChildrenRemainManual(t *testing.T) {
	st, advance := monitorContractStore(t)
	monitorContractCall(t, st, protocol.Request{Op: protocol.OpRegister, SessionID: "parent", Source: "hook"})
	child := monitorContractCall(t, st, protocol.Request{Op: protocol.OpRegister, SessionID: "parent",
		Source: "hook-subagent", AgentID: "child", AgentType: "Explore"})
	childSession := store.ChildSessionID("parent", "child")
	monitorContractCall(t, st, protocol.Request{Op: protocol.OpPeek, SessionID: "parent", Watcher: true})
	assertMonitorContractWake(t, st, "parent", "monitor")
	assertMonitorContractWake(t, st, childSession, "manual")

	advance(9 * time.Second)
	monitorContractCall(t, st, protocol.Request{Op: protocol.OpPeek, SessionID: "parent", From: child.Name, Watcher: true})
	assertMonitorContractWake(t, st, childSession, "manual")
	advance(2 * time.Second)
	assertMonitorContractWake(t, st, "parent", "manual")
	assertMonitorContractWake(t, st, childSession, "manual")
}

func TestMonitorContractAnnouncesOnlyLiveWatcher(t *testing.T) {
	st, advance := monitorContractStore(t)
	monitorContractCall(t, st, protocol.Request{Op: protocol.OpRegister, SessionID: "parent", Source: "hook"})
	prompt := protocol.Request{Op: protocol.OpEvent, SessionID: "parent", Tool: "UserPromptSubmit"}
	watcher := protocol.Request{Op: protocol.OpPeek, SessionID: "parent", Watcher: true}
	assertAnnouncement := func(req protocol.Request, want bool) {
		t.Helper()
		if resp := monitorContractCall(t, st, req); resp.MonitorArmed != want {
			t.Errorf("%s agent=%q monitor_armed = %t, want %t", req.Tool, req.AgentID, resp.MonitorArmed, want)
		}
	}
	assertAnnouncement(prompt, false)
	monitorContractCall(t, st, watcher)
	assertAnnouncement(protocol.Request{Op: protocol.OpEvent, SessionID: "parent", Tool: "Read"}, false)
	assertAnnouncement(protocol.Request{Op: protocol.OpEvent, SessionID: "parent", Tool: "UserPromptSubmit",
		AgentID: "child", AgentType: "Explore"}, false)
	assertAnnouncement(prompt, true)
	assertAnnouncement(prompt, false)

	// A monitor can lapse and restart without a prompt during the gap. Its
	// renewed liveness must still get an announcement on the next prompt.
	advance(11 * time.Second)
	monitorContractCall(t, st, watcher)
	assertAnnouncement(prompt, true)
	assertAnnouncement(prompt, false)
	advance(11 * time.Second)
	assertAnnouncement(prompt, false)
	assertMonitorContractWake(t, st, "parent", "manual")
}

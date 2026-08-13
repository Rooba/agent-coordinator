package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/socktest"
	"github.com/Rooba/agent-coordinator/internal/store"
)

func roundTrip(t *testing.T, sock string, req protocol.Request) protocol.Response {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp protocol.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func startDaemon(t *testing.T, idle time.Duration) (string, chan error) {
	t.Helper()
	dir := socktest.Dir(t)
	sock := filepath.Join(dir, "d.sock")
	st, err := store.Open(filepath.Join(dir, "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() { done <- Serve(l, st, idle); close(exited) }()
	// Drive the real shutdown path before the temp dir is removed: closing
	// the listener ends Serve, which joins its goroutines and closes the
	// store, so the db handle is released (windows cannot delete open files).
	t.Cleanup(func() {
		l.Close()
		<-exited
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		default: // the test consumed it (idle-exit path)
		}
	})
	return sock, done
}

func TestRegisterEventAgentsFlow(t *testing.T) {
	sock, _ := startDaemon(t, time.Minute)
	r := roundTrip(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: "/r", SessionID: "s1", Source: "startup"})
	if !r.OK || r.Name == "" {
		t.Fatalf("register: %+v", r)
	}
	r = roundTrip(t, sock, protocol.Request{Op: protocol.OpEvent, Scope: "/r", SessionID: "s1", Tool: "Read", Activity: "Reading x"})
	if !r.OK {
		t.Fatalf("event: %+v", r)
	}
	r = roundTrip(t, sock, protocol.Request{Op: protocol.OpAgents, Scope: "/r"})
	if !r.OK || len(r.Agents) != 1 {
		t.Fatalf("agents: %+v", r)
	}
	r = roundTrip(t, sock, protocol.Request{Op: "bogus", Scope: "/r"})
	if r.OK || r.Error == "" {
		t.Fatalf("bogus op must error: %+v", r)
	}
}

func TestIdleReturnsNoticesOnce(t *testing.T) {
	sock, _ := startDaemon(t, time.Minute)
	a := roundTrip(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: "/r", SessionID: "sa", Source: "startup"})
	b := roundTrip(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: "/r", SessionID: "sb", Source: "startup"})
	r := roundTrip(t, sock, protocol.Request{Op: protocol.OpSend, Scope: "/r", From: a.Name, To: b.Name, Body: "ping"})
	if !r.OK {
		t.Fatalf("send: %+v", r)
	}
	r = roundTrip(t, sock, protocol.Request{Op: protocol.OpIdle, Scope: "/r", SessionID: "sb"})
	if !r.OK || len(r.Notices) != 1 || !strings.Contains(r.Notices[0], a.Name) {
		t.Fatalf("first idle must carry one notice naming %s: %+v", a.Name, r)
	}
	r = roundTrip(t, sock, protocol.Request{Op: protocol.OpIdle, Scope: "/r", SessionID: "sb"})
	if !r.OK || len(r.Notices) != 0 {
		t.Fatalf("second idle must carry no notices (no Stop loop): %+v", r)
	}
	// Peek still sees the unread mail: idle consumed the nudge, not the message.
	r = roundTrip(t, sock, protocol.Request{Op: protocol.OpPeek, Scope: "/r", From: b.Name})
	if !r.OK || r.Unread != 1 {
		t.Fatalf("peek: %+v", r)
	}
}

// Requests carrying AgentID target the child row: register mints the child
// name, events land on the child, deregister retires the child only.
func TestSubagentRequestsTargetChildRow(t *testing.T) {
	sock, _ := startDaemon(t, time.Minute)
	parent := roundTrip(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: "/r", SessionID: "p1", Source: "hook"})
	child := roundTrip(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: "/r", SessionID: "p1",
		Source: "hook-subagent", AgentID: "a1", AgentType: "Explore"})
	if !child.OK || child.Name != parent.Name+"/explore-1" {
		t.Fatalf("child register: %+v", child)
	}
	r := roundTrip(t, sock, protocol.Request{Op: protocol.OpEvent, Scope: "/r", SessionID: "p1",
		AgentID: "a1", AgentType: "Explore", Tool: "Read", Activity: "Reading x"})
	if !r.OK {
		t.Fatalf("child event: %+v", r)
	}
	r = roundTrip(t, sock, protocol.Request{Op: protocol.OpBoard, Scope: "/r"})
	if !r.OK || len(r.Agents) != 2 {
		t.Fatalf("board: %+v", r)
	}
	for _, a := range r.Agents {
		switch a.Name {
		case parent.Name:
			if a.Parent != "" || a.Activity != "" {
				t.Fatalf("parent row must stay clean: %+v", a)
			}
		case child.Name:
			if a.Parent != parent.Name || a.Activity != "Reading x" {
				t.Fatalf("child row: %+v", a)
			}
		default:
			t.Fatalf("unexpected agent %+v", a)
		}
	}
	// SubagentStop: only the child goes gone, and the default board hides it.
	r = roundTrip(t, sock, protocol.Request{Op: protocol.OpDeregister, Scope: "/r", SessionID: "p1", AgentID: "a1"})
	if !r.OK {
		t.Fatalf("child deregister: %+v", r)
	}
	r = roundTrip(t, sock, protocol.Request{Op: protocol.OpBoard, Scope: "/r"})
	if len(r.Agents) != 1 || r.Agents[0].Name != parent.Name || r.Agents[0].Status != "active" {
		t.Fatalf("default board must show only the live parent: %+v", r.Agents)
	}
	r = roundTrip(t, sock, protocol.Request{Op: protocol.OpBoard, Scope: "/r", IncludeGone: true})
	seen := false
	for _, a := range r.Agents {
		if a.Name == child.Name && a.Status == "gone" {
			seen = true
		}
	}
	if len(r.Agents) != 2 || !seen {
		t.Fatalf("include_gone board must keep the gone child: %+v", r.Agents)
	}
}

// The heartbeat: an MCP tool call carrying the bound session id lifts sticky
// idle back to active, and never resurrects a gone row.
func TestToolCallHeartbeatRefreshesIdleRow(t *testing.T) {
	sock, _ := startDaemon(t, time.Minute)
	roundTrip(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: "/r", SessionID: "s1", Source: "hook"})
	roundTrip(t, sock, protocol.Request{Op: protocol.OpIdle, Scope: "/r", SessionID: "s1"})
	r := roundTrip(t, sock, protocol.Request{Op: protocol.OpAgents, Scope: "/r"})
	if len(r.Agents) != 1 || r.Agents[0].Status != "idle" {
		t.Fatalf("precondition: want sticky idle, got %+v", r.Agents)
	}
	// A board call naming the session is enough to heartbeat it back to active.
	roundTrip(t, sock, protocol.Request{Op: protocol.OpBoard, Scope: "/r", SessionID: "s1"})
	r = roundTrip(t, sock, protocol.Request{Op: protocol.OpAgents, Scope: "/r"})
	if len(r.Agents) != 1 || r.Agents[0].Status != "active" {
		t.Fatalf("heartbeat must lift idle to active: %+v", r.Agents)
	}
	roundTrip(t, sock, protocol.Request{Op: protocol.OpDeregister, Scope: "/r", SessionID: "s1"})
	roundTrip(t, sock, protocol.Request{Op: protocol.OpBoard, Scope: "/r", SessionID: "s1"})
	r = roundTrip(t, sock, protocol.Request{Op: protocol.OpBoard, Scope: "/r", IncludeGone: true})
	if len(r.Agents) != 1 || r.Agents[0].Status != "gone" {
		t.Fatalf("heartbeat must not resurrect a gone row: %+v", r.Agents)
	}
}

func TestWhoamiOp(t *testing.T) {
	sock, _ := startDaemon(t, time.Minute)
	reg := roundTrip(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: "/r", SessionID: "s1", Source: "hook"})
	r := roundTrip(t, sock, protocol.Request{Op: protocol.OpWhoami, Scope: "/r", SessionID: "s1"})
	if !r.OK || r.Name != reg.Name || r.AgentID == "" || r.Source != "hook" || r.Parent != "" {
		t.Fatalf("whoami: %+v", r)
	}
	r = roundTrip(t, sock, protocol.Request{Op: protocol.OpWhoami, Scope: "/r", SessionID: "unknown"})
	if r.OK || r.Error == "" {
		t.Fatalf("whoami for unknown session must error: %+v", r)
	}
}

func TestRegisterOnlyIfNoHook(t *testing.T) {
	sock, _ := startDaemon(t, time.Minute)
	// Empty scope: the hookless-harness path mints as usual.
	r := roundTrip(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: "/r", SessionID: "mcp-1", Source: "mcp:codex", OnlyIfNoHook: true})
	if !r.OK || r.Name == "" {
		t.Fatalf("hookless mint: %+v", r)
	}
	// A live hook row makes a NEW guarded register fail closed.
	roundTrip(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: "/r", SessionID: "claude-uuid", Source: "hook"})
	r = roundTrip(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: "/r", SessionID: "mcp-2", Source: "mcp:codex", OnlyIfNoHook: true})
	if r.OK || !strings.Contains(r.Error, "cannot determine your identity") {
		t.Fatalf("guarded register must fail closed: %+v", r)
	}
}

// Bind-as-lock: a second daemon finding a live listener on the socket must
// report ErrAlreadyServing so its process can exit 0 quietly.
func TestListenerExitsWhenPeerServes(t *testing.T) {
	sock := filepath.Join(socktest.Dir(t), "d.sock")
	t.Setenv("AC_SOCKET", sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, _, err := Listener(); !errors.Is(err, ErrAlreadyServing) {
		t.Fatalf("want ErrAlreadyServing with a live peer, got %v", err)
	}
}

// The file lock is the serializer: while a peer holds sock+".lock", Listener
// must defer with ErrAlreadyServing even though nothing is dialable yet (the
// peer may be mid-bind) - it must never remove the file or bind itself.
func TestListenerDefersToLockHolder(t *testing.T) {
	sock := filepath.Join(socktest.Dir(t), "d.sock")
	t.Setenv("AC_SOCKET", sock)
	peer, err := os.OpenFile(sock+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if held, err := tryLock(peer); err != nil || !held {
		t.Fatalf("peer lock: held=%v err=%v", held, err)
	}
	if _, _, err := Listener(); !errors.Is(err, ErrAlreadyServing) {
		t.Fatalf("want ErrAlreadyServing while a peer holds the lock, got %v", err)
	}
}

// A dead socket file (unclean shutdown) is not a lock: Listener removes it
// and binds.
func TestListenerReplacesStaleSocket(t *testing.T) {
	sock := filepath.Join(socktest.Dir(t), "d.sock")
	t.Setenv("AC_SOCKET", sock)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close() // leaves the socket file behind, nobody listening
	l, activated, err := Listener()
	if err != nil || activated {
		t.Fatalf("want a fresh listener over the stale socket, got activated=%v err=%v", activated, err)
	}
	l.Close()
	// Listener pinned sock+".lock" in sockLock for the process lifetime (by
	// design for real daemons). Release it here so windows can delete the
	// temp dir, and reset for other tests in this process.
	sockLock.Close()
	sockLock = nil
}

func TestIdleExit(t *testing.T) {
	sock, done := startDaemon(t, 300*time.Millisecond)
	roundTrip(t, sock, protocol.Request{Op: protocol.OpAgents, Scope: "/r"})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("idle exit returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not idle-exit")
	}
}

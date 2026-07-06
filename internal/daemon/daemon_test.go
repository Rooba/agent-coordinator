package daemon

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-coordinator/go/internal/protocol"
	"github.com/agent-coordinator/go/internal/store"
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
	dir := t.TempDir()
	sock := filepath.Join(dir, "d.sock")
	st, err := store.Open(filepath.Join(dir, "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Serve(l, st, idle) }()
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

package daemon

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
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

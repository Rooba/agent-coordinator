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

// Bind-as-lock: a second daemon finding a live listener on the socket must
// report ErrAlreadyServing so its process can exit 0 quietly.
func TestListenerExitsWhenPeerServes(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
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
	sock := filepath.Join(t.TempDir(), "d.sock")
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
	sock := filepath.Join(t.TempDir(), "d.sock")
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

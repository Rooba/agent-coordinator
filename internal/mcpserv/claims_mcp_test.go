package mcpserv

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/daemon"
	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/scope"
	"github.com/Rooba/agent-coordinator/internal/socktest"
	"github.com/Rooba/agent-coordinator/internal/store"
)

// startDaemon runs a real daemon+store and returns the socket plus a seeding
// round-tripper, for MCP tests that need actual claim/journal semantics.
func startDaemon(t *testing.T) (string, func(protocol.Request) protocol.Response) {
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
	exited := make(chan struct{})
	go func() { daemon.Serve(l, st, time.Minute); close(exited) }()
	t.Cleanup(func() { l.Close(); <-exited })
	sc := scope.Resolve(cwd)
	seed := func(req protocol.Request) protocol.Response {
		t.Helper()
		req.Scope = sc
		resp, err := roundTrip(sock, req)
		if err != nil || !resp.OK {
			t.Fatalf("seed %+v: %+v err=%v", req, resp, err)
		}
		return resp
	}
	return sock, seed
}

func TestClaimToolForwardsPathAndNote(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"claim","arguments":{"from":"amber-fox","path":"internal/hub.go","note":"rewiring"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"release","arguments":{"from":"amber-fox","path":"internal/hub.go"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"message_history","arguments":{"from":"amber-fox","peer":"brisk-owl","limit":5}}}`)
	if len(*got) != 3 {
		t.Fatalf("want claim+release+history requests: %+v", *got)
	}
	if r := (*got)[0]; r.Op != protocol.OpClaim || r.From != "amber-fox" || r.Path != "internal/hub.go" || r.Note != "rewiring" {
		t.Fatalf("claim request: %+v", r)
	}
	if r := (*got)[1]; r.Op != protocol.OpRelease || r.Path != "internal/hub.go" {
		t.Fatalf("release request: %+v", r)
	}
	if r := (*got)[2]; r.Op != protocol.OpHistory || r.Peer != "brisk-owl" || r.Limit != 5 {
		t.Fatalf("history request: %+v", r)
	}
	if !strings.Contains(out[0], "claimed internal/hub.go") || !strings.Contains(out[1], "released internal/hub.go") {
		t.Fatalf("claim/release results: %v", out[:2])
	}
}

// A claim on a path held by a live peer must surface the holder and note
// verbatim as a tool error.
func TestClaimConflictRendersHolderAndNote(t *testing.T) {
	sock, seed := startDaemon(t)
	a := seed(protocol.Request{Op: protocol.OpRegister, SessionID: "sa", Source: "hook"})
	b := seed(protocol.Request{Op: protocol.OpRegister, SessionID: "sb", Source: "hook"})
	seed(protocol.Request{Op: protocol.OpClaim, From: a.Name, Path: "/hub.go", Note: "rewiring dispatch"})
	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"claim","arguments":{"from":"`+b.Name+`","path":"/hub.go","note":"mine"}}}`)
	if !strings.Contains(out[0], `"isError":true`) ||
		!strings.Contains(out[0], "held by "+a.Name) || !strings.Contains(out[0], "rewiring dispatch") {
		t.Fatalf("conflict must surface holder and note: %s", out[0])
	}
	// list_claims still shows the original holder.
	out = rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_claims","arguments":{}}}`)
	if !strings.Contains(out[0], `\"holder\": \"`+a.Name+`\"`) || !strings.Contains(out[0], `\"path\": \"/hub.go\"`) {
		t.Fatalf("list_claims must keep the original holder: %s", out[0])
	}
}

// message_history is a pure audit: reading it leaves unread mail unread.
func TestMessageHistoryIsNonDestructive(t *testing.T) {
	sock, seed := startDaemon(t)
	a := seed(protocol.Request{Op: protocol.OpRegister, SessionID: "sa", Source: "hook"})
	b := seed(protocol.Request{Op: protocol.OpRegister, SessionID: "sb", Source: "hook"})
	seed(protocol.Request{Op: protocol.OpSend, From: a.Name, To: b.Name, Body: "audit me"})
	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"message_history","arguments":{"from":"`+b.Name+`"}}}`)
	for _, want := range []string{`\"body_preview\": \"audit me\"`, `\"read_at\": 0`, `\"from\": \"` + a.Name + `\"`} {
		if !strings.Contains(out[0], want) {
			t.Fatalf("history output missing %s: %s", want, out[0])
		}
	}
	if peek := seed(protocol.Request{Op: protocol.OpPeek, From: b.Name}); peek.Unread != 1 {
		t.Fatalf("history read must not consume unread mail: %+v", peek)
	}
}

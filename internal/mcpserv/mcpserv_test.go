package mcpserv

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/daemon"
	"github.com/Rooba/agent-coordinator/internal/hookcli"
	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/scope"
	"github.com/Rooba/agent-coordinator/internal/socktest"
	"github.com/Rooba/agent-coordinator/internal/store"
)

// The real bind dir must never leak into tests: disabled by default,
// individual tests opt in via stubBind.
func TestMain(m *testing.M) {
	bindDirFn = func() (string, error) { return "", errors.New("bind disabled in tests") }
	ancestryFn = func() []int { return nil }
	hookcli.PidAlive = func(int) bool { return true } // test pids are fictional
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

// cwd is the working directory Serve is started with in these tests.
// Assertions on the forwarded scope must compare against scope.Resolve(cwd),
// never the literal: Resolve canonicalizes per-OS (on windows this becomes a
// volume-qualified backslash path).
const cwd = "/some/repo"

func fakeDaemon(t *testing.T, resp protocol.Response) (string, *[]protocol.Request) {
	t.Helper()
	sock := filepath.Join(socktest.Dir(t), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var got []protocol.Request
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var r protocol.Request
			json.Unmarshal(line, &r)
			got = append(got, r)
			b, _ := json.Marshal(resp)
			c.Write(append(b, '\n'))
			c.Close()
		}
	}()
	return sock, &got
}

func rpc(t *testing.T, sock string, lines ...string) []string {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out bytes.Buffer
	if err := Serve(in, &out, sock, cwd); err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(out.String()), "\n")
}

func TestInitializeAndList(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{OK: true})
	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"codex","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if len(out) != 2 {
		t.Fatalf("want 2 responses (notification is silent), got %d: %v", len(out), out)
	}
	if !strings.Contains(out[0], `"protocolVersion":"2025-06-18"`) ||
		!strings.Contains(out[0], "agent-coordinator") || !strings.Contains(out[0], `"instructions"`) {
		t.Fatalf("initialize: %s", out[0])
	}
	for _, tool := range []string{"register_agent", "whoami", "status_board", "list_agents", "send_message", "read_messages", "peek_messages", "broadcast", "claim", "release", "list_claims", "message_history"} {
		if !strings.Contains(out[1], `"`+tool+`"`) {
			t.Fatalf("tools/list missing %s: %s", tool, out[1])
		}
	}
}

func TestProtocolVersionNegotiation(t *testing.T) {
	for requested := range supportedProtocolVersions {
		if got := negotiateProtocolVersion(requested); got != requested {
			t.Fatalf("supported version %q negotiated as %q", requested, got)
		}
	}
	if got := negotiateProtocolVersion("2099-01-01"); got != latestProtocolVersion {
		t.Fatalf("unknown version negotiated as %q, want %q", got, latestProtocolVersion)
	}
}

func TestPing(t *testing.T) {
	out := rpc(t, "/unused", `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	if len(out) != 1 || !strings.Contains(out[0], `"result":{}`) {
		t.Fatalf("ping response: %v", out)
	}
}

// Regression: a single line over the old 1MiB Scanner cap must be served,
// not kill Serve.
func TestOversizedLineIsServed(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{OK: true})
	pad := strings.Repeat("x", 1<<20+4096)
	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"`+pad+`"}}`)
	if len(out) != 1 || !strings.Contains(out[0], "status_board") {
		t.Fatalf("oversized request not served: %d responses", len(out))
	}
}

// Regression: a garbage line must yield a JSON-RPC -32700 parse error and
// Serve must keep serving subsequent requests.
func TestGarbageLineYieldsParseError(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{OK: true})
	out := rpc(t, sock,
		"this is not json",
		`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)
	if len(out) != 2 {
		t.Fatalf("want parse-error line + response, got %v", out)
	}
	if !strings.Contains(out[0], "-32700") || !strings.Contains(out[0], `"id":null`) || !strings.Contains(out[0], "parse error") {
		t.Fatalf("parse error line: %s", out[0])
	}
	if !strings.Contains(out[1], `"id":7`) || !strings.Contains(out[1], "status_board") {
		t.Fatalf("server must keep serving after garbage: %s", out[1])
	}
}

func TestSendMessageToolCall(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_message","arguments":{"from":"amber-fox","to":"brisk-owl","body":"ping"}}}`)
	if len(*got) != 1 || (*got)[0].Op != protocol.OpSend || (*got)[0].To != "brisk-owl" || (*got)[0].Scope != scope.Resolve(cwd) {
		t.Fatalf("daemon saw %+v", *got)
	}
	if !strings.Contains(out[0], `"content"`) {
		t.Fatalf("tool result: %s", out[0])
	}
}

// Hookless-harness path: no bind file and (per the daemon's guarded register)
// no live hook rows, so a fresh mcp- identity is minted and later deregistered.
func TestMessagingWithoutFromUsesMCPSessionIdentity(t *testing.T) {
	stubBind(t, []int{999}) // empty bind dir: nothing to adopt
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "amber-fox"})
	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"codex","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"send_message","arguments":{"to":"brisk-owl","body":"ping"}}}`)
	if len(out) != 2 || !strings.Contains(out[1], `"content"`) {
		t.Fatalf("tool result: %v", out)
	}
	if len(*got) != 3 {
		t.Fatalf("want register, send, deregister; got %+v", *got)
	}
	if r := (*got)[0]; r.Op != protocol.OpRegister || !strings.HasPrefix(r.SessionID, "mcp-") ||
		r.Source != "mcp:codex" || !r.OnlyIfNoHook {
		t.Fatalf("register request: %+v", r)
	}
	if (*got)[1].Op != protocol.OpSend || (*got)[1].From != "amber-fox" || (*got)[1].To != "brisk-owl" {
		t.Fatalf("send request: %+v", (*got)[1])
	}
	if (*got)[2].Op != protocol.OpDeregister || (*got)[2].SessionID != (*got)[0].SessionID {
		t.Fatalf("deregister request: %+v", (*got)[2])
	}
}

// A bind file whose pid chain intersects our ancestry binds this connection to
// the hook identity: bare read_messages reads the HOOK inbox, and EOF must not
// deregister the hook row (the session outlives the MCP process).
func TestBindsToHookRowAndReadsHookInbox(t *testing.T) {
	dir := stubBind(t, []int{999, 500})
	hookcli.WriteBind(dir, hookcli.Bind{SessionID: "claude-uuid-1", Scope: scope.Resolve(cwd),
		Name: "quick-wolf", Pids: []int{500, 400}, TS: time.Now().Unix()})
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "quick-wolf"})
	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_messages","arguments":{}}}`)
	if !strings.Contains(out[0], `"content"`) {
		t.Fatalf("tool result: %v", out)
	}
	if len(*got) != 2 {
		t.Fatalf("want register + read only (no deregister of a hook row): %+v", *got)
	}
	if r := (*got)[0]; r.Op != protocol.OpRegister || r.SessionID != "claude-uuid-1" || r.OnlyIfNoHook {
		t.Fatalf("register request must adopt the bound session: %+v", r)
	}
	if r := (*got)[1]; r.Op != protocol.OpRead || r.From != "quick-wolf" || r.SessionID != "claude-uuid-1" {
		t.Fatalf("read request must carry the bound session as heartbeat: %+v", r)
	}
}

// An early call that minted an mcp- identity must not pin it for the process
// lifetime: once the hook's bind file appears, the next call retires the
// minted row and adopts the hook identity.
func TestLateBindAdoptionSwitchesIdentity(t *testing.T) {
	dir := stubBind(t, []int{999, 500})
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "minted-name"})

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(inR, outW, sock, cwd) }()
	sc := bufio.NewScanner(outR)
	call := func(id int) {
		t.Helper()
		fmt.Fprintf(inW, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"send_message","arguments":{"to":"peer","body":"hi"}}}`+"\n", id)
		if !sc.Scan() {
			t.Fatalf("no response for call %d", id)
		}
	}
	call(1) // empty bind dir: mints an mcp- identity
	hookcli.WriteBind(dir, hookcli.Bind{SessionID: "claude-uuid-9", Scope: scope.Resolve(cwd),
		Name: "quick-wolf", Pids: []int{500}, TS: time.Now().Unix()})
	call(2) // bind file appeared: adopt the hook identity
	inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	reqs := *got
	if len(reqs) != 5 {
		t.Fatalf("want mint+send then deregister+adopt+send: %+v", reqs)
	}
	if reqs[0].Op != protocol.OpRegister || !strings.HasPrefix(reqs[0].SessionID, "mcp-") || !reqs[0].OnlyIfNoHook {
		t.Fatalf("first call must mint: %+v", reqs[0])
	}
	if reqs[1].Op != protocol.OpSend || reqs[1].SessionID != reqs[0].SessionID {
		t.Fatalf("first send: %+v", reqs[1])
	}
	if reqs[2].Op != protocol.OpDeregister || reqs[2].SessionID != reqs[0].SessionID {
		t.Fatalf("minted row must be retired on adoption: %+v", reqs[2])
	}
	if reqs[3].Op != protocol.OpRegister || reqs[3].SessionID != "claude-uuid-9" || reqs[3].OnlyIfNoHook {
		t.Fatalf("adoption register: %+v", reqs[3])
	}
	if reqs[4].Op != protocol.OpSend || reqs[4].SessionID != "claude-uuid-9" {
		t.Fatalf("second send must ride the adopted session: %+v", reqs[4])
	}
}

// No bind match and live hook rows in scope: the daemon refuses the guarded
// register, and bare messaging calls surface the identity error verbatim.
func TestFailClosedWhenIdentityUnknown(t *testing.T) {
	stubBind(t, []int{999})
	const identityErr = "cannot determine your identity: pass from=<your name> or call register_agent"
	sock, got := fakeDaemon(t, protocol.Response{Error: identityErr})
	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_message","arguments":{"to":"brisk-owl","body":"ping"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_messages","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)
	for i, line := range out {
		if !strings.Contains(line, `"isError":true`) || !strings.Contains(line, "cannot determine your identity") {
			t.Fatalf("call %d must fail with the identity error: %s", i, line)
		}
	}
	for _, r := range *got {
		if r.Op != protocol.OpRegister || !r.OnlyIfNoHook {
			t.Fatalf("only guarded registers expected: %+v", r)
		}
	}
	// Explicit from still works without any registration.
	sock2, got2 := fakeDaemon(t, protocol.Response{OK: true})
	rpc(t, sock2,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_message","arguments":{"from":"quick-wolf","to":"brisk-owl","body":"ping"}}}`)
	if len(*got2) != 1 || (*got2)[0].Op != protocol.OpSend {
		t.Fatalf("explicit from must bypass binding: %+v", *got2)
	}
}

func TestWhoamiReturnsBoundIdentity(t *testing.T) {
	dir := stubBind(t, []int{999, 500})
	hookcli.WriteBind(dir, hookcli.Bind{SessionID: "claude-uuid-1", Scope: scope.Resolve(cwd),
		Name: "quick-wolf", Pids: []int{500}, TS: time.Now().Unix()})
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "quick-wolf", AgentID: "abc123", Source: "hook"})
	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)
	// The identity JSON is nested in the RPC envelope, so its quotes arrive escaped.
	for _, want := range []string{`\"name\": \"quick-wolf\"`, `\"agent_id\": \"abc123\"`, `\"source\": \"hook\"`} {
		if !strings.Contains(out[0], want) {
			t.Fatalf("whoami output missing %s: %s", want, out[0])
		}
	}
	if strings.Contains(out[0], "parent") {
		t.Fatalf("parent must be omitted when empty: %s", out[0])
	}
	if len(*got) != 2 || (*got)[1].Op != protocol.OpWhoami || (*got)[1].SessionID != "claude-uuid-1" {
		t.Fatalf("want register + whoami for the bound session: %+v", *got)
	}
}

// End-to-end retro case 1 at the MCP layer: with a REAL daemon and store, a
// subagent that passes from=<child name> reads only the child inbox and the
// parent keeps its unread mail.
func TestChildReadDoesNotDrainParentInbox(t *testing.T) {
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
	parent := seed(protocol.Request{Op: protocol.OpRegister, SessionID: "p1", Source: "hook"})
	peer := seed(protocol.Request{Op: protocol.OpRegister, SessionID: "peer1", Source: "hook"})
	child := seed(protocol.Request{Op: protocol.OpRegister, SessionID: "p1",
		Source: "hook-subagent", AgentID: "a1", AgentType: "Explore"})
	seed(protocol.Request{Op: protocol.OpSend, From: peer.Name, To: parent.Name, Body: "for-parent-1"})
	seed(protocol.Request{Op: protocol.OpSend, From: peer.Name, To: parent.Name, Body: "for-parent-2"})
	seed(protocol.Request{Op: protocol.OpSend, From: peer.Name, To: child.Name, Body: "for-child"})

	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_messages","arguments":{"from":"`+child.Name+`"}}}`)
	if !strings.Contains(out[0], "for-child") || strings.Contains(out[0], "for-parent") {
		t.Fatalf("child read must return only child mail: %s", out[0])
	}
	peek := seed(protocol.Request{Op: protocol.OpPeek, From: parent.Name})
	if peek.Unread != 2 {
		t.Fatalf("parent unread after child read: want 2, got %d", peek.Unread)
	}
}

// peek_messages is a pure preview: it reports count/senders/ids and the
// subsequent read_messages still returns the mail.
func TestPeekMessagesIsNonDestructive(t *testing.T) {
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
	a := seed(protocol.Request{Op: protocol.OpRegister, SessionID: "sa", Source: "hook"})
	b := seed(protocol.Request{Op: protocol.OpRegister, SessionID: "sb", Source: "hook"})
	seed(protocol.Request{Op: protocol.OpSend, From: a.Name, To: b.Name, Body: "first"})
	seed(protocol.Request{Op: protocol.OpSend, From: a.Name, To: b.Name, Body: "second"})

	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"peek_messages","arguments":{"from":"`+b.Name+`"}}}`)
	for _, want := range []string{`\"unread\": 2`, `\"` + a.Name + `\"`, `\"high_water\"`, `\"ids\"`} {
		if !strings.Contains(out[0], want) {
			t.Fatalf("peek output missing %s: %s", want, out[0])
		}
	}
	out = rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_messages","arguments":{"from":"`+b.Name+`"}}}`)
	if !strings.Contains(out[0], "first") || !strings.Contains(out[0], "second") {
		t.Fatalf("read after peek must still return the mail: %s", out[0])
	}
}

// Board rows always render agent_id next to name in the tool output.
func TestStatusBoardOutputIncludesAgentID(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Agents: []protocol.AgentInfo{
		{AgentID: "abc123", Name: "amber-fox", Status: "active"},
		{AgentID: "def456", Name: "brisk-owl", Status: "idle"},
	}})
	for _, tool := range []string{"status_board", "list_agents"} {
		out := rpc(t, sock,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":{}}}`)
		for _, want := range []string{`\"agent_id\": \"abc123\"`, `\"agent_id\": \"def456\"`} {
			if !strings.Contains(out[0], want) {
				t.Fatalf("%s output missing %s: %s", tool, want, out[0])
			}
		}
	}
	if (*got)[0].IncludeGone {
		t.Fatalf("default board request must not include gone: %+v", (*got)[0])
	}
}

func TestStatusBoardForwardsIncludeGone(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status_board","arguments":{"include_gone":true}}}`)
	if len(*got) != 1 || (*got)[0].Op != protocol.OpBoard || !(*got)[0].IncludeGone {
		t.Fatalf("include_gone not forwarded: %+v", *got)
	}
}

func TestDaemonErrorSurfacesAsToolError(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{Error: "no agent \"nobody\" in this workspace"})
	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_message","arguments":{"from":"a","to":"nobody","body":"x"}}}`)
	if !strings.Contains(out[0], `"isError":true`) || !strings.Contains(out[0], "nobody") {
		t.Fatalf("want isError with reason: %s", out[0])
	}
}

package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/paths"
	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/socktest"
	"github.com/Rooba/agent-coordinator/internal/store"
)

// serveBoth starts one daemon over both listeners in a fresh temp dir. The
// caller sets AC_RELAY_LISTEN (and any relay env) first; the returned channel
// carries the relay address the OS picked, and stays empty when no relay came
// up. Restoring relayReady is registered before the daemon's own cleanup, so
// it runs after Serve has exited.
func serveBoth(t *testing.T) (sock string, st *store.Store, ready <-chan string) {
	t.Helper()
	dir := socktest.Dir(t)
	t.Setenv("AC_DB", filepath.Join(dir, "d.db"))
	t.Setenv("AC_TOKEN", "")
	addrC := make(chan string, 1)
	prev := relayReady
	relayReady = func(a string) { addrC <- a }
	t.Cleanup(func() { relayReady = prev })

	sock = filepath.Join(dir, "d.sock")
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
	go func() { Serve(l, st, time.Minute); close(exited) }()
	t.Cleanup(func() {
		l.Close()
		<-exited
	})
	return sock, st, addrC
}

// startRelayDaemon runs a daemon whose relay listens on 127.0.0.1:0 - never
// 7400, so a test cannot collide with a real broker. It returns the socket
// path, the relay address and the shared token.
func startRelayDaemon(t *testing.T) (sock, tcpAddr, token string) {
	t.Helper()
	sock, tcpAddr, token, _ = relayDaemon(t)
	return sock, tcpAddr, token
}

// relayDaemon is startRelayDaemon plus the store handle, for the tests that
// have to seed the eyes ledger the way a request_eyes call will.
func relayDaemon(t *testing.T) (sock, tcpAddr, token string, st *store.Store) {
	t.Helper()
	t.Setenv("AC_RELAY_INSECURE", "")
	t.Setenv("AC_RELAY_LISTEN", "127.0.0.1:0")
	sock, st, ready := serveBoth(t)
	select {
	case tcpAddr = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("relay listener never came up")
	}
	token, err := paths.RelayToken()
	if err != nil {
		t.Fatal(err)
	}
	return sock, tcpAddr, token, st
}

// tcpRoundTrip sends one request over the relay listener.
func tcpRoundTrip(t *testing.T, addr string, req protocol.Request) protocol.Response {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
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

// captureStderr redirects the daemon's log lines to a file for the rest of
// the test and returns a reader for what was written.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(socktest.Dir(t), "err")
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = prev
		f.Close()
	})
	return func() string {
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
}

// registerLauncher is the host broker's first call: the shared token alone
// buys a launcher row in its reserved host: scope.
func registerLauncher(t *testing.T, addr, token, session, scope string) protocol.Response {
	t.Helper()
	r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: scope,
		SessionID: session, Kind: protocol.KindLauncher, Platform: "windows",
		Capabilities: []string{"browser.chrome", "provider.claude"}, Token: token})
	if !r.OK || r.Name == "" || len(r.SessionSecret) != 64 {
		t.Fatalf("launcher register: %+v", r)
	}
	return r
}

// A relay register hands back the session secret exactly once; re-registering
// means presenting it, and without it the row is not up for grabs.
func TestRelayRegisterReturnsSecretOnce(t *testing.T) {
	_, addr, tok := startRelayDaemon(t)
	first := registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	again := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "host:BOX",
		SessionID: "broker-1", Kind: protocol.KindLauncher, Token: tok, SessionSecret: first.SessionSecret})
	if !again.OK || again.Name != first.Name || again.SessionSecret != "" {
		t.Fatalf("re-register must be idempotent: %+v", again)
	}
	stolen := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "host:BOX",
		SessionID: "broker-1", Kind: protocol.KindLauncher, Token: tok})
	if stolen.OK || stolen.Error != "unauthorized" {
		t.Fatalf("the token alone must not re-claim a live relay session: %+v", stolen)
	}
}

// The shared token buys a broker row in a reserved host: scope and nothing
// else - it can never mint an identity inside a workspace.
func TestRelayLauncherMustRegisterInAHostScope(t *testing.T) {
	_, addr, tok := startRelayDaemon(t)
	r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "/r",
		SessionID: "broker-1", Kind: protocol.KindLauncher, Token: tok})
	if r.OK || r.Error != "foreign session" {
		t.Fatalf("launcher outside host: scope: %+v", r)
	}
}

// The row, not the caller, says who a relay client is: a request that names
// no scope and no sender is still answered for the registered identity.
func TestRelayGateBindsIdentityFromTheRow(t *testing.T) {
	_, addr, tok := startRelayDaemon(t)
	reg := registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	board := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpBoard,
		SessionID: "broker-1", Token: tok, SessionSecret: reg.SessionSecret})
	if !board.OK || len(board.Agents) != 1 {
		t.Fatalf("board for the bound scope: %+v", board)
	}
	// Only the launcher's own scope holds that row, so answering it at all is
	// the proof that the gate supplied the scope.
	if a := board.Agents[0]; a.Name != reg.Name || a.Kind != protocol.KindLauncher {
		t.Fatalf("board row: %+v", a)
	}
}

// A launcher mints the eyes child of the task it was handed: the child
// session names the task, the launcher's own secret is the proof, and the
// child comes back with a secret of its own.
func TestRelayEyesChildNeedsItsLaunchersAuthority(t *testing.T) {
	sock, addr, tok, st := relayDaemon(t)
	broker := registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	if r := roundTrip(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: "/r",
		SessionID: "s-a", Source: "hook"}); !r.OK {
		t.Fatalf("unix register: %+v", r)
	}
	who := roundTrip(t, sock, protocol.Request{Op: protocol.OpWhoami, Scope: "/r", SessionID: "s-a"})
	if !who.OK {
		t.Fatalf("whoami: %+v", who)
	}
	task, _, err := st.AssignEyesTask(store.EyesRequest{
		Requester: protocol.AgentRef{Name: who.Name, AgentID: who.AgentID, Scope: "/r"},
		Runtime:   "claude", Brief: "does the login page render"})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	child := "eyes-" + task.TaskID

	// The shared token alone never creates an eyes child.
	bare := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "/r",
		SessionID: child, Kind: protocol.KindEyes, Token: tok})
	if bare.OK || bare.Error != "unauthorized" {
		t.Fatalf("eyes register without a launcher: %+v", bare)
	}
	// Nor does a session id that names no task.
	unnamed := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "/r",
		SessionID: "not-a-task", Kind: protocol.KindEyes, Token: tok,
		AuthSessionID: "broker-1", SessionSecret: broker.SessionSecret})
	if unnamed.OK || unnamed.Error != "unauthorized" {
		t.Fatalf("eyes session must be eyes-<task_id>: %+v", unnamed)
	}
	// The child belongs to the requester's workspace, not one the caller picks.
	elsewhere := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "/other",
		SessionID: child, Kind: protocol.KindEyes, Token: tok,
		AuthSessionID: "broker-1", SessionSecret: broker.SessionSecret})
	if elsewhere.OK || elsewhere.Error != "foreign session" {
		t.Fatalf("eyes child in a foreign scope: %+v", elsewhere)
	}
	// Another broker holds a valid secret, but not this task.
	other := registerLauncher(t, addr, tok, "broker-2", "host:BOX2")
	poached := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "/r",
		SessionID: child, Kind: protocol.KindEyes, Token: tok,
		AuthSessionID: "broker-2", SessionSecret: other.SessionSecret})
	if poached.OK || poached.Error != "not your task" {
		t.Fatalf("only the assigned launcher mints the child: %+v", poached)
	}

	minted := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "/r",
		SessionID: child, Kind: protocol.KindEyes, Token: tok,
		AuthSessionID: "broker-1", SessionSecret: broker.SessionSecret})
	if !minted.OK || minted.Name == "" || len(minted.SessionSecret) != 64 {
		t.Fatalf("minted child: %+v", minted)
	}
	if minted.SessionSecret == broker.SessionSecret {
		t.Fatalf("the child must get a secret of its own")
	}
	// The child's own secret is what its later requests carry, and the
	// launcher's is not a licence to act AS the child.
	if r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpPeek, SessionID: child,
		Token: tok, SessionSecret: minted.SessionSecret}); !r.OK {
		t.Fatalf("child peek: %+v", r)
	}
	if r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpPeek, SessionID: child,
		Token: tok, SessionSecret: broker.SessionSecret}); r.OK || r.Error != "unauthorized" {
		t.Fatalf("the launcher secret must not drive the child: %+v", r)
	}
	// And the child lands on the requester's board as eyes.
	board := roundTrip(t, sock, protocol.Request{Op: protocol.OpBoard, Scope: "/r"})
	kinds := map[string]string{}
	for _, ag := range board.Agents {
		kinds[ag.Name] = ag.Kind
	}
	if kinds[minted.Name] != protocol.KindEyes {
		t.Fatalf("child on the requester board: %+v", board.Agents)
	}
}

// A wrong token is refused before the store is touched at all.
func TestRelayRejectsBadToken(t *testing.T) {
	_, addr, tok := startRelayDaemon(t)
	bad := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "host:BOX",
		SessionID: "broker-1", Kind: protocol.KindLauncher, Token: "not-the-token"})
	if bad.OK || bad.Error != "unauthorized" {
		t.Fatalf("bad token: %+v", bad)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(os.Getenv("AC_DB")), "relay.token")); err != nil {
		t.Fatalf("the relay listen must have published a token file: %v", err)
	}
	registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	for _, secret := range []string{"", strings.Repeat("0", 64)} {
		r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpPeek, SessionID: "broker-1",
			Token: tok, SessionSecret: secret})
		if r.OK || r.Error != "unauthorized" {
			t.Fatalf("secret %q: %+v", secret, r)
		}
	}
}

// A relay request that names no session cannot be bound to a row, so it is
// refused before it can reach the unix socket's "no session" trust path.
func TestRelayRequiresASessionID(t *testing.T) {
	_, addr, tok := startRelayDaemon(t)
	for _, req := range []protocol.Request{
		{Op: protocol.OpBoard, Scope: "/r", Token: tok},
		{Op: protocol.OpSend, Scope: "/r", From: "someone", To: "someone", Body: "hi", Token: tok},
		{Op: protocol.OpRegister, Scope: "host:BOX", Kind: protocol.KindLauncher, Token: tok},
	} {
		r := tcpRoundTrip(t, addr, req)
		if r.OK || r.Error != "unauthorized" {
			t.Fatalf("%s without a session: %+v", req.Op, r)
		}
	}
}

// The allowlist is the boundary: the claims ledger, the journal, whoami and
// the eyes ops never cross it, and a register that names no role is not a
// relay register at all.
func TestRelayOpAllowlist(t *testing.T) {
	_, addr, tok := startRelayDaemon(t)
	reg := registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	for _, op := range []string{protocol.OpClaim, protocol.OpRelease, protocol.OpClaims,
		protocol.OpHistory, protocol.OpWhoami, protocol.OpRequestEyes, protocol.OpCancelEyes} {
		r := tcpRoundTrip(t, addr, protocol.Request{Op: op, SessionID: "broker-1",
			Token: tok, SessionSecret: reg.SessionSecret, Path: "/x", Brief: "b"})
		if r.OK || r.Error != "op not allowed on relay" {
			t.Fatalf("%s: %+v", op, r)
		}
	}
	for _, kind := range []string{"", "hook", "subagent"} {
		r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "/r",
			SessionID: "sneaky", Kind: kind, Token: tok})
		if r.OK || r.Error != "op not allowed on relay" {
			t.Fatalf("register kind %q: %+v", kind, r)
		}
	}
}

// Hook and MCP sessions are untouchable from the relay, whatever credential
// the caller presents.
func TestRelayCannotActAsAUnixSession(t *testing.T) {
	sock, addr, tok := startRelayDaemon(t)
	if r := roundTrip(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: "/r",
		SessionID: "s-a", Source: "hook"}); !r.OK {
		t.Fatalf("unix register: %+v", r)
	}
	r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRead, Scope: "/r",
		SessionID: "s-a", Token: tok, SessionSecret: strings.Repeat("0", 64)})
	if r.OK || r.Error != "foreign session" {
		t.Fatalf("relay reading a hook session: %+v", r)
	}
	unknown := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRead, Scope: "/r",
		SessionID: "never-registered", Token: tok})
	if unknown.OK || unknown.Error != "unauthorized" {
		t.Fatalf("an unknown session must not say so: %+v", unknown)
	}
}

// The unix socket keeps its own trust boundary: no token, no secret, no gate,
// and no credential minted.
func TestUnixRequestsNeedNoToken(t *testing.T) {
	sock, _, _ := startRelayDaemon(t)
	r := roundTrip(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: "/r", SessionID: "s-a", Source: "hook"})
	if !r.OK || r.Name == "" {
		t.Fatalf("unix register must not need a token: %+v", r)
	}
	if r.SessionSecret != "" {
		t.Fatalf("a unix register must mint no secret: %+v", r)
	}
	if w := roundTrip(t, sock, protocol.Request{Op: protocol.OpWhoami, Scope: "/r", SessionID: "s-a"}); !w.OK {
		t.Fatalf("whoami stays a unix op: %+v", w)
	}
}

// Identity is rewritten from the authenticated row, and a task id is the
// daemon's to stamp: a relay client cannot put one on ordinary mail.
func TestRelayGateRewritesTheRequest(t *testing.T) {
	dir := socktest.Dir(t)
	st, err := store.Open(filepath.Join(dir, "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg, err := st.RegisterRelay(store.RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay"})
	if err != nil {
		t.Fatal(err)
	}
	gate := &relayGate{st: st, token: "shared-token"}
	req := protocol.Request{Op: protocol.OpSend, Scope: "/somewhere-else", From: "impostor",
		To: "someone", Body: "hi", TaskID: "task-000000000000",
		SessionID: "broker-1", Token: "shared-token", SessionSecret: reg.Secret}
	if resp, final := gate.check(&req); final {
		t.Fatalf("a proven relay send must dispatch: %+v", resp)
	}
	if req.Scope != "host:BOX" || req.From != reg.Name || req.TaskID != "" {
		t.Fatalf("gate must answer as the row: %+v", req)
	}
	if resp, final := (*relayGate)(nil).check(&req); final {
		t.Fatalf("the unix listener has no gate: %+v", resp)
	}
}

// One store failure, one wire string. "no session" collapses into
// "unauthorized" so a caller never learns whether the id or the secret was
// wrong; the lifecycle errors keep their own text.
func TestRelayErrorStrings(t *testing.T) {
	for err, want := range map[error]string{
		store.ErrNoSession:      "unauthorized",
		store.ErrRelayAuth:      "unauthorized",
		store.ErrForeignSession: "foreign session",
		store.ErrEyesBusy:       "eyes busy",
		store.ErrNoLauncher:     "no host launcher",
		store.ErrUnknownTask:    "unknown task",
		store.ErrNotYourTask:    "not your task",
		store.ErrTaskNotLive:    store.ErrTaskNotLive.Error(),
		store.ErrBadTransition:  store.ErrBadTransition.Error(),
	} {
		if got := relayError(err); got != want {
			t.Errorf("relayError(%v) = %q, want %q", err, got, want)
		}
	}
	wrapped := fmt.Errorf("%w: done -> accepted", store.ErrBadTransition)
	if got := relayError(wrapped); got != wrapped.Error() {
		t.Errorf("a wrapped transition must keep its detail: %q", got)
	}
}

// A relay address that is not loopback is refused by the address parser, and
// that must cost the daemon its relay - never its unix socket.
func TestRelayNonLoopbackKeepsUnixOnly(t *testing.T) {
	logged := captureStderr(t)
	t.Setenv("AC_RELAY_LISTEN", "10.0.0.1:7400")
	sock, _, ready := serveBoth(t)
	r := roundTrip(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: "/r", SessionID: "s-a", Source: "hook"})
	if !r.OK || r.Name == "" {
		t.Fatalf("unix coordination must survive a bad relay address: %+v", r)
	}
	select {
	case addr := <-ready:
		t.Fatalf("a non-loopback relay must not bind: %s", addr)
	default:
	}
	if !strings.Contains(logged(), "loopback") {
		t.Fatalf("the refusal must be logged: %q", logged())
	}
}

// AC_RELAY_INSECURE drops the shared token and nothing else: the per-session
// secret, the allowlist and the scope rules all still hold.
func TestRelayInsecureSkipsOnlyTheToken(t *testing.T) {
	logged := captureStderr(t)
	t.Setenv("AC_RELAY_INSECURE", "1")
	t.Setenv("AC_RELAY_LISTEN", "127.0.0.1:0")
	_, _, ready := serveBoth(t)
	var addr string
	select {
	case addr = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("relay listener never came up")
	}
	if !strings.Contains(logged(), "AC_RELAY_INSECURE") {
		t.Fatalf("the insecure listener must warn loudly: %q", logged())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(os.Getenv("AC_DB")), "relay.token")); err == nil {
		t.Fatal("an insecure relay must not publish a token file")
	}
	reg := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "host:BOX",
		SessionID: "broker-1", Kind: protocol.KindLauncher})
	if !reg.OK || len(reg.SessionSecret) != 64 {
		t.Fatalf("register without a token: %+v", reg)
	}
	if r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpPeek, SessionID: "broker-1",
		SessionSecret: strings.Repeat("0", 64)}); r.OK || r.Error != "unauthorized" {
		t.Fatalf("the session secret still binds: %+v", r)
	}
	if r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpClaims, SessionID: "broker-1",
		SessionSecret: reg.SessionSecret}); r.OK || r.Error != "op not allowed on relay" {
		t.Fatalf("the allowlist still holds: %+v", r)
	}
}

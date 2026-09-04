package daemon

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
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

// premintSecret is the credential a broker mints for itself before its first
// register: 32 bytes as lowercase hex. Deriving it from the session id lets a
// test retry a registration with the very same secret, as the broker does.
func premintSecret(session string) string {
	sum := sha256.Sum256([]byte("premint:" + session))
	return hex.EncodeToString(sum[:])
}

// hex12 reports whether v is an agent_id: 12 lowercase hex characters.
func hex12(v string) bool {
	if len(v) != 12 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil && strings.ToLower(v) == v
}

// registerLauncher is the host broker's first call: the shared token plus the
// secret it preminted buy a launcher row in its reserved host: scope. It
// returns the row and that secret, which is what its later requests carry.
func registerLauncher(t *testing.T, addr, token, session, scope string) (protocol.Response, string) {
	t.Helper()
	secret := premintSecret(session)
	r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: scope,
		SessionID: session, Kind: protocol.KindLauncher, Platform: "windows",
		Capabilities: []string{"browser.chrome", "provider.claude"}, Token: token, SessionSecret: secret})
	if !r.OK || r.Name == "" || !hex12(r.AgentID) {
		t.Fatalf("launcher register: %+v", r)
	}
	if r.SessionSecret != "" {
		t.Fatalf("a launcher brings its own secret; the store must mint none: %+v", r)
	}
	return r, secret
}

// A launcher registers with the secret it preminted: the same one every time,
// so a lost response costs a retry and never an identity, and a caller
// without it gets no row at all.
func TestRelayRegisterTakesAPremintedSecret(t *testing.T) {
	_, addr, tok := startRelayDaemon(t)
	first, secret := registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	again := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "host:BOX",
		SessionID: "broker-1", Kind: protocol.KindLauncher, Token: tok, SessionSecret: secret})
	if !again.OK || again.Name != first.Name || again.AgentID != first.AgentID || again.SessionSecret != "" {
		t.Fatalf("an exact retry must be idempotent: %+v", again)
	}
	stolen := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "host:BOX",
		SessionID: "broker-1", Kind: protocol.KindLauncher, Token: tok,
		SessionSecret: premintSecret("someone-else")})
	if stolen.OK || stolen.Error != "unauthorized" {
		t.Fatalf("the token alone must not re-claim a live relay session: %+v", stolen)
	}
	for _, secret := range []string{"", "short", strings.Repeat("Z", 64)} {
		r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "host:BOX",
			SessionID: "broker-new", Kind: protocol.KindLauncher, Token: tok, SessionSecret: secret})
		if r.OK || r.Error != "unauthorized" {
			t.Fatalf("a new launcher without a minted secret %q: %+v", secret, r)
		}
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
	reg, secret := registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	board := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpBoard,
		SessionID: "broker-1", Token: tok, SessionSecret: secret})
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
	broker, brokerSecret := registerLauncher(t, addr, tok, "broker-1", "host:BOX")
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
		AuthSessionID: "broker-1", SessionSecret: brokerSecret})
	if unnamed.OK || unnamed.Error != "unauthorized" {
		t.Fatalf("eyes session must be eyes-<task_id>: %+v", unnamed)
	}
	// The child belongs to the requester's workspace, not one the caller picks.
	elsewhere := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "/other",
		SessionID: child, Kind: protocol.KindEyes, Token: tok,
		AuthSessionID: "broker-1", SessionSecret: brokerSecret})
	if elsewhere.OK || elsewhere.Error != "foreign session" {
		t.Fatalf("eyes child in a foreign scope: %+v", elsewhere)
	}
	// Another broker holds a valid secret, but not this task.
	_, otherSecret := registerLauncher(t, addr, tok, "broker-2", "host:BOX2")
	poached := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "/r",
		SessionID: child, Kind: protocol.KindEyes, Token: tok,
		AuthSessionID: "broker-2", SessionSecret: otherSecret})
	if poached.OK || poached.Error != "not your task" {
		t.Fatalf("only the assigned launcher mints the child: %+v", poached)
	}

	minted := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "/r",
		SessionID: child, Kind: protocol.KindEyes, Token: tok,
		AuthSessionID: "broker-1", SessionSecret: brokerSecret})
	if !minted.OK || minted.Name == "" || !hex12(minted.AgentID) || len(minted.SessionSecret) != 64 {
		t.Fatalf("minted child: %+v", minted)
	}
	if minted.SessionSecret == brokerSecret || minted.AgentID == broker.AgentID {
		t.Fatalf("the child must get an identity of its own: %+v", minted)
	}
	// The child's own secret is what its later requests carry, and the
	// launcher's is not a licence to act AS the child.
	if r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpPeek, SessionID: child,
		Token: tok, SessionSecret: minted.SessionSecret}); !r.OK {
		t.Fatalf("child peek: %+v", r)
	}
	if r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpPeek, SessionID: child,
		Token: tok, SessionSecret: brokerSecret}); r.OK || r.Error != "unauthorized" {
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
	_, secret := registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	for _, op := range []string{protocol.OpClaim, protocol.OpRelease, protocol.OpClaims,
		protocol.OpHistory, protocol.OpWhoami, protocol.OpRequestEyes, protocol.OpCancelEyes} {
		r := tcpRoundTrip(t, addr, protocol.Request{Op: op, SessionID: "broker-1",
			Token: tok, SessionSecret: secret, Path: "/x", Brief: "b"})
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
	secret := premintSecret("broker-1")
	reg, err := st.RegisterRelay(store.RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay", Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	gate := &relayGate{st: st, token: "shared-token"}
	req := protocol.Request{Op: protocol.OpSend, Scope: "/somewhere-else", From: "impostor",
		To: "someone", Body: "hi", TaskID: "task-000000000000", AgentID: "deadbeef1234",
		SessionID: "broker-1", Token: "shared-token", SessionSecret: secret}
	if resp, final := gate.check(&req); final {
		t.Fatalf("a proven relay send must dispatch: %+v", resp)
	}
	if req.Scope != "host:BOX" || req.From != reg.Name || req.TaskID != "" || req.AgentID != "" {
		t.Fatalf("gate must answer as the row: %+v", req)
	}
	if resp, final := (*relayGate)(nil).check(&req); final {
		t.Fatalf("the unix listener has no gate: %+v", resp)
	}
	// An empty token authorizes nobody: only the explicit insecure flag does.
	blank := &relayGate{st: st}
	if resp, _ := blank.check(&protocol.Request{Op: protocol.OpPeek, SessionID: "broker-1",
		SessionSecret: secret}); resp.Error != "unauthorized" {
		t.Fatalf("an empty token must not disable auth: %+v", resp)
	}
	open := &relayGate{st: st, insecure: true}
	if resp, final := open.check(&protocol.Request{Op: protocol.OpPeek, SessionID: "broker-1",
		SessionSecret: secret}); final {
		t.Fatalf("insecure must skip only the token: %+v", resp)
	}
}

// A relay client has no subagents: an agent_id it sends is dropped, so an
// event can never mint a hook-subagent row in the requester's workspace.
func TestRelayEventCannotMintChildRows(t *testing.T) {
	sock, addr, tok := startRelayDaemon(t)
	reg, secret := registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpEvent, SessionID: "broker-1",
		Token: tok, SessionSecret: secret, AgentID: "childid12345", AgentType: "eyes",
		Tool: "Read", Activity: "polling"})
	if !r.OK {
		t.Fatalf("relay event: %+v", r)
	}
	board := roundTrip(t, sock, protocol.Request{Op: protocol.OpBoard, Scope: "host:BOX", IncludeGone: true})
	if !board.OK || len(board.Agents) != 1 || board.Agents[0].Name != reg.Name {
		t.Fatalf("an agent_id from the relay must mint nothing: %+v", board.Agents)
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
		store.ErrNoProvider:     "no matching provider",
		store.ErrBadRuntime:     store.ErrBadRuntime.Error(),
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
	// Anything undocumented is this daemon's problem: the caller gets no
	// detail, and the detail is logged instead.
	logged := captureStderr(t)
	if got := relayError(fmt.Errorf("database is locked: %s", "/state/d.db")); got != "internal error" {
		t.Errorf("an undocumented failure must not cross the wire: %q", got)
	}
	if !strings.Contains(logged(), "database is locked") {
		t.Errorf("the detail must be logged: %q", logged())
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
	secret := premintSecret("broker-1")
	reg := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "host:BOX",
		SessionID: "broker-1", Kind: protocol.KindLauncher, SessionSecret: secret})
	if !reg.OK || !hex12(reg.AgentID) {
		t.Fatalf("register without a token: %+v", reg)
	}
	if r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpPeek, SessionID: "broker-1",
		SessionSecret: strings.Repeat("0", 64)}); r.OK || r.Error != "unauthorized" {
		t.Fatalf("the session secret still binds: %+v", r)
	}
	if r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpClaims, SessionID: "broker-1",
		SessionSecret: secret}); r.OK || r.Error != "op not allowed on relay" {
		t.Fatalf("the allowlist still holds: %+v", r)
	}
}

// registerUnix registers a hook session over the unix socket and returns its
// full ref: only whoami reports the agent id a cross-scope target addresses.
func registerUnix(t *testing.T, sock, scope, session string) protocol.AgentRef {
	t.Helper()
	if r := roundTrip(t, sock, protocol.Request{Op: protocol.OpRegister, Scope: scope,
		SessionID: session, Source: "hook"}); !r.OK || r.Name == "" {
		t.Fatalf("register %s/%s: %+v", scope, session, r)
	}
	who := roundTrip(t, sock, protocol.Request{Op: protocol.OpWhoami, Scope: scope, SessionID: session})
	if !who.OK {
		t.Fatalf("whoami %s/%s: %+v", scope, session, who)
	}
	return protocol.AgentRef{Name: who.Name, AgentID: who.AgentID, Scope: scope}
}

// The workspace directory and the eyes listing are cross-scope reads that need
// no from, and the reserved host: scope stays out of the directory - it is
// broker plumbing, which is where list_eyes belongs.
func TestListWorkspacesAndListEyes(t *testing.T) {
	sock, addr, tok := startRelayDaemon(t)
	registerUnix(t, sock, "/repo-a", "s-a")
	registerUnix(t, sock, "/repo-b", "s-b")
	launcher, secret := registerLauncher(t, addr, tok, "broker-1", "host:BOX")

	ws := roundTrip(t, sock, protocol.Request{Op: protocol.OpListWorkspaces})
	if !ws.OK || len(ws.Workspaces) != 2 {
		t.Fatalf("want two workspaces, got %+v", ws)
	}
	for _, w := range ws.Workspaces {
		if strings.HasPrefix(w.Scope, "host:") {
			t.Fatalf("a host: scope must not be listed as a workspace: %+v", ws.Workspaces)
		}
		if w.LiveAgents != 1 {
			t.Fatalf("occupancy: %+v", w)
		}
	}
	eyes := roundTrip(t, sock, protocol.Request{Op: protocol.OpListEyes})
	if !eyes.OK || len(eyes.Agents) != 1 {
		t.Fatalf("want the launcher, got %+v", eyes)
	}
	if a := eyes.Agents[0]; a.Name != launcher.Name || a.Kind != protocol.KindLauncher ||
		a.Scope != "host:BOX" || a.Origin != relayOrigin || a.Platform != "windows" ||
		len(a.Capabilities) != 2 {
		t.Fatalf("launcher row: %+v", a)
	}
	// In-scope the board answers for one workspace, so it carries the relay
	// fields but no scope of its own.
	board := roundTrip(t, sock, protocol.Request{Op: protocol.OpBoard, Scope: "host:BOX"})
	if !board.OK || len(board.Agents) != 1 {
		t.Fatalf("host board: %+v", board)
	}
	if a := board.Agents[0]; a.Kind != protocol.KindLauncher || a.Origin != relayOrigin ||
		a.Platform != "windows" || len(a.Capabilities) != 2 || a.Scope != "" {
		t.Fatalf("board row: %+v", a)
	}
	// The broker reads the same directory over the relay.
	over := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpListWorkspaces,
		SessionID: "broker-1", Token: tok, SessionSecret: secret})
	if !over.OK || len(over.Workspaces) != 2 {
		t.Fatalf("relay list_workspaces: %+v", over)
	}
}

// send_workspace delivers into another scope, and the daemon - not the caller -
// stamps who it is from: a client-supplied reply_to is ignored.
func TestSendWorkspaceStampsSenderAndIgnoresClientReplyTo(t *testing.T) {
	sock, _, _ := startRelayDaemon(t)
	a := registerUnix(t, sock, "/repo-a", "s-a")
	b := registerUnix(t, sock, "/repo-b", "s-b")
	r := roundTrip(t, sock, protocol.Request{Op: protocol.OpSendWorkspace, Scope: "/repo-a", SessionID: "s-a",
		From: a.Name, Body: "hello over there",
		Target:  &protocol.AgentRef{Scope: "/repo-b", Name: b.Name},
		ReplyTo: &protocol.AgentRef{Name: "impostor", AgentID: "deadbeef1234", Scope: "/elsewhere"}})
	if !r.OK {
		t.Fatalf("send_workspace: %+v", r)
	}
	// An agent id beats a name: names are per-scope labels, ids are stable.
	if byID := roundTrip(t, sock, protocol.Request{Op: protocol.OpSendWorkspace, Scope: "/repo-a",
		SessionID: "s-a", Body: "by id",
		Target: &protocol.AgentRef{Scope: "/repo-b", Name: "no-such-agent", AgentID: b.AgentID}}); !byID.OK {
		t.Fatalf("target by agent id: %+v", byID)
	}
	// An unknown target is the store's own refusal, not a silent drop.
	for _, target := range []*protocol.AgentRef{{Scope: "/repo-b", Name: "ghost"}, {Scope: "/nowhere", Name: b.Name}} {
		bad := roundTrip(t, sock, protocol.Request{Op: protocol.OpSendWorkspace, Scope: "/repo-a",
			SessionID: "s-a", Body: "nobody home", Target: target})
		if bad.OK || !strings.Contains(bad.Error, "no agent") {
			t.Fatalf("target %+v: %+v", target, bad)
		}
	}
	read := roundTrip(t, sock, protocol.Request{Op: protocol.OpRead, Scope: "/repo-b", From: b.Name})
	if !read.OK || len(read.Messages) != 2 {
		t.Fatalf("recipient inbox: %+v", read)
	}
	m := read.Messages[0]
	if m.From != a.Name || m.FromScope != "/repo-a" || m.Body != "hello over there" || m.Broadcast {
		t.Fatalf("delivered message: %+v", m)
	}
	if m.ReplyTo == nil || *m.ReplyTo != a {
		t.Fatalf("reply_to must be the authenticated sender %+v, got %+v", a, m.ReplyTo)
	}
	// The sender's own workspace saw nothing.
	if own := roundTrip(t, sock, protocol.Request{Op: protocol.OpRead, Scope: "/repo-a", From: a.Name}); len(own.Messages) != 0 {
		t.Fatalf("the message must not land in the sender's scope: %+v", own.Messages)
	}
	// In scope, send_workspace is ordinary mail: from_scope appears only when
	// the sender really is somewhere else.
	c := registerUnix(t, sock, "/repo-a", "s-a2")
	if r := roundTrip(t, sock, protocol.Request{Op: protocol.OpSendWorkspace, Scope: "/repo-a",
		SessionID: "s-a", Body: "next door",
		Target: &protocol.AgentRef{Scope: "/repo-a", Name: c.Name}}); !r.OK {
		t.Fatalf("in-scope send_workspace: %+v", r)
	}
	inScope := roundTrip(t, sock, protocol.Request{Op: protocol.OpRead, Scope: "/repo-a", From: c.Name})
	if len(inScope.Messages) != 1 || inScope.Messages[0].FromScope != "" || inScope.Messages[0].From != a.Name {
		t.Fatalf("in-scope mail must carry no from_scope: %+v", inScope.Messages)
	}
}

// No target name means a scoped broadcast: everyone live in that workspace.
func TestSendWorkspaceWithoutNameReachesEveryone(t *testing.T) {
	sock, _, _ := startRelayDaemon(t)
	a := registerUnix(t, sock, "/repo-a", "s-a")
	b1 := registerUnix(t, sock, "/repo-b", "s-b1")
	b2 := registerUnix(t, sock, "/repo-b", "s-b2")
	r := roundTrip(t, sock, protocol.Request{Op: protocol.OpSendWorkspace, Scope: "/repo-a", SessionID: "s-a",
		From: a.Name, Body: "heads up", Target: &protocol.AgentRef{Scope: "/repo-b"}})
	if !r.OK {
		t.Fatalf("scoped broadcast: %+v", r)
	}
	for _, ref := range []protocol.AgentRef{b1, b2} {
		read := roundTrip(t, sock, protocol.Request{Op: protocol.OpRead, Scope: "/repo-b", From: ref.Name})
		if len(read.Messages) != 1 || !read.Messages[0].Broadcast || read.Messages[0].FromScope != "/repo-a" {
			t.Fatalf("%s inbox: %+v", ref.Name, read.Messages)
		}
	}
}

// A relay client's identity is the row it proved it owns: the return address
// is stamped from that row, and the task id it chose is dropped.
func TestRelaySendWorkspaceStampsTheLauncher(t *testing.T) {
	sock, addr, tok := startRelayDaemon(t)
	b := registerUnix(t, sock, "/repo-b", "s-b")
	launcher, secret := registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpSendWorkspace, SessionID: "broker-1",
		Token: tok, SessionSecret: secret, Scope: "/elsewhere", From: "impostor",
		Body: "chrome is up", TaskID: "task-000000000000",
		Target:  &protocol.AgentRef{Scope: "/repo-b", Name: b.Name},
		ReplyTo: &protocol.AgentRef{Name: "impostor", AgentID: "deadbeef1234", Scope: "/elsewhere"}})
	if !r.OK {
		t.Fatalf("relay send_workspace: %+v", r)
	}
	read := roundTrip(t, sock, protocol.Request{Op: protocol.OpRead, Scope: "/repo-b", From: b.Name})
	if !read.OK || len(read.Messages) != 1 {
		t.Fatalf("recipient inbox: %+v", read)
	}
	m := read.Messages[0]
	if m.From != launcher.Name || m.FromScope != "host:BOX" || m.Kind != protocol.KindLauncher {
		t.Fatalf("delivered message: %+v", m)
	}
	want := protocol.AgentRef{Name: launcher.Name, AgentID: launcher.AgentID, Scope: "host:BOX"}
	if m.ReplyTo == nil || *m.ReplyTo != want {
		t.Fatalf("reply_to must be the proven row %+v, got %+v", want, m.ReplyTo)
	}
	if m.TaskID != "" {
		t.Fatalf("a task id is the daemon's to stamp: %+v", m)
	}
}

// Every identified call is a heartbeat, the new ops included: a listing lifts
// a sticky idle back to active.
func TestNewOpsRefreshTheCaller(t *testing.T) {
	sock, _, _ := startRelayDaemon(t)
	a := registerUnix(t, sock, "/r", "s-a")
	roundTrip(t, sock, protocol.Request{Op: protocol.OpIdle, Scope: "/r", SessionID: "s-a"})
	board := roundTrip(t, sock, protocol.Request{Op: protocol.OpBoard, Scope: "/r", IncludeGone: true})
	if len(board.Agents) != 1 || board.Agents[0].Status != "idle" {
		t.Fatalf("precondition - the agent must be idle: %+v", board.Agents)
	}
	roundTrip(t, sock, protocol.Request{Op: protocol.OpListWorkspaces, Scope: "/r", SessionID: "s-a"})
	board = roundTrip(t, sock, protocol.Request{Op: protocol.OpBoard, Scope: "/r", IncludeGone: true})
	if len(board.Agents) != 1 || board.Agents[0].Status != "active" || board.Agents[0].Name != a.Name {
		t.Fatalf("list_workspaces must count as a heartbeat: %+v", board.Agents)
	}
}

// isTaskID reports whether v is a minted task id: "task-" plus 12 hex.
func isTaskID(v string) bool {
	id, ok := strings.CutPrefix(v, "task-")
	return ok && hex12(id)
}

// tcpHangUp sends one relay request and closes without reading the answer -
// the client whose response is lost on the way back.
func tcpHangUp(t *testing.T, addr string, req protocol.Request) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatal(err)
	}
}

// awaitRow waits for a row a hung-up request wrote, so a retry never races
// the handler that is still finishing the first call.
func awaitRow(t *testing.T, st *store.Store, scope, session string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if _, err := st.Identity(scope, session); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no row for %s/%s", scope, session)
}

// The whole happy path a requester sees: a task id, the broker that took it,
// and the launch waiting in that broker's inbox with the requester as the
// return address.
func TestRequestEyesQueuesTheLaunch(t *testing.T) {
	sock, addr, tok := startRelayDaemon(t)
	a := registerUnix(t, sock, "/r", "s-a")
	broker, secret := registerLauncher(t, addr, tok, "broker-1", "host:BOX")

	r := roundTrip(t, sock, protocol.Request{Op: protocol.OpRequestEyes, Scope: "/r", SessionID: "s-a",
		From: a.Name, Brief: "does the login page render", Runtime: "claude"})
	if !r.OK || !isTaskID(r.TaskID) {
		t.Fatalf("request_eyes: %+v", r)
	}
	want := protocol.AgentRef{Name: broker.Name, AgentID: broker.AgentID, Scope: "host:BOX"}
	if r.Launcher == nil || *r.Launcher != want {
		t.Fatalf("launcher %+v, want %+v", r.Launcher, want)
	}
	inbox := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRead, SessionID: "broker-1",
		Token: tok, SessionSecret: secret})
	if !inbox.OK || len(inbox.Messages) != 1 {
		t.Fatalf("launcher inbox: %+v", inbox)
	}
	m := inbox.Messages[0]
	if m.From != a.Name || m.FromScope != "/r" || m.TaskID != r.TaskID {
		t.Fatalf("launch envelope: %+v", m)
	}
	var launch protocol.TaskLaunchMsg
	if err := json.Unmarshal([]byte(m.Body), &launch); err != nil {
		t.Fatalf("launch body %q: %v", m.Body, err)
	}
	if launch.Type != protocol.TaskLaunch || launch.TaskID != r.TaskID || launch.Runtime != "claude" ||
		launch.Scope != "/r" || launch.Brief != "does the login page render" || launch.DeadlineS != 300 {
		t.Fatalf("launch body: %+v", launch)
	}
	if launch.ReplyTo != a {
		t.Fatalf("reply_to must be the requester %+v, got %+v", a, launch.ReplyTo)
	}
}

// The ways a brief cannot be placed are four different answers, so the
// requester knows whether to wait, to ask for another runtime, or to fix the
// call.
func TestRequestEyesRefusals(t *testing.T) {
	sock, addr, tok := startRelayDaemon(t)
	a := registerUnix(t, sock, "/r", "s-a")
	ask := func(runtime string) protocol.Response {
		return roundTrip(t, sock, protocol.Request{Op: protocol.OpRequestEyes, Scope: "/r",
			SessionID: "s-a", From: a.Name, Brief: "b", Runtime: runtime})
	}
	if r := ask("claude"); r.OK || r.Error != "no host launcher" {
		t.Fatalf("no broker is polling: %+v", r)
	}
	registerLauncher(t, addr, tok, "broker-1", "host:BOX") // browser.chrome + provider.claude
	if r := ask("codex"); r.OK || r.Error != "no matching provider" {
		t.Fatalf("a runtime no broker runs: %+v", r)
	}
	if r := ask("Claude"); r.OK || r.Error != `invalid runtime: "Claude"` {
		t.Fatalf("a runtime that is not a provider name: %+v", r)
	}
	if r := ask("claude"); !r.OK {
		t.Fatalf("request_eyes: %+v", r)
	}
	if r := ask("claude"); r.OK || r.Error != "eyes busy" {
		t.Fatalf("the only broker already holds a job: %+v", r)
	}
}

// Cancel is the requester's own move: a stranger cannot make it, an id nobody
// minted is not a task, and a repeat is a no-op rather than a second cancel.
func TestCancelEyesAuthorizesAgainstTheRequester(t *testing.T) {
	sock, addr, tok := startRelayDaemon(t)
	a := registerUnix(t, sock, "/r", "s-a")
	b := registerUnix(t, sock, "/r", "s-b")
	_, secret := registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	req := roundTrip(t, sock, protocol.Request{Op: protocol.OpRequestEyes, Scope: "/r", SessionID: "s-a",
		From: a.Name, Brief: "b"})
	if !req.OK {
		t.Fatalf("request_eyes: %+v", req)
	}
	cancel := func(session, from, taskID string) protocol.Response {
		return roundTrip(t, sock, protocol.Request{Op: protocol.OpCancelEyes, Scope: "/r",
			SessionID: session, From: from, TaskID: taskID})
	}
	if r := cancel("s-a", a.Name, "task-000000000000"); r.OK || r.Error != "unknown task" {
		t.Fatalf("an id nobody minted: %+v", r)
	}
	if r := cancel("s-b", b.Name, req.TaskID); r.OK || r.Error != "not your task" {
		t.Fatalf("a stranger must not cancel: %+v", r)
	}
	if r := cancel("s-a", a.Name, req.TaskID); !r.OK || r.TaskID != req.TaskID {
		t.Fatalf("the requester cancels: %+v", r)
	}
	if r := cancel("s-a", a.Name, req.TaskID); !r.OK || r.TaskID != req.TaskID {
		t.Fatalf("a repeat must be a no-op, not an error: %+v", r)
	}
	inbox := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRead, SessionID: "broker-1",
		Token: tok, SessionSecret: secret})
	if len(inbox.Messages) != 2 {
		t.Fatalf("the broker holds the launch and exactly one cancel: %+v", inbox.Messages)
	}
	var body protocol.TaskCancelMsg
	if err := json.Unmarshal([]byte(inbox.Messages[1].Body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Type != protocol.TaskCancel || body.TaskID != req.TaskID {
		t.Fatalf("cancel body: %+v", body)
	}
}

// The loop as the broker drives it: read the launch, mint the child, ack as
// the launcher, report as the child. The requester ends up with the ack and
// one report from the child - a retried report adds no second copy.
func TestEyesTaskRoundTrip(t *testing.T) {
	sock, addr, tok := startRelayDaemon(t)
	a := registerUnix(t, sock, "/r", "s-a")
	launcher, secret := registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	req := roundTrip(t, sock, protocol.Request{Op: protocol.OpRequestEyes, Scope: "/r",
		SessionID: "s-a", From: a.Name, Brief: "does the login page render"})
	if !req.OK {
		t.Fatalf("request_eyes: %+v", req)
	}
	inbox := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRead, SessionID: "broker-1",
		Token: tok, SessionSecret: secret})
	if len(inbox.Messages) != 1 || inbox.Messages[0].TaskID != req.TaskID {
		t.Fatalf("launcher inbox: %+v", inbox.Messages)
	}
	var launch protocol.TaskLaunchMsg
	if err := json.Unmarshal([]byte(inbox.Messages[0].Body), &launch); err != nil {
		t.Fatal(err)
	}
	child := "eyes-" + launch.TaskID
	minted := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: launch.Scope,
		SessionID: child, Kind: protocol.KindEyes, Token: tok,
		AuthSessionID: "broker-1", SessionSecret: secret})
	if !minted.OK {
		t.Fatalf("child register: %+v", minted)
	}
	// The broker replies to the address the launch carried, exactly as it
	// would for any other mail.
	target := &protocol.AgentRef{Scope: launch.ReplyTo.Scope, Name: launch.ReplyTo.Name}
	accepted, err := json.Marshal(protocol.TaskAcceptedMsg{Type: protocol.TaskAccepted, TaskID: launch.TaskID,
		Child: &protocol.AgentRef{Name: minted.Name, AgentID: minted.AgentID, Scope: launch.Scope}})
	if err != nil {
		t.Fatal(err)
	}
	if r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpSendWorkspace, SessionID: "broker-1",
		Token: tok, SessionSecret: secret, Body: string(accepted), Target: target}); !r.OK {
		t.Fatalf("task.accepted: %+v", r)
	}
	result, err := json.Marshal(protocol.TaskResultMsg{Type: protocol.TaskResult, TaskID: launch.TaskID,
		Status: "ok", Summary: "the login page renders"})
	if err != nil {
		t.Fatal(err)
	}
	report := func() protocol.Response {
		return tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpSendWorkspace, SessionID: child,
			Token: tok, SessionSecret: minted.SessionSecret, Body: string(result), Target: target})
	}
	if r := report(); !r.OK {
		t.Fatalf("task.result: %+v", r)
	}
	if r := report(); !r.OK {
		t.Fatalf("a lost ack costs a retry, not an error: %+v", r)
	}
	read := roundTrip(t, sock, protocol.Request{Op: protocol.OpRead, Scope: "/r", From: a.Name})
	if len(read.Messages) != 2 {
		t.Fatalf("requester inbox: %+v", read.Messages)
	}
	ack, rep := read.Messages[0], read.Messages[1]
	if ack.From != launcher.Name || ack.FromScope != "host:BOX" || ack.Kind != protocol.KindLauncher ||
		ack.TaskID != req.TaskID || ack.Body != string(accepted) {
		t.Fatalf("the ack comes from the launcher: %+v", ack)
	}
	// The child lives in the requester's own workspace, so its report carries
	// no from_scope at all.
	if rep.From != minted.Name || rep.FromScope != "" || rep.Kind != protocol.KindEyes ||
		rep.TaskID != req.TaskID || rep.Body != string(result) {
		t.Fatalf("the report comes from the child: %+v", rep)
	}
}

// A launch is redelivered until the broker acks it: a lost read must not
// strand the task, one poll must never hand back the same launch twice, and
// the ack is what stops it.
func TestLauncherPollRedeliversAQueuedLaunch(t *testing.T) {
	sock, addr, tok := startRelayDaemon(t)
	a := registerUnix(t, sock, "/r", "s-a")
	_, secret := registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	req := roundTrip(t, sock, protocol.Request{Op: protocol.OpRequestEyes, Scope: "/r",
		SessionID: "s-a", From: a.Name, Brief: "b"})
	if !req.OK {
		t.Fatalf("request_eyes: %+v", req)
	}
	poll := func(op string) protocol.Response {
		return tcpRoundTrip(t, addr, protocol.Request{Op: op, SessionID: "broker-1",
			Token: tok, SessionSecret: secret})
	}
	first := poll(protocol.OpRead)
	if len(first.Messages) != 1 || first.Messages[0].TaskID != req.TaskID {
		t.Fatalf("first poll: %+v", first.Messages)
	}
	again := poll(protocol.OpRead)
	if len(again.Messages) != 1 || again.Messages[0].ID != first.Messages[0].ID {
		t.Fatalf("a queued launch must come back: %+v", again.Messages)
	}
	if p := poll(protocol.OpPeek); p.Unread != 1 || len(p.PeekIDs) != 1 || p.PeekIDs[0] != first.Messages[0].ID {
		t.Fatalf("peek must show the unfinished launch: %+v", p)
	}
	accepted, err := json.Marshal(protocol.TaskAcceptedMsg{Type: protocol.TaskAccepted, TaskID: req.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpSendWorkspace, SessionID: "broker-1",
		Token: tok, SessionSecret: secret, Body: string(accepted)}); !r.OK {
		t.Fatalf("task.accepted: %+v", r)
	}
	if done := poll(protocol.OpRead); len(done.Messages) != 0 {
		t.Fatalf("an accepted launch must stop coming back: %+v", done.Messages)
	}
	if p := poll(protocol.OpPeek); p.Unread != 0 {
		t.Fatalf("peek after the ack: %+v", p)
	}
}

// A register whose response never arrives costs a retry and nothing else: the
// broker re-presents the secret it preminted, and the child is re-minted on
// its launcher's authority with a fresh one.
func TestRelayRegisterSurvivesALostResponse(t *testing.T) {
	sock, addr, tok, st := relayDaemon(t)
	secret := premintSecret("broker-1")
	launcher := protocol.Request{Op: protocol.OpRegister, Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Capabilities: []string{"browser.chrome", "provider.claude"},
		Token: tok, SessionSecret: secret}
	tcpHangUp(t, addr, launcher)
	awaitRow(t, st, "host:BOX", "broker-1")
	retry := tcpRoundTrip(t, addr, launcher)
	if !retry.OK || !hex12(retry.AgentID) {
		t.Fatalf("the exact retry must succeed: %+v", retry)
	}
	a := registerUnix(t, sock, "/r", "s-a")
	req := roundTrip(t, sock, protocol.Request{Op: protocol.OpRequestEyes, Scope: "/r",
		SessionID: "s-a", From: a.Name, Brief: "b"})
	if !req.OK {
		t.Fatalf("request_eyes: %+v", req)
	}
	child := protocol.Request{Op: protocol.OpRegister, Scope: "/r", SessionID: "eyes-" + req.TaskID,
		Kind: protocol.KindEyes, Token: tok, AuthSessionID: "broker-1", SessionSecret: secret}
	tcpHangUp(t, addr, child)
	awaitRow(t, st, "/r", "eyes-"+req.TaskID)
	// The child's secret was lost with the response, so the retry mints
	// another one - and that one is what drives the child.
	reissued := tcpRoundTrip(t, addr, child)
	if !reissued.OK || len(reissued.SessionSecret) != 64 {
		t.Fatalf("the child must be re-minted: %+v", reissued)
	}
	if r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpPeek, SessionID: "eyes-" + req.TaskID,
		Token: tok, SessionSecret: reissued.SessionSecret}); !r.OK {
		t.Fatalf("the reissued secret must drive the child: %+v", r)
	}
}

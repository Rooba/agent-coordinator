package daemon

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/store"
)

const (
	// hostScopePrefix is the reserved scope a host broker registers into. The
	// shared token buys a launcher row there and nowhere else.
	hostScopePrefix = "host:"
	// eyesSessionPrefix ties a child session to the task it reports on.
	eyesSessionPrefix = "eyes-"
	// relayOrigin marks every row created over TCP, and every request that
	// arrived there, so a relay client can never be mistaken for a hook or
	// MCP session - nor a local one for a broker.
	relayOrigin = store.RelayOrigin
)

// relayOps is everything a relay client may ask for. What is missing is the
// point: the claims ledger, the message journal, whoami and the eyes ops
// never cross the boundary, so a host broker cannot summon itself or read
// another workspace's coordination state.
var relayOps = map[string]bool{
	protocol.OpRegister: true, protocol.OpDeregister: true, protocol.OpEvent: true,
	protocol.OpIdle: true, protocol.OpBoard: true, protocol.OpAgents: true,
	protocol.OpSend: true, protocol.OpBroadcast: true, protocol.OpRead: true,
	protocol.OpPeek: true, protocol.OpListWorkspaces: true, protocol.OpListEyes: true,
	protocol.OpSendWorkspace: true,
}

// relayWireErrors are the only failures whose text may cross the wire: each
// one is the caller's own mistake, and none of them names anything the caller
// did not already send. Anything else is this daemon's problem, so it is
// logged here and answered "internal error".
var relayWireErrors = []error{
	store.ErrRelayAuth, store.ErrForeignSession, store.ErrEyesBusy, store.ErrNoLauncher,
	store.ErrNoProvider, store.ErrBadRuntime, store.ErrUnknownTask, store.ErrNotYourTask,
	store.ErrTaskNotLive, store.ErrBadTransition, store.ErrNoAgent, errReservedTask,
}

// relayGate authenticates TCP requests and rewrites their identity from the
// row the caller proved it owns. A nil gate is the unix listener, whose trust
// boundary is the socket directory's permissions.
type relayGate struct {
	st       *store.Store
	token    string
	insecure bool // AC_RELAY_INSECURE: skip the shared-token compare, nothing else
}

// relayLog is the relay's one log line. Loopback traffic is low volume and a
// refusal has to be visible; credentials never appear in it.
func relayLog(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "relay: "+format+"\n", args...)
}

// refuse is a gate refusal: the connection is answered and closed with
// nothing dispatched, and the caller is told only which rule it broke.
func refuse(reason string) (protocol.Response, bool) {
	return protocol.Response{Error: reason}, true
}

// check either answers a relay request outright - a refusal, or the register
// that binds the caller's credential - or rewrites it for dispatch. Every
// refusal is logged here, in the one place they all pass through.
func (g *relayGate) check(req *protocol.Request, claimedKind string) (protocol.Response, bool) {
	if g == nil {
		return protocol.Response{}, false // unix: nothing to prove
	}
	resp, final := g.decide(req, claimedKind)
	if resp.Error != "" {
		relayLog("refused %s: %s", req.Op, resp.Error)
	}
	return resp, final
}

// decide is the gate proper: token, then allowlist, then the session binding,
// so a caller failing the first two never touches the store.
func (g *relayGate) decide(req *protocol.Request, claimedKind string) (protocol.Response, bool) {
	// An empty token authorizes nobody: only AC_RELAY_INSECURE, set on
	// purpose, drops this check.
	if !g.insecure && (g.token == "" || subtle.ConstantTimeCompare([]byte(req.Token), []byte(g.token)) != 1) {
		return refuse("unauthorized")
	}
	if !relayOps[req.Op] {
		return refuse("op not allowed on relay")
	}
	// Every relay request names its session. Without one there is no row to
	// answer as, and the unix socket's "no session" trust path would be
	// reachable from the wire.
	if req.SessionID == "" {
		return refuse("unauthorized")
	}
	if req.Op == protocol.OpRegister {
		return g.register(req, claimedKind)
	}
	id, err := g.st.VerifyRelaySecret(req.SessionID, req.SessionSecret)
	if err != nil {
		return refuse(relayError(err))
	}
	// The row, never the caller, says who this is - its scope, its name and its
	// role. A relay client has no subagents, so an agent_id it sent would mint
	// or retarget a child row in somebody else's workspace. Origin is the
	// daemon's own stamp: it is what lets this caller speak for a task at all.
	req.Scope, req.From, req.Kind, req.AgentID = id.Scope, id.Name, id.Kind, ""
	req.Origin = relayOrigin
	// A task id belongs to the eyes lifecycle, so ordinary mail never carries
	// one a relay client chose.
	if sendOps[req.Op] {
		req.TaskID = ""
	}
	return protocol.Response{}, false
}

// register answers the two registrations the relay allows, and the kind a
// caller asks to be is the one thing it says about itself that is read here.
// Each needs proof of its own: a broker brings the secret it preminted for its
// launcher row, while an eyes child is minted for it by the launcher holding
// that task.
func (g *relayGate) register(req *protocol.Request, claimedKind string) (protocol.Response, bool) {
	switch claimedKind {
	case protocol.KindLauncher:
		if !strings.HasPrefix(req.Scope, hostScopePrefix) {
			return refuse("foreign session")
		}
		// The store keeps only the sha256 of what the broker brought, and
		// mints nothing, so a lost response costs a retry and never an
		// identity.
		if _, err := g.st.RegisterRelay(store.RelayRegistration{
			Scope: req.Scope, SessionID: req.SessionID, Kind: claimedKind, Origin: relayOrigin,
			Platform: req.Platform, Capabilities: req.Capabilities, Secret: req.SessionSecret}); err != nil {
			return refuse(relayError(err))
		}
		return g.registered(req.Scope, req.SessionID, "")
	case protocol.KindEyes:
		// Only the launcher already holding this task's launch may spin its
		// child up: the session id names the task, auth_session_id and the
		// secret name the launcher, and the store proves the pairing. All
		// ownership, existence and state failures share one answer so this gate
		// is not a task-discovery oracle.
		taskID, named := strings.CutPrefix(req.SessionID, eyesSessionPrefix)
		if !named || req.AuthSessionID == "" {
			return refuse("unauthorized")
		}
		launcher, err := g.st.VerifyRelaySecret(req.AuthSessionID, req.SessionSecret)
		if err != nil || launcher.Kind != protocol.KindLauncher {
			return refuse("unauthorized")
		}
		task, err := g.st.EyesTask(taskID)
		if err != nil {
			return refuse(childRegisterError(err))
		}
		// The child lives in the requesting workspace, not one the caller
		// picks, so a broker asking for any other scope is refused outright.
		if task.RequesterScope != req.Scope {
			return refuse("unauthorized")
		}
		reg, err := g.st.ReissueEyesChild(taskID, req.AuthSessionID, req.SessionSecret)
		if err != nil {
			return refuse(childRegisterError(err))
		}
		return g.registered(task.RequesterScope, req.SessionID, reg.Secret)
	}
	return refuse("op not allowed on relay")
}

// childRegisterError hides every expected property of an eyes task. A real
// storage failure still follows the relay's generic internal-error path.
func childRegisterError(err error) string {
	for _, hidden := range []error{store.ErrUnknownTask, store.ErrNotYourTask, store.ErrTaskNotLive,
		store.ErrForeignSession, store.ErrRelayAuth, store.ErrNoSession} {
		if errors.Is(err, hidden) {
			return "unauthorized"
		}
	}
	return relayError(err)
}

// registered answers a register with the row that was actually written. The
// broker addresses its launcher and its children by agent_id, so a response
// that only named them would leave it guessing.
func (g *relayGate) registered(scope, sessionID, secret string) (protocol.Response, bool) {
	id, err := g.st.Identity(scope, sessionID)
	if err != nil {
		return refuse(relayError(err))
	}
	return protocol.Response{OK: true, Name: id.Name, AgentID: id.AgentID, SessionSecret: secret}, true
}

// relayError turns a store failure into the wire string the spec fixes. A
// session that does not exist and a secret that does not match share one
// answer, so a caller never learns which credential was wrong, and anything
// undocumented stays local.
func relayError(err error) string {
	if errors.Is(err, store.ErrNoSession) {
		return store.ErrRelayAuth.Error()
	}
	for _, wire := range relayWireErrors {
		if errors.Is(err, wire) {
			return err.Error()
		}
	}
	relayLog("internal: %v", err)
	return "internal error"
}

// relayErrorText is the same rule for a failure a dispatch already flattened
// to text: every op's answer passes it on the way out of the relay, so no
// store wording reaches a broker just because some op forgot.
func relayErrorText(msg string) string {
	for _, wire := range relayWireErrors {
		if strings.HasPrefix(msg, wire.Error()) {
			return msg
		}
	}
	relayLog("internal: %s", msg)
	return "internal error"
}

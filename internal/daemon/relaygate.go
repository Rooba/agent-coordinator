package daemon

import (
	"crypto/subtle"
	"errors"
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
	// relayOrigin marks every row created over TCP, so a relay client can
	// never be mistaken for a hook or MCP session.
	relayOrigin = "relay"
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

// relayGate authenticates TCP requests and rewrites their identity from the
// row the caller proved it owns. A nil gate is the unix listener, whose trust
// boundary is the socket directory's permissions.
type relayGate struct {
	st    *store.Store
	token string // empty means AC_RELAY_INSECURE: skip the shared-token check
}

// refuse is a gate refusal: the connection is answered and closed with
// nothing dispatched, and the caller is told only which rule it broke.
func refuse(reason string) (protocol.Response, bool) {
	return protocol.Response{Error: reason}, true
}

// check either answers a relay request outright - a refusal, or the register
// that mints the caller's credential - or rewrites it for dispatch. Token,
// then allowlist, then the session binding: a caller failing the first two
// never touches the store.
func (g *relayGate) check(req *protocol.Request) (protocol.Response, bool) {
	if g == nil {
		return protocol.Response{}, false // unix: nothing to prove
	}
	if g.token != "" && subtle.ConstantTimeCompare([]byte(req.Token), []byte(g.token)) != 1 {
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
		return g.register(req)
	}
	id, err := g.st.VerifyRelaySecret(req.SessionID, req.SessionSecret)
	if err != nil {
		return refuse(relayError(err))
	}
	// The row, never the caller, says who this is.
	req.Scope, req.From = id.Scope, id.Name
	// A task id belongs to the eyes lifecycle, so ordinary mail never carries
	// one a relay client chose.
	if req.Op == protocol.OpSend || req.Op == protocol.OpBroadcast {
		req.TaskID = ""
	}
	return protocol.Response{}, false
}

// register answers the two registrations the relay allows. It is the one op
// that mints a credential, and each shape needs proof of its own: the shared
// token for a broker's launcher row, the assigned launcher's secret for that
// task's eyes child.
func (g *relayGate) register(req *protocol.Request) (protocol.Response, bool) {
	switch req.Kind {
	case protocol.KindLauncher:
		if !strings.HasPrefix(req.Scope, hostScopePrefix) {
			return refuse("foreign session")
		}
		reg, err := g.st.RegisterRelay(store.RelayRegistration{
			Scope: req.Scope, SessionID: req.SessionID, Kind: req.Kind, Origin: relayOrigin,
			Platform: req.Platform, Capabilities: req.Capabilities, Secret: req.SessionSecret})
		if err != nil {
			return refuse(relayError(err))
		}
		return protocol.Response{OK: true, Name: reg.Name, SessionSecret: reg.Secret}, true
	case protocol.KindEyes:
		// Only the launcher already holding this task's launch may spin its
		// child up: the session id names the task, auth_session_id and the
		// secret name the launcher, and the store proves the pairing.
		taskID, named := strings.CutPrefix(req.SessionID, eyesSessionPrefix)
		if !named || req.AuthSessionID == "" {
			return refuse("unauthorized")
		}
		task, err := g.st.EyesTask(taskID)
		if err != nil {
			return refuse(relayError(err))
		}
		// The child lives in the requesting workspace, not one the caller
		// picks, so a broker asking for any other scope is refused outright.
		if task.RequesterScope != req.Scope {
			return refuse("foreign session")
		}
		reg, err := g.st.ReissueEyesChild(taskID, req.AuthSessionID, req.SessionSecret)
		if err != nil {
			return refuse(relayError(err))
		}
		return protocol.Response{OK: true, Name: reg.Name, SessionSecret: reg.Secret}, true
	}
	return refuse("op not allowed on relay")
}

// relayError turns a store failure into the wire string the spec fixes. The
// sentinels already spell their own; only "no session" is normalized, because
// a caller must never learn whether the id or the secret was wrong.
func relayError(err error) string {
	if errors.Is(err, store.ErrNoSession) {
		return store.ErrRelayAuth.Error()
	}
	return err.Error()
}

package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

// hostScopePrefix marks the reserved broker scopes (host:<COMPUTERNAME>).
// They are plumbing, not workspaces, so the workspace directory hides them,
// ListEyes surfaces them instead, and only a launcher living in one can be
// handed a brief.
const hostScopePrefix = "host:"

var (
	// ErrNoSession: no agent row exists for that session id at all.
	ErrNoSession = errors.New("no session")
	// ErrForeignSession: the row exists but is not this caller's to act as -
	// it came in locally, or it names another scope, kind or origin.
	ErrForeignSession = errors.New("foreign session")
	// ErrRelayAuth: the presented session secret did not match.
	ErrRelayAuth = errors.New("unauthorized")
)

// secretHex returns n CSPRNG bytes as hex - the shape used for task ids and
// for the one secret the store still mints, an eyes child's.
func secretHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// validSecret is the shape every relay credential has: 32 CSPRNG bytes as
// lowercase hex. Clients mint their own before the first register, so the
// store's job is to insist on the shape rather than trust the length.
func validSecret(v string) bool {
	if len(v) != 64 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// sha256Hex is the only form a secret is ever stored in.
func sha256Hex(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// live reports whether a row is active or idle - the two states that mean
// somebody is there.
func (s *Store) live(explicit string, lastSeen int64) bool {
	st := s.freshStatus(explicit, lastSeen)
	return st == "active" || st == "idle"
}

// RelayRegistration is a registration that names its own role: a host broker
// claiming its launcher row, or an eyes child claiming a row in the
// requesting workspace. Secret re-claims a session that already exists.
type RelayRegistration struct {
	Scope, SessionID, Kind, Origin, Platform, Secret string
	Capabilities                                     []string
}

// RelayResult is the registered name plus a minted secret. Only
// ReissueEyesChild fills Secret in: a launcher brings its own, so a register
// answers with the name alone.
type RelayResult struct{ Name, Secret string }

// RegisterRelay registers a launcher. A relay session id names exactly ONE
// row: scope, kind and origin are fixed when it is created, and a re-register
// refreshes only platform and caps, so holding a secret can never move or
// clone an identity. The client mints its own secret before its first
// register and presents that same one on every retry - the store keeps only
// its sha256 - so a lost response costs a retry, never an identity. Eyes rows
// never pass through here at all: ReissueEyesChild is the only thing that
// creates or refreshes one, on the authority of the launcher holding its task.
func (s *Store) RegisterRelay(r RelayRegistration) (RelayResult, error) {
	switch r.Kind {
	case protocol.KindEyes:
		return RelayResult{}, ErrForeignSession
	case protocol.KindLauncher:
	default:
		return RelayResult{}, errors.New("register: kind must be eyes or launcher")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return RelayResult{}, err
	}
	defer tx.Rollback()
	// Read the prior row inside the tx: with the checks and the write in one
	// transaction, two racing registrations cannot both create this session.
	prior, err := s.agentBySession(tx, r.SessionID)
	switch {
	case errors.Is(err, ErrNoSession):
		if r.Origin == "relay" && !validSecret(r.Secret) {
			return RelayResult{}, ErrRelayAuth
		}
	case err != nil:
		return RelayResult{}, err
	case prior.Scope != r.Scope, prior.Origin != r.Origin, prior.Kind != "" && prior.Kind != r.Kind:
		return RelayResult{}, ErrForeignSession
	case prior.secretHash != "": // an existing relay row is re-claimed only by its holder
		if subtle.ConstantTimeCompare([]byte(sha256Hex(r.Secret)), []byte(prior.secretHash)) != 1 {
			return RelayResult{}, ErrRelayAuth
		}
	}
	caps := r.Capabilities
	if caps == nil {
		caps = []string{}
	}
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		return RelayResult{}, err
	}
	hash := prior.secretHash
	if hash == "" && r.Origin == "relay" {
		hash = sha256Hex(r.Secret)
	}
	source := "join"
	if r.Origin == "relay" {
		source = "relay"
	}
	name, err := s.register(tx, r.Scope, r.SessionID, source)
	if err != nil {
		return RelayResult{}, err
	}
	if _, err := tx.Exec(`UPDATE agents SET kind=?, origin=?, platform=?, caps=?, relay_secret_hash=?
		WHERE scope=? AND session_id=?`, r.Kind, r.Origin, r.Platform, string(capsJSON), hash,
		r.Scope, r.SessionID); err != nil {
		return RelayResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RelayResult{}, err
	}
	return RelayResult{Name: name}, nil
}

// ReissueEyesChild re-mints the child session of a live task on its assigned
// launcher's authority: session "eyes-<task_id>" in the task's requester
// scope, with a fresh secret that replaces any earlier one. Without it a lost
// register response strands the task - only the child may report, and only
// the launcher knows its secret. The launcher proves itself here, in the same
// transaction that mints the credential, so no gate check can go stale
// between the two.
func (s *Store) ReissueEyesChild(taskID, launcherSession, launcherSecret string) (RelayResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return RelayResult{}, err
	}
	defer tx.Rollback()
	t, err := s.eyesTask(tx, taskID)
	if err != nil {
		return RelayResult{}, err
	}
	if t.LauncherSession != launcherSession {
		return RelayResult{}, ErrNotYourTask
	}
	if t.State != "queued" && t.State != "accepted" {
		return RelayResult{}, ErrTaskNotLive
	}
	launcher, err := s.agentBySession(tx, launcherSession)
	if err != nil {
		return RelayResult{}, err
	}
	if launcher.Origin != "relay" || launcher.Kind != protocol.KindLauncher {
		return RelayResult{}, ErrForeignSession
	}
	if subtle.ConstantTimeCompare([]byte(sha256Hex(launcherSecret)), []byte(launcher.secretHash)) != 1 {
		return RelayResult{}, ErrRelayAuth
	}
	session := "eyes-" + taskID
	// The child row is the task's own or it is not touched: any row wearing
	// that session id which is not this relay's eyes row in the requester's
	// workspace is somebody else's identity, and taking it over would also
	// leave the session id naming two rows.
	var foreign int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agents WHERE session_id=?
		AND NOT (scope=? AND origin='relay' AND kind=?)`,
		session, t.RequesterScope, protocol.KindEyes).Scan(&foreign); err != nil {
		return RelayResult{}, err
	}
	if foreign > 0 {
		return RelayResult{}, ErrForeignSession
	}
	secret, err := secretHex(32)
	if err != nil {
		return RelayResult{}, err
	}
	name, err := s.register(tx, t.RequesterScope, session, "relay")
	if err != nil {
		return RelayResult{}, err
	}
	if _, err := tx.Exec(`UPDATE agents SET kind=?, origin='relay', relay_secret_hash=?
		WHERE scope=? AND session_id=?`, protocol.KindEyes, sha256Hex(secret), t.RequesterScope, session); err != nil {
		return RelayResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RelayResult{}, err
	}
	return RelayResult{Name: name, Secret: secret}, nil
}

// RelayIdentity is one agent row looked up by session id alone. secretHash
// stays unexported so no caller can move a stored hash around.
type RelayIdentity struct {
	Scope, Name, AgentID, Kind, Origin string
	secretHash                         string
}

// agentBySession finds a row by session id across scopes - the relay's one
// non-scoped identity read, because the gate must authenticate a caller
// before it knows which workspace answers for it. A relay row always wins:
// every caller of this is asking about a relay identity, and a local session
// that happens to share the id must not be able to shadow one. The rest of
// the order is fixed so a squatted id resolves the same way every time.
func (s *Store) agentBySession(q execQuerier, sessionID string) (RelayIdentity, error) {
	var id RelayIdentity
	err := q.QueryRow(`SELECT scope, name, agent_id, kind, origin, relay_secret_hash FROM agents
		WHERE session_id=? ORDER BY origin='relay' DESC, registered_at, scope LIMIT 1`, sessionID).
		Scan(&id.Scope, &id.Name, &id.AgentID, &id.Kind, &id.Origin, &id.secretHash)
	if err == sql.ErrNoRows {
		return RelayIdentity{}, ErrNoSession
	}
	return id, err
}

// VerifyRelaySecret authenticates a relay session: the row must exist, must
// have been created over the relay, and must match the presented secret. The
// gate calls this before it knows the caller's workspace, which is sound
// because the relay row is the one that answers (see agentBySession) - a
// local session sharing the id is reported as foreign, not authenticated in
// its place. The compare is constant time so a wrong secret leaks no prefix.
func (s *Store) VerifyRelaySecret(sessionID, secret string) (RelayIdentity, error) {
	id, err := s.agentBySession(s.db, sessionID)
	if err != nil {
		return RelayIdentity{}, err
	}
	if id.Origin != "relay" {
		return RelayIdentity{}, ErrForeignSession
	}
	if subtle.ConstantTimeCompare([]byte(sha256Hex(secret)), []byte(id.secretHash)) != 1 {
		return RelayIdentity{}, ErrRelayAuth
	}
	return id, nil
}

// ListWorkspaces is the occupancy directory: one row per scope with a live
// agent, counted by kind. Reserved host: scopes are hidden - they are broker
// plumbing, and ListEyes is where they belong.
func (s *Store) ListWorkspaces() ([]protocol.WorkspaceInfo, error) {
	rows, err := s.db.Query(`SELECT scope, status, last_seen, kind FROM agents ORDER BY scope, registered_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byScope := map[string]*protocol.WorkspaceInfo{}
	var order []string
	for rows.Next() {
		var scope, explicit, kind string
		var seen int64
		if err := rows.Scan(&scope, &explicit, &seen, &kind); err != nil {
			return nil, err
		}
		if strings.HasPrefix(scope, hostScopePrefix) || !s.live(explicit, seen) {
			continue
		}
		w := byScope[scope]
		if w == nil {
			w = &protocol.WorkspaceInfo{Scope: scope}
			byScope[scope] = w
			order = append(order, scope)
		}
		w.LiveAgents++
		switch kind {
		case protocol.KindEyes:
			w.EyesAgents++
		case protocol.KindLauncher:
			w.LauncherAgents++
		}
		if seen > w.LastSeen {
			w.LastSeen = seen
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]protocol.WorkspaceInfo, 0, len(order))
	for _, scope := range order {
		out = append(out, *byScope[scope])
	}
	return out, nil
}

// ListEyes lists the live host-side agents across every workspace - the
// brokers and the eyes children they run - so one call answers both "is a
// broker up?" and "is a child already running?". An empty list means the
// broker is not connected.
func (s *Store) ListEyes() ([]protocol.AgentInfo, error) {
	rows, err := s.db.Query(`SELECT scope, agent_id, name, status, last_seen, kind, origin, platform, caps
		FROM agents WHERE kind IN (?,?) ORDER BY scope, registered_at`,
		protocol.KindEyes, protocol.KindLauncher)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.AgentInfo
	for rows.Next() {
		var a protocol.AgentInfo
		var explicit, capsJSON string
		if err := rows.Scan(&a.Scope, &a.AgentID, &a.Name, &explicit, &a.LastSeen,
			&a.Kind, &a.Origin, &a.Platform, &capsJSON); err != nil {
			return nil, err
		}
		if !s.live(explicit, a.LastSeen) {
			continue
		}
		a.Status = s.freshStatus(explicit, a.LastSeen)
		json.Unmarshal([]byte(capsJSON), &a.Capabilities)
		out = append(out, a)
	}
	return out, rows.Err()
}

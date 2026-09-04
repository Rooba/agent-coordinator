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
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

// hostScopePrefix marks the reserved broker scopes (host:<COMPUTERNAME>).
// They are plumbing, not workspaces, so the workspace directory hides them
// and ListEyes surfaces them instead.
const hostScopePrefix = "host:"

// launcherWindow is how stale a launcher may be and still be handed a task.
// A healthy launcher polls its inbox at least every 5s, so 30s means "still
// polling" - much tighter than the 2 minute presence window.
const launcherWindow = 30 * time.Second

var (
	// ErrNoSession: no agent row exists for that session id at all.
	ErrNoSession = errors.New("no session")
	// ErrForeignSession: the row exists but did not come in over the relay,
	// so no relay client may act as it.
	ErrForeignSession = errors.New("foreign session")
	// ErrRelayAuth: the presented session secret did not match.
	ErrRelayAuth = errors.New("unauthorized")
	// ErrNoLauncher: no host broker is polling right now.
	ErrNoLauncher = errors.New("no host launcher")
	// ErrEyesBusy: a broker is polling, but every one is already running a job.
	ErrEyesBusy = errors.New("eyes busy")
	// ErrUnknownTask / ErrNotYourTask guard the eyes task ledger.
	ErrUnknownTask = errors.New("unknown task")
	ErrNotYourTask = errors.New("not your task")
)

// secretHex returns n CSPRNG bytes as hex - the shape used for per-session
// relay secrets.
func secretHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
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

// RelayResult is the registered name plus the one-time TCP secret. Secret is
// empty for a local registration and for a re-register: it is minted once,
// when the relay row is created.
type RelayResult struct{ Name, Secret string }

// RegisterRelay registers a kind-bearing agent. Such a row is always its own
// identity - it never binds to a hook agent - and a row created over the
// relay gets a secret whose sha256 is all the store keeps. A broker keeps one
// session across reconnects, so re-registering means presenting that secret.
func (s *Store) RegisterRelay(r RelayRegistration) (RelayResult, error) {
	if r.Kind != protocol.KindEyes && r.Kind != protocol.KindLauncher {
		return RelayResult{}, errors.New("register: kind must be eyes or launcher")
	}
	prior, err := s.AgentBySession(r.SessionID)
	switch {
	case errors.Is(err, ErrNoSession): // a brand new session needs no proof
	case err != nil:
		return RelayResult{}, err
	case prior.secretHash != "": // an existing relay row is re-claimed only by its holder
		if _, err := s.VerifyRelaySecret(r.SessionID, r.Secret); err != nil {
			return RelayResult{}, err
		}
	case r.Origin == "relay": // a session minted locally is not the relay's to take
		return RelayResult{}, ErrForeignSession
	}
	source := "join"
	if r.Origin == "relay" {
		source = "relay"
	}
	name, err := s.Register(r.Scope, r.SessionID, source)
	if err != nil {
		return RelayResult{}, err
	}
	caps := r.Capabilities
	if caps == nil {
		caps = []string{}
	}
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		return RelayResult{}, err
	}
	secret, hash := "", prior.secretHash
	if r.Origin == "relay" && hash == "" {
		if secret, err = secretHex(32); err != nil {
			return RelayResult{}, err
		}
		hash = sha256Hex(secret)
	}
	if _, err := s.db.Exec(`UPDATE agents SET kind=?, origin=?, platform=?, caps=?, relay_secret_hash=?
		WHERE scope=? AND session_id=?`, r.Kind, r.Origin, r.Platform, string(capsJSON), hash,
		r.Scope, r.SessionID); err != nil {
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

// AgentBySession finds a row by session id across scopes - the relay's one
// non-scoped identity read. A relay session id is minted per broker and per
// child, so it names exactly one row.
func (s *Store) AgentBySession(sessionID string) (RelayIdentity, error) {
	var id RelayIdentity
	err := s.db.QueryRow(`SELECT scope, name, agent_id, kind, origin, relay_secret_hash FROM agents
		WHERE session_id=? ORDER BY registered_at LIMIT 1`, sessionID).
		Scan(&id.Scope, &id.Name, &id.AgentID, &id.Kind, &id.Origin, &id.secretHash)
	if err == sql.ErrNoRows {
		return RelayIdentity{}, ErrNoSession
	}
	return id, err
}

// VerifyRelaySecret authenticates a relay session: the row must exist, must
// have been created over the relay, and must match the presented secret.
// The compare is constant time so a wrong secret leaks no prefix.
func (s *Store) VerifyRelaySecret(sessionID, secret string) (RelayIdentity, error) {
	id, err := s.AgentBySession(sessionID)
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

// LauncherRef addresses the host broker chosen for a task.
type LauncherRef struct{ SessionID, Scope, Name, AgentID string }

// PickLauncher returns a launcher that is still polling and not already
// running a job. The freshness window is 30s rather than the 2 minute
// presence window, because a launcher that went quiet cannot pick a task up.
func (s *Store) PickLauncher() (LauncherRef, error) {
	var l LauncherRef
	var busy bool
	// Free launchers sort first, so a busy row coming back means every live
	// launcher is busy - which the caller must report differently from "none".
	err := s.db.QueryRow(`SELECT a.session_id, a.scope, a.name, a.agent_id,
		EXISTS(SELECT 1 FROM eyes_tasks t WHERE t.launcher_session = a.session_id
		       AND t.state IN ('queued','accepted')) AS busy
		FROM agents a WHERE a.kind=? AND a.status != 'gone' AND a.last_seen >= ?
		ORDER BY busy, a.last_seen DESC LIMIT 1`,
		protocol.KindLauncher, s.Now().Add(-launcherWindow).Unix()).
		Scan(&l.SessionID, &l.Scope, &l.Name, &l.AgentID, &busy)
	switch {
	case err == sql.ErrNoRows:
		return LauncherRef{}, ErrNoLauncher
	case err != nil:
		return LauncherRef{}, err
	case busy:
		return LauncherRef{}, ErrEyesBusy
	}
	return l, nil
}

// EyesTask is one requested host-eyes job. The ledger exists so cancel
// authorization and launch idempotency never depend on scanning inboxes.
type EyesTask struct {
	TaskID, RequesterScope, RequesterAgentID, LauncherSession, Runtime, State string
	CreatedAt, UpdatedAt                                                      int64
}

// CreateEyesTask records a newly minted task as queued and reports whether
// this call is the one that created it. task_id is the primary key, so a
// repeat is not a second launch: the live row comes back with created false
// and the caller must not send another task.launch - browser actions have
// side effects.
func (s *Store) CreateEyesTask(t EyesTask) (EyesTask, bool, error) {
	now := s.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO eyes_tasks
		(task_id, requester_scope, requester_agent_id, launcher_session, runtime, state, created_at, updated_at)
		VALUES (?,?,?,?,?,'queued',?,?) ON CONFLICT(task_id) DO NOTHING`,
		t.TaskID, t.RequesterScope, t.RequesterAgentID, t.LauncherSession, t.Runtime, now, now)
	if err != nil {
		return EyesTask{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		live, err := s.EyesTask(t.TaskID)
		return live, false, err
	}
	t.State, t.CreatedAt, t.UpdatedAt = "queued", now, now
	return t, true, nil
}

// EyesTask reads one task; an id nobody minted is ErrUnknownTask.
func (s *Store) EyesTask(taskID string) (EyesTask, error) {
	var t EyesTask
	err := s.db.QueryRow(`SELECT task_id, requester_scope, requester_agent_id, launcher_session,
		runtime, state, created_at, updated_at FROM eyes_tasks WHERE task_id=?`, taskID).
		Scan(&t.TaskID, &t.RequesterScope, &t.RequesterAgentID, &t.LauncherSession,
			&t.Runtime, &t.State, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return EyesTask{}, ErrUnknownTask
	}
	return t, err
}

// SetEyesTaskState advances a task through queued|accepted|done|failed|cancelled.
func (s *Store) SetEyesTaskState(taskID, state string) error {
	res, err := s.db.Exec(`UPDATE eyes_tasks SET state=?, updated_at=? WHERE task_id=?`,
		state, s.Now().Unix(), taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUnknownTask
	}
	return nil
}

// CancelEyesTask authorizes a cancel against the recorded requester and
// marks the task cancelled, returning it so the caller can tell the launcher.
func (s *Store) CancelEyesTask(taskID string, caller protocol.AgentRef) (EyesTask, error) {
	t, err := s.EyesTask(taskID)
	if err != nil {
		return EyesTask{}, err
	}
	if !s.actsFor(t.RequesterScope, caller.AgentID, t.RequesterAgentID) {
		return EyesTask{}, ErrNotYourTask
	}
	if err := s.SetEyesTaskState(taskID, "cancelled"); err != nil {
		return EyesTask{}, err
	}
	t.State = "cancelled"
	return t, nil
}

// actsFor reports whether agentID is the owner or one of the owner's bound
// children: a subagent may cancel what its parent asked for.
func (s *Store) actsFor(scope, agentID, ownerID string) bool {
	if agentID == ownerID {
		return true
	}
	var parentID string
	err := s.db.QueryRow(`SELECT p.agent_id FROM agents c
		JOIN agents p ON p.scope = c.scope AND p.session_id = c.parent_session_id
		WHERE c.scope=? AND c.agent_id=?`, scope, agentID).Scan(&parentID)
	return err == nil && parentID == ownerID
}

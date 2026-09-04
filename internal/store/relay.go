package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

// hostScopePrefix marks the reserved broker scopes (host:<COMPUTERNAME>).
// They are plumbing, not workspaces, so the workspace directory hides them,
// ListEyes surfaces them instead, and only a launcher living in one can be
// handed a brief.
const hostScopePrefix = "host:"

const (
	// launcherWindow is how stale a launcher may be and still be handed a
	// task. A healthy launcher polls its inbox at least every 5s, so 30s
	// means "still polling" - much tighter than the 2 minute presence window.
	launcherWindow = 30 * time.Second
	// staleTaskWindow is how long an unfinished task keeps its launcher
	// reserved. Past it the broker has died or lost the job, and the ledger
	// row must not hold the host out of service until the 24h purge.
	staleTaskWindow = 30 * time.Minute
	// The requester's bound on one host job, applied where the launch body is
	// built so every caller gets the same rule.
	eyesDeadlineDefault = 300
	eyesDeadlineMax     = 1800
)

var (
	// ErrNoSession: no agent row exists for that session id at all.
	ErrNoSession = errors.New("no session")
	// ErrForeignSession: the row exists but is not this caller's to act as -
	// it came in locally, or it names another scope, kind or origin.
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
	// ErrTaskNotLive: the task has settled, so it can gain no child.
	ErrTaskNotLive = errors.New("task is not live")
	// ErrBadTransition: the requested state move is not one the lifecycle has.
	ErrBadTransition = errors.New("invalid task transition")
)

// secretHex returns n CSPRNG bytes as hex - the shape used for per-session
// relay secrets and for task ids.
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

// RegisterRelay registers a kind-bearing agent. A relay session id names
// exactly ONE row: scope, kind and origin are fixed when it is created, and a
// re-register refreshes only platform and caps, so holding a secret can never
// move or clone an identity. Row and metadata are written in one transaction
// - a half-registered row could never be re-claimed. A row created over the
// relay gets a secret whose sha256 is all the store keeps; re-registering it
// means presenting that secret, which is how a restarted broker resumes.
func (s *Store) RegisterRelay(r RelayRegistration) (RelayResult, error) {
	if r.Kind != protocol.KindEyes && r.Kind != protocol.KindLauncher {
		return RelayResult{}, errors.New("register: kind must be eyes or launcher")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return RelayResult{}, err
	}
	defer tx.Rollback()
	// Read the prior row inside the tx: with the checks and the write in one
	// transaction, two racing registrations cannot both believe they minted
	// this session's secret.
	prior, err := s.agentBySession(tx, r.SessionID)
	switch {
	case errors.Is(err, ErrNoSession): // a brand new session needs no proof
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
	secret, hash := "", prior.secretHash
	if r.Origin == "relay" && hash == "" {
		if secret, err = secretHex(32); err != nil {
			return RelayResult{}, err
		}
		hash = sha256Hex(secret)
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
	return RelayResult{Name: name, Secret: secret}, nil
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
	// The child belongs to the requester's workspace and nowhere else; a row
	// for that session in another scope is somebody else's identity.
	var scope string
	switch err := tx.QueryRow(`SELECT scope FROM agents WHERE session_id=?`, session).Scan(&scope); {
	case err == sql.ErrNoRows:
	case err != nil:
		return RelayResult{}, err
	case scope != t.RequesterScope:
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

// AgentBySession finds a row by session id across scopes - the relay's one
// non-scoped identity read. A relay session id is minted per broker and per
// child and cannot change scope, so it names exactly one row.
func (s *Store) AgentBySession(sessionID string) (RelayIdentity, error) {
	return s.agentBySession(s.db, sessionID)
}

func (s *Store) agentBySession(q execQuerier, sessionID string) (RelayIdentity, error) {
	var id RelayIdentity
	err := q.QueryRow(`SELECT scope, name, agent_id, kind, origin, relay_secret_hash FROM agents
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

// PickLauncher returns a broker that is still polling and not already running
// a job. A broker is a relay-registered launcher in a reserved host: scope -
// a local agent calling itself a launcher is not one. The freshness window is
// 30s rather than the 2 minute presence window, because a launcher that went
// quiet cannot pick a task up.
func (s *Store) PickLauncher() (LauncherRef, error) {
	return s.pickLauncher(s.db)
}

func (s *Store) pickLauncher(q execQuerier) (LauncherRef, error) {
	var l LauncherRef
	var busy bool
	now := s.Now()
	// Free launchers sort first, so a busy row coming back means every live
	// launcher is busy - which the caller must report differently from "none".
	// A task nobody has advanced for staleTaskWindow is abandoned, not in
	// flight, and stops reserving its launcher. substr rather than LIKE keeps
	// host: one case-sensitive rule, the same one ListWorkspaces hides by.
	err := q.QueryRow(`SELECT a.session_id, a.scope, a.name, a.agent_id,
		EXISTS(SELECT 1 FROM eyes_tasks t WHERE t.launcher_session = a.session_id
		       AND t.state IN ('queued','accepted') AND t.updated_at >= ?) AS busy
		FROM agents a WHERE a.kind=? AND a.origin='relay' AND substr(a.scope,1,?)=?
		AND a.status != 'gone' AND a.last_seen >= ?
		ORDER BY busy, a.last_seen DESC LIMIT 1`,
		now.Add(-staleTaskWindow).Unix(), protocol.KindLauncher,
		len(hostScopePrefix), hostScopePrefix, now.Add(-launcherWindow).Unix()).
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

// EyesRequest is one request_eyes: who is asking (stamped by the daemon from
// the authenticated session), what to run and how long it may take.
type EyesRequest struct {
	Requester protocol.AgentRef
	Runtime   string
	Brief     string
	DeadlineS int
}

// AssignEyesTask reserves a launcher, queues the task and delivers
// task.launch to that launcher in ONE transaction, so two concurrent requests
// cannot take the same broker and a queued task always has the mail that
// starts it. The task id is minted here, so there is no id a caller could
// replay: every accepted call is a new task by construction.
func (s *Store) AssignEyesTask(req EyesRequest) (EyesTask, protocol.AgentRef, error) {
	suffix, err := secretHex(6) // "task-" + 12 lowercase hex
	if err != nil {
		return EyesTask{}, protocol.AgentRef{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return EyesTask{}, protocol.AgentRef{}, err
	}
	defer tx.Rollback()
	l, err := s.pickLauncher(tx)
	if err != nil {
		return EyesTask{}, protocol.AgentRef{}, err
	}
	now := s.Now().Unix()
	t := EyesTask{TaskID: "task-" + suffix, RequesterScope: req.Requester.Scope,
		RequesterAgentID: req.Requester.AgentID, LauncherSession: l.SessionID,
		Runtime: req.Runtime, State: "queued", CreatedAt: now, UpdatedAt: now}
	if _, err := tx.Exec(`INSERT INTO eyes_tasks
		(task_id, requester_scope, requester_agent_id, launcher_session, runtime, state, created_at, updated_at)
		VALUES (?,?,?,?,?,'queued',?,?)`,
		t.TaskID, t.RequesterScope, t.RequesterAgentID, t.LauncherSession, t.Runtime, now, now); err != nil {
		return EyesTask{}, protocol.AgentRef{}, err
	}
	body, err := json.Marshal(protocol.TaskLaunchMsg{Type: protocol.TaskLaunch, TaskID: t.TaskID,
		Runtime: req.Runtime, Scope: req.Requester.Scope, Brief: req.Brief,
		ReplyTo: req.Requester, DeadlineS: eyesDeadline(req.DeadlineS)})
	if err != nil {
		return EyesTask{}, protocol.AgentRef{}, err
	}
	if err := s.sendToScope(tx, Delivery{FromScope: req.Requester.Scope, FromName: senderKey(req.Requester),
		ToScope: l.Scope, ToName: l.AgentID, Body: string(body), ReplyTo: &req.Requester,
		TaskID: t.TaskID}); err != nil {
		return EyesTask{}, protocol.AgentRef{}, err
	}
	if err := tx.Commit(); err != nil {
		return EyesTask{}, protocol.AgentRef{}, err
	}
	return t, protocol.AgentRef{Name: l.Name, AgentID: l.AgentID, Scope: l.Scope}, nil
}

// senderKey addresses an agent by its id when there is one - the name is the
// fallback for refs a caller built by name.
func senderKey(ref protocol.AgentRef) string {
	if ref.AgentID != "" {
		return ref.AgentID
	}
	return ref.Name
}

// eyesDeadline bounds one host job: 300s unless asked otherwise, 1800s max.
func eyesDeadline(d int) int {
	switch {
	case d <= 0:
		return eyesDeadlineDefault
	case d > eyesDeadlineMax:
		return eyesDeadlineMax
	}
	return d
}

// EyesTask reads one task; an id nobody minted is ErrUnknownTask.
func (s *Store) EyesTask(taskID string) (EyesTask, error) {
	return s.eyesTask(s.db, taskID)
}

func (s *Store) eyesTask(q execQuerier, taskID string) (EyesTask, error) {
	var t EyesTask
	err := q.QueryRow(`SELECT task_id, requester_scope, requester_agent_id, launcher_session,
		runtime, state, created_at, updated_at FROM eyes_tasks WHERE task_id=?`, taskID).
		Scan(&t.TaskID, &t.RequesterScope, &t.RequesterAgentID, &t.LauncherSession,
			&t.Runtime, &t.State, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return EyesTask{}, ErrUnknownTask
	}
	return t, err
}

// TransitionEyesTask applies one validated lifecycle move and tells the
// requester in the SAME transaction, so a re-sent report can never leave a
// state change without its mail or add a second copy of it. The edges are
// queued -> accepted by the assigned launcher, and queued|accepted -> done by
// the child ("eyes-" + task_id) or -> failed by either. A repeat of a move
// already made returns ok=false and writes nothing.
func (s *Store) TransitionEyesTask(taskID, actorSession, to, body string) (EyesTask, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return EyesTask{}, false, err
	}
	defer tx.Rollback()
	t, err := s.eyesTask(tx, taskID)
	if err != nil {
		return EyesTask{}, false, err
	}
	// Who may make this move at all: accepted is the assigned launcher's ack,
	// done is the child's report, and either of them may fail the task.
	child := "eyes-" + taskID
	var mayAct bool
	switch to {
	case "accepted":
		mayAct = actorSession == t.LauncherSession
	case "done":
		mayAct = actorSession == child
	case "failed":
		mayAct = actorSession == child || actorSession == t.LauncherSession
	default:
		return EyesTask{}, false, fmt.Errorf("%w: %q is not a task state", ErrBadTransition, to)
	}
	if !mayAct {
		return EyesTask{}, false, ErrNotYourTask
	}
	if t.State == to {
		return t, false, nil // the ack was lost, not the transition
	}
	// queued -> accepted -> done|failed, plus queued -> failed for a launcher
	// that never accepted. A settled task moves nowhere.
	if live := t.State == "queued" || t.State == "accepted"; !live || (to == "accepted" && t.State != "queued") {
		return EyesTask{}, false, fmt.Errorf("%w: %s -> %s", ErrBadTransition, t.State, to)
	}
	actor, err := s.agentBySession(tx, actorSession)
	if err != nil {
		return EyesTask{}, false, err
	}
	now := s.Now().Unix()
	if _, err := tx.Exec(`UPDATE eyes_tasks SET state=?, updated_at=? WHERE task_id=?`, to, now, taskID); err != nil {
		return EyesTask{}, false, err
	}
	ref := protocol.AgentRef{Name: actor.Name, AgentID: actor.AgentID, Scope: actor.Scope}
	if err := s.sendToScope(tx, Delivery{FromScope: actor.Scope, FromName: senderKey(ref),
		ToScope: t.RequesterScope, ToName: t.RequesterAgentID, Body: body, ReplyTo: &ref,
		TaskID: taskID}); err != nil {
		return EyesTask{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return EyesTask{}, false, err
	}
	t.State, t.UpdatedAt = to, now
	return t, true, nil
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
	t.UpdatedAt = s.Now().Unix()
	if _, err := s.db.Exec(`UPDATE eyes_tasks SET state='cancelled', updated_at=? WHERE task_id=?`,
		t.UpdatedAt, taskID); err != nil {
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

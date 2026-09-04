package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

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
	// ErrNoLauncher: no host broker is polling right now.
	ErrNoLauncher = errors.New("no host launcher")
	// ErrNoProvider: brokers are polling, but none runs the asked-for runtime.
	ErrNoProvider = errors.New("no matching provider")
	// ErrEyesBusy: a broker is polling, but every one is already running a job.
	ErrEyesBusy = errors.New("eyes busy")
	// ErrUnknownTask / ErrNotYourTask guard the eyes task ledger.
	ErrUnknownTask = errors.New("unknown task")
	ErrNotYourTask = errors.New("not your task")
	// ErrTaskNotLive: the task has settled, so it can gain no child.
	ErrTaskNotLive = errors.New("task is not live")
	// ErrBadTransition: the requested state move is not one the lifecycle has.
	ErrBadTransition = errors.New("invalid task transition")
	// ErrBadRuntime: the asked-for runtime is not shaped like a provider name.
	ErrBadRuntime = errors.New("invalid runtime")
)

// LauncherRef addresses the host broker chosen for a task. caps stays
// unexported: it is the raw JSON the pick matched on, read here only to name
// the runtime an unqualified brief will run.
type LauncherRef struct {
	SessionID, Scope, Name, AgentID string
	caps                            string
}

// providerOrder is the fixed preference for a brief that named no runtime, so
// the same broker always answers with the same provider.
var providerOrder = []string{"claude", "codex", "grok"}

// providerCap is how a broker advertises a runtime: quoted so a match hits a
// whole JSON array element rather than a prefix of one.
func providerCap(runtime string) string { return `"provider.` + runtime + `"` }

// defaultProvider is the runtime an unqualified brief runs on: the first
// provider this broker advertises, in providerOrder.
func defaultProvider(caps string) string {
	for _, p := range providerOrder {
		if strings.Contains(caps, providerCap(p)) {
			return p
		}
	}
	return ""
}

// validRuntime keeps a runtime a bare provider name: it reaches a capability
// match and the task ledger, so nothing else may hide in it. Empty is the
// caller asking for whatever the host runs.
func validRuntime(r string) bool {
	if len(r) > 32 {
		return false
	}
	for i := 0; i < len(r); i++ {
		c := r[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// PickLauncher returns any broker that is still polling and not already
// running a job. A broker is a relay-registered launcher in a reserved host:
// scope - a local agent calling itself a launcher is not one.
func (s *Store) PickLauncher() (LauncherRef, error) {
	return s.pickLauncher(s.db, "")
}

// pickLauncher takes the best live broker for a runtime, and its three
// outcomes are three different answers for the requester: nobody is polling,
// nobody runs that provider, or everyone who does is busy. One query decides
// all three because the rows sort provider-match first, then free first.
func (s *Store) pickLauncher(q execQuerier, runtime string) (LauncherRef, error) {
	var l LauncherRef
	var matches, busy bool
	now := s.Now()
	// A runtime is served by the capability "provider.<runtime>"; a brief that
	// named none still needs a broker with some provider, so the needle drops
	// to the prefix every provider cap shares.
	needle := `"provider.`
	if runtime != "" {
		needle = providerCap(runtime)
	}
	// A task nobody has advanced for staleTaskWindow is abandoned, not in
	// flight, and stops reserving its launcher. substr rather than LIKE keeps
	// host: one case-sensitive rule, the same one ListWorkspaces hides by.
	err := q.QueryRow(`SELECT a.session_id, a.scope, a.name, a.agent_id, a.caps,
		instr(a.caps, ?) > 0 AS matches,
		EXISTS(SELECT 1 FROM eyes_tasks t WHERE t.launcher_session = a.session_id
		       AND t.state IN ('queued','accepted') AND t.updated_at >= ?) AS busy
		FROM agents a WHERE a.kind=? AND a.origin='relay' AND substr(a.scope,1,?)=?
		AND a.status != 'gone' AND a.last_seen >= ?
		ORDER BY matches DESC, busy, a.last_seen DESC LIMIT 1`,
		needle, now.Add(-staleTaskWindow).Unix(), protocol.KindLauncher,
		len(hostScopePrefix), hostScopePrefix, now.Add(-launcherWindow).Unix()).
		Scan(&l.SessionID, &l.Scope, &l.Name, &l.AgentID, &l.caps, &matches, &busy)
	switch {
	case err == sql.ErrNoRows:
		return LauncherRef{}, ErrNoLauncher
	case err != nil:
		return LauncherRef{}, err
	case !matches:
		return LauncherRef{}, ErrNoProvider
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
// replay: every accepted call is a new task by construction. A brief that
// named no runtime gets the broker's first advertised provider, recorded on
// the task so nothing downstream has to guess.
func (s *Store) AssignEyesTask(req EyesRequest) (EyesTask, protocol.AgentRef, error) {
	if !validRuntime(req.Runtime) {
		return EyesTask{}, protocol.AgentRef{}, fmt.Errorf("%w: %q", ErrBadRuntime, req.Runtime)
	}
	suffix, err := secretHex(6) // "task-" + 12 lowercase hex
	if err != nil {
		return EyesTask{}, protocol.AgentRef{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return EyesTask{}, protocol.AgentRef{}, err
	}
	defer tx.Rollback()
	l, err := s.pickLauncher(tx, req.Runtime)
	if err != nil {
		return EyesTask{}, protocol.AgentRef{}, err
	}
	// A brief that named no runtime still runs on one, so the task records
	// which provider the chosen broker will use.
	runtime := req.Runtime
	if runtime == "" {
		if runtime = defaultProvider(l.caps); runtime == "" {
			return EyesTask{}, protocol.AgentRef{}, ErrNoProvider
		}
	}
	now := s.Now().Unix()
	t := EyesTask{TaskID: "task-" + suffix, RequesterScope: req.Requester.Scope,
		RequesterAgentID: req.Requester.AgentID, LauncherSession: l.SessionID,
		Runtime: runtime, State: "queued", CreatedAt: now, UpdatedAt: now}
	if _, err := tx.Exec(`INSERT INTO eyes_tasks
		(task_id, requester_scope, requester_agent_id, launcher_session, runtime, state, created_at, updated_at)
		VALUES (?,?,?,?,?,'queued',?,?)`,
		t.TaskID, t.RequesterScope, t.RequesterAgentID, t.LauncherSession, t.Runtime, now, now); err != nil {
		return EyesTask{}, protocol.AgentRef{}, err
	}
	body, err := json.Marshal(protocol.TaskLaunchMsg{Type: protocol.TaskLaunch, TaskID: t.TaskID,
		Runtime: runtime, Scope: req.Requester.Scope, Brief: req.Brief,
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

// PendingLaunches lists the launch mail of every task still queued on this
// broker, oldest task first, whether or not the broker already read it.
// Delivery is at-least-once on purpose: a broker that lost a read asks again,
// and dropping a duplicate by task id is its job. Only launch bodies count -
// other mail about the same task is not work to run - and one launcher
// session resolves to one row, so a squatted id cannot fan the queue out.
func (s *Store) PendingLaunches(launcherSession string) ([]protocol.Message, error) {
	rows, err := s.db.Query(`
		SELECT m.id, COALESCE(a.name, m.from_agent), m.body, m.created_at,
		       m.reply_to, m.from_scope, m.kind, m.task_id
		FROM eyes_tasks t
		JOIN agents l ON l.rowid = (SELECT l2.rowid FROM agents l2
			WHERE l2.session_id = t.launcher_session ORDER BY l2.registered_at, l2.scope LIMIT 1)
		JOIN messages m ON m.task_id = t.task_id AND m.scope = l.scope AND m.to_agent = l.agent_id
			AND m.body LIKE '{"type":"task.launch"%'
		LEFT JOIN agents a ON a.scope = COALESCE(NULLIF(m.from_scope, ''), m.scope) AND a.agent_id = m.from_agent
		WHERE t.launcher_session = ? AND t.state = 'queued'
		ORDER BY t.created_at, m.id`, launcherSession)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.Message
	for rows.Next() {
		var m protocol.Message
		var replyTo string
		if err := rows.Scan(&m.ID, &m.From, &m.Body, &m.SentAt, &replyTo,
			&m.FromScope, &m.Kind, &m.TaskID); err != nil {
			return nil, err
		}
		m.ReplyTo = replyRef(replyTo)
		out = append(out, m)
	}
	return out, rows.Err()
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

// eyesRole is which side of a task an actor speaks for.
type eyesRole string

const (
	roleLauncher  eyesRole = "launcher"
	roleChild     eyesRole = "child"
	roleRequester eyesRole = "requester"
)

// eyesEdges is the whole task lifecycle in one table: per target state, which
// role may ask for it and from which states. The launcher acks a brief and
// may give up on one it never accepted; once it has accepted, its child owns
// the outcome; the requester's side may call the whole thing off while it is
// still live. Anything absent here is not a transition.
var eyesEdges = map[string]map[eyesRole][]string{
	"accepted":  {roleLauncher: {"queued"}},
	"done":      {roleChild: {"accepted"}},
	"failed":    {roleChild: {"queued", "accepted"}, roleLauncher: {"queued"}},
	"cancelled": {roleRequester: {"queued", "accepted"}},
}

// eyesActor is how a caller proves which side it is: a relay session id (the
// assigned launcher, or the task's child) or a local agent ref (the requester
// or one of its bound children).
type eyesActor struct {
	Session string
	Ref     protocol.AgentRef
}

// TransitionEyesTask applies one lifecycle move made by a relay session - the
// launcher's ack, or the child's report - and mails body to the requester.
func (s *Store) TransitionEyesTask(taskID, actorSession, to, body string) (EyesTask, bool, error) {
	return s.moveEyesTask(taskID, to, eyesActor{Session: actorSession}, body)
}

// CancelEyesTask is the requester's own move: it settles the task and tells
// the broker holding it. Cancelling an already cancelled task is a no-op, so
// a retried cancel_eyes never sends a second task.cancel.
func (s *Store) CancelEyesTask(taskID string, caller protocol.AgentRef) (EyesTask, error) {
	t, _, err := s.moveEyesTask(taskID, "cancelled", eyesActor{Ref: caller}, "")
	return t, err
}

// moveEyesTask is the ONE write path for the ledger: authorize the actor's
// role against the edge table, then write the new state and the message that
// move implies in a single transaction - so a state change never exists
// without its mail, and a repeat adds neither.
func (s *Store) moveEyesTask(taskID, to string, a eyesActor, msg string) (EyesTask, bool, error) {
	edge, ok := eyesEdges[to]
	if !ok {
		return EyesTask{}, false, fmt.Errorf("%w: %q is not a task state", ErrBadTransition, to)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return EyesTask{}, false, err
	}
	defer tx.Rollback()
	t, err := s.eyesTask(tx, taskID)
	if err != nil {
		return EyesTask{}, false, err
	}
	role := s.eyesRole(tx, t, a)
	from, ok := edge[role]
	if !ok {
		return EyesTask{}, false, ErrNotYourTask
	}
	if t.State == to {
		return t, false, nil // the ack was lost, not the transition
	}
	if !slices.Contains(from, t.State) {
		return EyesTask{}, false, fmt.Errorf("%w: %s -> %s", ErrBadTransition, t.State, to)
	}
	d, deliver, err := s.eyesMail(tx, t, role, a, msg)
	if err != nil {
		return EyesTask{}, false, err
	}
	now := s.Now().Unix()
	if _, err := tx.Exec(`UPDATE eyes_tasks SET state=?, updated_at=? WHERE task_id=?`, to, now, taskID); err != nil {
		return EyesTask{}, false, err
	}
	if deliver {
		if err := s.sendToScope(tx, d); err != nil {
			return EyesTask{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return EyesTask{}, false, err
	}
	t.State, t.UpdatedAt = to, now
	return t, true, nil
}

// eyesRole says which side of this task the actor is, or "" for a caller with
// no standing in it.
func (s *Store) eyesRole(q execQuerier, t EyesTask, a eyesActor) eyesRole {
	switch {
	case a.Session == "":
		if s.actsFor(q, t.RequesterScope, a.Ref.AgentID, t.RequesterAgentID) {
			return roleRequester
		}
	case a.Session == "eyes-"+t.TaskID:
		return roleChild
	case a.Session == t.LauncherSession:
		return roleLauncher
	}
	return ""
}

// eyesMail addresses the message a move implies: a cancel goes to the broker
// holding the job and carries its own task.cancel body, while an ack or a
// report goes back to the requester carrying what the actor sent. It reports
// false when there is nobody left to tell - an agent row is purged hours
// before its task is, and that must not block the move.
func (s *Store) eyesMail(q execQuerier, t EyesTask, role eyesRole, a eyesActor, msg string) (Delivery, bool, error) {
	if role == roleRequester {
		l, err := s.agentBySession(q, t.LauncherSession)
		if errors.Is(err, ErrNoSession) {
			return Delivery{}, false, nil // the broker's row is gone: nobody to tell
		}
		if err != nil {
			return Delivery{}, false, err
		}
		cancel, err := json.Marshal(protocol.TaskCancelMsg{Type: protocol.TaskCancel, TaskID: t.TaskID})
		if err != nil {
			return Delivery{}, false, err
		}
		// The canceller lives in the requester's workspace by construction -
		// that is what made it the requester's side in the first place.
		ref := protocol.AgentRef{Name: a.Ref.Name, AgentID: a.Ref.AgentID, Scope: t.RequesterScope}
		return Delivery{FromScope: t.RequesterScope, FromName: senderKey(ref), ToScope: l.Scope,
			ToName: l.AgentID, Body: string(cancel), ReplyTo: &ref, TaskID: t.TaskID}, true, nil
	}
	actor, err := s.agentBySession(q, a.Session)
	if err != nil {
		return Delivery{}, false, err
	}
	ref := protocol.AgentRef{Name: actor.Name, AgentID: actor.AgentID, Scope: actor.Scope}
	return Delivery{FromScope: actor.Scope, FromName: senderKey(ref), ToScope: t.RequesterScope,
		ToName: t.RequesterAgentID, Body: msg, ReplyTo: &ref, TaskID: t.TaskID}, true, nil
}

// actsFor reports whether agentID is the owner or one of the owner's bound
// children: a subagent may cancel what its parent asked for.
func (s *Store) actsFor(q execQuerier, scope, agentID, ownerID string) bool {
	if agentID == ownerID {
		return true
	}
	var parentID string
	err := q.QueryRow(`SELECT p.agent_id FROM agents c
		JOIN agents p ON p.scope = c.scope AND p.session_id = c.parent_session_id
		WHERE c.scope=? AND c.agent_id=?`, scope, agentID).Scan(&parentID)
	return err == nil && parentID == ownerID
}

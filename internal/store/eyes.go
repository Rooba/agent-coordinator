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
	// The requester's bound on one host job, applied where the launch body is
	// built so every caller gets the same rule.
	eyesDeadlineDefault = 300
	eyesDeadlineMax     = 1800
	// eyesProgressMailInterval is how often a running task may DM its
	// requester. The row updates on every sample because that is cheap and
	// private; mail is not, so a 30 minute task costs an inbox a handful of
	// messages rather than one per turn.
	eyesProgressMailInterval = 120
	// maxEyesToolBytes bounds the one provider-supplied string a progress
	// record carries.
	maxEyesToolBytes = 64
	// maxEyesTurns bounds the counter, so a malformed sample cannot store nonsense.
	maxEyesTurns = 100000
	// eyesHeartbeatStaleS is how quiet a live task may go before its last
	// sign of life stops counting as one. A stalled task sends no mail, so
	// this is the only signal a reader gets that it has gone quiet.
	eyesHeartbeatStaleS = 120
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

// browserCap is what makes a broker eyes rather than just a shell: a brief is
// browser work, so a host that cannot drive Chrome takes none of it.
const browserCap = `"browser.chrome"`

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

// providerMatch is the runtime test the pick applies: the named provider, or
// - for a brief that named none - any provider defaultProvider could choose,
// so the pick and the default can never disagree about what a provider is.
func providerMatch(runtime string) (string, []any) {
	runtimes := []string{runtime}
	if runtime == "" {
		runtimes = providerOrder
	}
	terms := make([]string, len(runtimes))
	args := make([]any, len(runtimes))
	for i, r := range runtimes {
		terms[i], args[i] = "instr(a.caps, ?) > 0", providerCap(r)
	}
	return "(" + strings.Join(terms, " OR ") + ")", args
}

// taskHoldsBroker is the ledger's "still running" rule: a live task, or a
// cancelled one the broker has not answered - the provider keeps running
// until it says it stopped. The pick and the deadline sweep share it, so a
// task can never reserve a broker nothing will ever release.
const taskHoldsBroker = `(t.state IN ('queued','accepted') OR (t.state = 'cancelled' AND t.cancel_acked = 0))`

// PickLauncher returns any broker that is still polling and not already
// running a job. A broker is a relay-registered launcher in a reserved host:
// scope - a local agent calling itself a launcher is not one.
func (s *Store) PickLauncher() (LauncherRef, error) {
	return s.pickLauncher(s.db, "")
}

// pickLauncher takes the best live broker for a runtime, and its three
// outcomes are three different answers for the requester: nobody is polling,
// nobody can run that brief, or everyone who can is busy. One query decides
// all three because the rows sort match first, then free first. A match is
// Chrome AND the provider - eyes without a browser is not eyes.
func (s *Store) pickLauncher(q execQuerier, runtime string) (LauncherRef, error) {
	var l LauncherRef
	var matches, busy bool
	// ExpireEyesTasks is what hands a busy broker back when nobody reports.
	// substr rather than LIKE keeps host: one case-sensitive rule, the same
	// one ListWorkspaces hides by.
	match, args := providerMatch(runtime)
	args = append(args, browserCap, protocol.KindLauncher, len(hostScopePrefix), hostScopePrefix,
		s.Now().Add(-launcherWindow).Unix())
	err := q.QueryRow(`SELECT a.session_id, a.scope, a.name, a.agent_id, a.caps,
		`+match+` AND instr(a.caps, ?) > 0 AS matches,
		EXISTS(SELECT 1 FROM eyes_tasks t WHERE t.launcher_session = a.session_id
		       AND `+taskHoldsBroker+`) AS busy
		FROM agents a WHERE a.kind=? AND a.origin='relay' AND substr(a.scope,1,?)=?
		AND a.status != 'gone' AND a.last_seen >= ?
		ORDER BY matches DESC, busy, a.last_seen DESC LIMIT 1`, args...).
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

// taskLedgerBody is the daemon-owned task stamp. Ordinary send paths never
// write task_id, so body text that merely quotes or nests "task.launch" stays
// ordinary mail while real ledger mail never depends on JSON substring scans.
const taskLedgerBody = `m.task_id != ''`

// EyesTask is one requested host-eyes job. The ledger exists so cancel
// authorization and launch idempotency never depend on scanning inboxes.
type EyesTask struct {
	TaskID, RequesterScope, RequesterAgentID, Runtime, State string
	// LauncherSession and LauncherScope are the broker's row together: a
	// session id alone names no one, since it can be registered again in
	// another workspace once the original row is purged.
	LauncherSession, LauncherScope string
	CreatedAt, UpdatedAt           int64
	// CancelAcked is set once the broker has reported on a cancelled task.
	// Until then the task still reserves its broker.
	CancelAcked bool
}

// unsettled reports whether the task still holds its broker: live, or
// cancelled with the broker yet to say it stopped. Such a task can still gain
// a child, because that child is what sends the report acking the cancel.
func (t EyesTask) unsettled() bool {
	return t.State == "queued" || t.State == "accepted" ||
		(t.State == "cancelled" && !t.CancelAcked)
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
// task.launch in ONE transaction, so no two requests can take the same broker
// and a queued task always has the mail that starts it. The id, the chosen
// runtime and the bounded deadline are all recorded here, so nothing
// downstream has to guess them.
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
	now, deadline := s.Now().Unix(), eyesDeadline(req.DeadlineS)
	t := EyesTask{TaskID: "task-" + suffix, RequesterScope: req.Requester.Scope,
		RequesterAgentID: req.Requester.AgentID, LauncherSession: l.SessionID, LauncherScope: l.Scope,
		Runtime: runtime, State: "queued", CreatedAt: now, UpdatedAt: now}
	if _, err := tx.Exec(`INSERT INTO eyes_tasks
		(task_id, requester_scope, requester_agent_id, launcher_session, launcher_scope, runtime, state, deadline_s, created_at, updated_at)
		VALUES (?,?,?,?,?,?,'queued',?,?,?)`,
		t.TaskID, t.RequesterScope, t.RequesterAgentID, t.LauncherSession, t.LauncherScope,
		t.Runtime, deadline, now, now); err != nil {
		return EyesTask{}, protocol.AgentRef{}, err
	}
	body, err := json.Marshal(protocol.TaskLaunchMsg{Type: protocol.TaskLaunch, TaskID: t.TaskID,
		Runtime: runtime, Scope: req.Requester.Scope, Brief: req.Brief,
		ReplyTo: req.Requester, DeadlineS: deadline})
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
// broker: work it has not acked yet.
func (s *Store) PendingLaunches(launcherSession string) ([]protocol.Message, error) {
	return s.pendingTaskMail(launcherSession, "queued", false)
}

// PendingCancels lists the cancel mail of every cancelled task this broker
// has not answered. A cancel is only durable once the broker reports back:
// until then the provider may still be running, so the notice is redelivered.
func (s *Store) PendingCancels(launcherSession string) ([]protocol.Message, error) {
	return s.pendingTaskMail(launcherSession, "cancelled", true)
}

// pendingTaskMail is the redelivery queue behind both: this broker's tasks in
// one state, with their first launcher message (launch) or last (cancel),
// oldest first. Those positions are ledger invariants: assignment writes one
// launch, and cancellation writes at most one later cancel transactionally.
// Redelivery is at-least-once on purpose - dropping a duplicate by task id is
// the broker's own job.
func (s *Store) pendingTaskMail(launcherSession, state string, latest bool) ([]protocol.Message, error) {
	position := "MIN"
	if latest {
		position = "MAX"
	}
	rows, err := s.db.Query(`
		SELECT m.id, COALESCE(a.name, m.from_agent), m.body, m.created_at,
		       m.reply_to, m.from_scope, m.kind, m.task_id
		FROM eyes_tasks t
		JOIN agents l ON l.rowid = (SELECT l2.rowid FROM agents l2
			WHERE l2.session_id = t.launcher_session `+relayFirstRow+`)
		JOIN messages m ON m.id = (SELECT `+position+`(m2.id) FROM messages m2
			WHERE m2.task_id = t.task_id AND m2.scope = l.scope AND m2.to_agent = l.agent_id)
		`+senderJoin+`
		WHERE t.launcher_session = ? AND t.state = ? AND t.cancel_acked = 0
		ORDER BY t.created_at, m.id`, launcherSession, state)
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
		launcher_scope, runtime, state, cancel_acked, created_at, updated_at
		FROM eyes_tasks WHERE task_id=?`, taskID).
		Scan(&t.TaskID, &t.RequesterScope, &t.RequesterAgentID, &t.LauncherSession,
			&t.LauncherScope, &t.Runtime, &t.State, &t.CancelAcked, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return EyesTask{}, ErrUnknownTask
	}
	return t, err
}

// eyesTaskListMax bounds one listing: 50 rows is more ledger than a human
// reads at a glance, and the newest are the ones that matter.
const eyesTaskListMax = 50

// EyesTaskFilter narrows the ledger. Scope is the workspace boundary;
// OwnerAgentID is the opt-in "only mine" view - the tasks that agent could
// cancel, its own plus the ones its parent asked for. An empty field does not
// filter, so the whole workspace is the zero value.
type EyesTaskFilter struct {
	Scope, OwnerAgentID string
	Limit               int
}

// EyesTaskView is one ledger row plus the numbers that answer "is it alive
// and how long has it got?". All are measured on the daemon's clock - the
// same one the deadline sweep judges by - so a caller never has to reconcile
// its own. A settled task is measured to the moment it settled, so its
// elapsed time stops there instead of growing forever.
type EyesTaskView struct {
	EyesTask
	DeadlineS, ElapsedS int
	// RemainingS goes negative once a live task passes its deadline; a
	// settled one reports no time left rather than a negative count.
	RemainingS int
	// Progress is the last sample the task reported, HeartbeatAt when it
	// arrived (0 if none has) and HeartbeatAgeS how long ago that was.
	Progress      EyesProgress
	HeartbeatAt   int64
	HeartbeatAgeS int
	// Overdue is a live task whose deadline passed with no completion or
	// cancellation confirmed: the sweep has not reached it and nothing has
	// reported, so whether it still runs is unknown.
	Overdue bool
	// HeartbeatStale is a live task that has shown no sign of life for
	// eyesHeartbeatStaleS, measured from its last sample or, if none ever
	// arrived, from when it was created.
	HeartbeatStale bool
}

// ListEyesTasks lists the task ledger, tasks still holding a broker first and
// newest first within that, so an in-flight job is always the top row.
func (s *Store) ListEyesTasks(f EyesTaskFilter) ([]EyesTaskView, error) {
	var where []string
	var args []any
	if f.Scope != "" {
		where = append(where, "t.requester_scope = ?")
		args = append(args, f.Scope)
	}
	if f.OwnerAgentID != "" {
		// "Mine" means the tasks this agent could cancel: the ones it asked
		// for, plus the ones its parent asked for.
		where = append(where, `(t.requester_agent_id = ? OR t.requester_agent_id = (
			SELECT p.agent_id FROM agents c
			JOIN agents p ON c.parent_session_id != '' AND p.scope = c.scope AND p.session_id = c.parent_session_id
			WHERE c.scope = t.requester_scope AND c.agent_id = ?))`)
		args = append(args, f.OwnerAgentID, f.OwnerAgentID)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := s.db.Query(`SELECT t.task_id, t.requester_scope, t.requester_agent_id,
		t.launcher_session, t.launcher_scope, t.runtime, t.state, t.cancel_acked,
		t.deadline_s, t.created_at, t.updated_at, t.progress, t.heartbeat_at
		FROM eyes_tasks t`+clause+`
		ORDER BY `+taskHoldsBroker+` DESC, t.created_at DESC, t.task_id LIMIT ?`,
		append(args, eyesTaskLimit(f.Limit))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.Now().Unix()
	var out []EyesTaskView
	for rows.Next() {
		var v EyesTaskView
		var progress string
		if err := rows.Scan(&v.TaskID, &v.RequesterScope, &v.RequesterAgentID, &v.LauncherSession,
			&v.LauncherScope, &v.Runtime, &v.State, &v.CancelAcked, &v.DeadlineS,
			&v.CreatedAt, &v.UpdatedAt, &progress, &v.HeartbeatAt); err != nil {
			return nil, err
		}
		// A live task is measured to now; a settled one stops at the moment
		// it settled, so yesterday's two-minute job still reads as two minutes.
		live := v.unsettled()
		end := v.UpdatedAt
		if live {
			end = now
		}
		v.ElapsedS = int(end - v.CreatedAt)
		v.RemainingS = v.DeadlineS - v.ElapsedS
		v.Overdue = live && v.RemainingS < 0
		if !live {
			v.RemainingS = max(v.RemainingS, 0)
		}
		v.Progress = decodeEyesProgress(progress)
		quietSince := v.CreatedAt
		if v.HeartbeatAt > 0 {
			quietSince = v.HeartbeatAt
			v.HeartbeatAgeS = max(int(end-v.HeartbeatAt), 0)
		}
		v.HeartbeatStale = live && end-quietSince >= eyesHeartbeatStaleS
		out = append(out, v)
	}
	return out, rows.Err()
}

// eyesTaskLimit bounds one listing to eyesTaskListMax rows.
func eyesTaskLimit(n int) int {
	if n <= 0 || n > eyesTaskListMax {
		return eyesTaskListMax
	}
	return n
}

// EyesProgress is what a running task reports about itself. It is content-free
// by construction: a turn count and the NAME of the tool last used are all it
// may carry, because an eyes task looks at a live browser session and nothing
// derived from the page may be stored or mailed. MailedAt and MailedTurns are
// the throttle's own memory of the last copy the requester was sent.
type EyesProgress struct {
	Turns       int    `json:"turns,omitempty"`
	Tool        string `json:"tool,omitempty"`
	MailedAt    int64  `json:"mailed_at,omitempty"`
	MailedTurns int    `json:"mailed_turns,omitempty"`
}

// cleanEyesTool bounds a tool name and requires it to BE a name: a value
// carrying anything else is dropped whole rather than compacted, so page text
// cannot reach the row or an inbox by posing as a tool.
func cleanEyesTool(name string) string {
	if len(name) > maxEyesToolBytes {
		name = name[:maxEyesToolBytes]
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '-', c == '.', c == ':':
		default:
			return ""
		}
	}
	return name
}

func decodeEyesProgress(raw string) EyesProgress {
	var p EyesProgress
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	return p
}

// RecordEyesProgress writes one liveness sample from a running task: the row
// moves on every sample and the task's child agent is marked seen, so "is it
// alive?" is answerable at a glance, while the requester's inbox is spared
// everything but a throttled copy. It reports the stored sample and whether
// that copy was sent.
func (s *Store) RecordEyesProgress(taskID string, a EyesActor, p EyesProgress) (EyesProgress, bool, error) {
	now := s.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return EyesProgress{}, false, err
	}
	defer tx.Rollback()
	t, err := s.eyesTask(tx, taskID)
	if err != nil {
		return EyesProgress{}, false, err
	}
	// Only the two sides that actually run the job may say it is alive, and
	// only while it still is: a settled task takes no more samples.
	role := s.eyesRole(tx, t, a)
	if role != roleChild && role != roleLauncher {
		return EyesProgress{}, false, ErrNotYourTask
	}
	if t.State != "queued" && t.State != "accepted" {
		return EyesProgress{}, false, fmt.Errorf("%w: %s", ErrTaskNotLive, t.State)
	}
	next, due := s.nextEyesProgress(tx, t, p, now)
	if err := s.writeEyesProgress(tx, t, next, now); err != nil {
		return EyesProgress{}, false, err
	}
	if due {
		// The body is built HERE, from the clamped record, so nothing a
		// provider wrote is ever passed through to an inbox.
		body, err := json.Marshal(protocol.TaskProgressMsg{Type: protocol.TaskProgress,
			TaskID: t.TaskID, Turns: next.Turns, Tool: next.Tool, ElapsedS: int(now - t.CreatedAt)})
		if err != nil {
			return EyesProgress{}, false, err
		}
		d, deliver, err := s.eyesMail(tx, t, role, a, string(body))
		if err != nil {
			return EyesProgress{}, false, err
		}
		if deliver {
			if err := s.sendToScope(tx, d); err != nil {
				return EyesProgress{}, false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return EyesProgress{}, false, err
	}
	return next, due, nil
}

// nextEyesProgress folds one sample onto the stored one and decides whether the
// requester is due another copy. Turns only ever climb, the last known tool
// name survives a sample that names none, and the first sample only starts the
// clock - the accept already told the requester the task had begun. A task
// stuck on one turn mails nothing at all, and its heartbeat keeps arriving, so
// the row reads as alive but not advancing rather than as failed.
func (s *Store) nextEyesProgress(q execQuerier, t EyesTask, p EyesProgress, now int64) (EyesProgress, bool) {
	var raw string
	if err := q.QueryRow(`SELECT progress FROM eyes_tasks WHERE task_id=?`, t.TaskID).Scan(&raw); err != nil {
		raw = ""
	}
	prev := decodeEyesProgress(raw)
	next := EyesProgress{Turns: min(max(p.Turns, prev.Turns), maxEyesTurns), Tool: cleanEyesTool(p.Tool),
		MailedAt: prev.MailedAt, MailedTurns: prev.MailedTurns}
	if next.Tool == "" {
		next.Tool = cleanEyesTool(prev.Tool)
	}
	due := prev.MailedAt != 0 && now-prev.MailedAt >= eyesProgressMailInterval && next.Turns > prev.MailedTurns
	if due || prev.MailedAt == 0 {
		next.MailedAt, next.MailedTurns = now, next.Turns
	}
	return next, due
}

// writeEyesProgress stamps the row and refreshes the task's child agent, whose
// last_seen would otherwise stay frozen at registration - leaving no way to
// tell a working task from a dead one.
func (s *Store) writeEyesProgress(q execQuerier, t EyesTask, p EyesProgress, now int64) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err := q.Exec(`UPDATE eyes_tasks SET progress=?, heartbeat_at=? WHERE task_id=?`,
		string(body), now, t.TaskID); err != nil {
		return err
	}
	_, err = q.Exec(`UPDATE agents SET last_seen=?, status=CASE status WHEN 'idle' THEN 'active' ELSE status END
		WHERE scope=? AND session_id=? AND status != 'gone'`, now, t.RequesterScope, "eyes-"+t.TaskID)
	return err
}

// eyesRole is which side of a task an actor speaks for.
type eyesRole string

const (
	roleLauncher  eyesRole = "launcher"
	roleChild     eyesRole = "child"
	roleRequester eyesRole = "requester"
	roleSystem    eyesRole = "system"
)

// eyesEdges is the whole task lifecycle in one table: per target state, which
// role may ask for it and from which states. The launcher acks a brief and
// may give up on one it never accepted; once it has accepted, its child owns
// the outcome; the requester's side may call the whole thing off while it is
// still live; and the deadline sweep may fail anything still live, because a
// task nobody reports on must not run out the ledger. Anything absent here is
// not a transition.
var eyesEdges = map[string]map[eyesRole][]string{
	"accepted": {roleLauncher: {"queued"}},
	"done":     {roleChild: {"accepted"}},
	"failed": {roleChild: {"queued", "accepted"}, roleLauncher: {"queued"},
		roleSystem: {"queued", "accepted"}},
	"cancelled": {roleRequester: {"queued", "accepted"}},
}

// EyesActor is the authenticated caller behind one lifecycle move: the row it
// proved (its workspace and session), the ref that workspace knows it by, and
// where the request arrived. Origin is the daemon's own stamp and never
// travels on the wire, so only a relay client can be a broker or its child.
type EyesActor struct {
	Scope, SessionID, Origin string
	Ref                      protocol.AgentRef
	system                   bool // the deadline sweep, which speaks for nobody
}

// TransitionEyesTask applies one lifecycle move made by a relay session - the
// launcher's ack, or the child's report - and mails body to the requester.
func (s *Store) TransitionEyesTask(taskID string, a EyesActor, to, body string) (EyesTask, bool, error) {
	return s.moveEyesTask(taskID, to, a, body)
}

// CancelEyesTask is the requester's own move: it settles the task and tells
// the broker holding it. Cancelling an already cancelled task is a no-op, so
// a retried cancel_eyes never sends a second task.cancel.
func (s *Store) CancelEyesTask(taskID string, a EyesActor) (EyesTask, error) {
	t, _, err := s.moveEyesTask(taskID, "cancelled", a, "")
	return t, err
}

// moveEyesTask is the ONE write path for the ledger: authorize the actor's
// role against the edge table, then write the new state and the message that
// move implies in a single transaction - so a state change never exists
// without its mail, and a repeat adds neither.
func (s *Store) moveEyesTask(taskID, to string, a EyesActor, msg string) (EyesTask, bool, error) {
	edge, ok := eyesEdges[to]
	if !ok {
		return EyesTask{}, false, fmt.Errorf("%w: %q is not a task state", ErrBadTransition, to)
	}
	// One timestamp for the whole move, read before the transaction opens so
	// nothing inside it waits on the clock.
	now := s.Now().Unix()
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
	// A cancelled task still gets a terminal report, once the broker has
	// killed the provider. That report IS the cancel's acknowledgement:
	// record it so the notice stops being redelivered, rather than refusing
	// it and leaving the broker to retry forever.
	if t.State == "cancelled" && s.acksCancel(tx, t, role, to) {
		if _, err := tx.Exec(`UPDATE eyes_tasks SET cancel_acked=1 WHERE task_id=?`, taskID); err != nil {
			return EyesTask{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return EyesTask{}, false, err
		}
		return t, false, nil
	}
	if !slices.Contains(from, t.State) {
		return EyesTask{}, false, fmt.Errorf("%w: %s -> %s", ErrBadTransition, t.State, to)
	}
	d, deliver, err := s.eyesMail(tx, t, role, a, msg)
	if err != nil {
		return EyesTask{}, false, err
	}
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
// no standing in it. Every side is decided on the row the caller actually
// proved - its workspace, its session and the kind that row was registered
// with - so a session id alone buys nothing: the launcher and its child exist
// only over the relay, and the requester's side only off the unix socket.
func (s *Store) eyesRole(q execQuerier, t EyesTask, a EyesActor) eyesRole {
	switch {
	case a.system:
		return roleSystem
	case a.Origin == RelayOrigin && a.SessionID == "eyes-"+t.TaskID && a.Scope == t.RequesterScope:
		if s.relayKind(q, a.Scope, a.SessionID, protocol.KindEyes) {
			return roleChild
		}
	case a.Origin == RelayOrigin && a.SessionID == t.LauncherSession && a.Scope == t.LauncherScope:
		if s.relayKind(q, a.Scope, a.SessionID, protocol.KindLauncher) {
			return roleLauncher
		}
	case a.Origin == "" && a.Ref.Scope == t.RequesterScope &&
		s.actsFor(q, t.RequesterScope, a.Ref.AgentID, t.RequesterAgentID):
		return roleRequester
	}
	return ""
}

// relayKind reports whether the row the caller proved is a relay row of that
// kind. The lookup is (scope, session) - never a session id searched across
// workspaces - so nothing registered elsewhere can wear a broker's name.
func (s *Store) relayKind(q execQuerier, scope, session, kind string) bool {
	id, err := s.agentAt(q, scope, session)
	return err == nil && id.Origin == RelayOrigin && id.Kind == kind
}

// acksCancel reports whether a terminal report on a cancelled task answers
// its cancel: the child the broker killed, the broker's own failure when the
// task never got a child, or the deadline sweep - which is the only thing
// left to free a broker that never reported at all.
func (s *Store) acksCancel(q execQuerier, t EyesTask, role eyesRole, to string) bool {
	switch role {
	case roleChild:
		return to == "done" || to == "failed"
	case roleLauncher:
		return to == "failed" && !s.relayKind(q, t.RequesterScope, "eyes-"+t.TaskID, protocol.KindEyes)
	case roleSystem:
		return to == "failed"
	}
	return false
}

// eyesMail addresses the message a move implies: a cancel goes to the broker
// holding the job and carries its own task.cancel body, while an ack, a
// report or a deadline failure goes back to the requester carrying what the
// actor sent. It reports false when there is nobody left to tell a cancel, or
// nobody left to speak an expiry - an agent row is purged hours before its
// task is, and that must not block a move nobody could have mailed anyway.
func (s *Store) eyesMail(q execQuerier, t EyesTask, role eyesRole, a EyesActor, msg string) (Delivery, bool, error) {
	if role == roleRequester {
		l, err := s.agentAt(q, t.LauncherScope, t.LauncherSession)
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
		// The cancel carries the TASK's requester, not whoever pressed the
		// button - a bound subagent may be the one cancelling. Every
		// lifecycle mail then names the same return address, which is the
		// one the broker matches its launch against and answers on.
		from, err := s.resolveSender(q, t.RequesterScope, t.RequesterAgentID)
		if err != nil {
			return Delivery{}, false, err
		}
		ref := from.AgentRef
		return Delivery{From: from, ToScope: l.Scope, ToName: l.AgentID,
			Body: string(cancel), ReplyTo: &ref, TaskID: t.TaskID}, true, nil
	}
	actor, err := s.taskVoice(q, t, a)
	if err == nil && role == roleSystem {
		// An agent row is purged hours before its task, so the sweep confirms
		// the requester is still there before it counts on mailing one.
		_, _, err = s.resolveAgent(q, t.RequesterScope, t.RequesterAgentID)
	}
	if err != nil {
		// Only an expiry tolerates a missing party - it speaks for nobody and
		// its job is freeing the broker. An authenticated actor whose row
		// vanished must fail instead, so no state change can exist without
		// the mail it implies.
		if role == roleSystem && (errors.Is(err, ErrNoSession) || errors.Is(err, ErrNoAgent)) {
			return Delivery{}, false, nil
		}
		return Delivery{}, false, err
	}
	ref := protocol.AgentRef{Name: actor.Name, AgentID: actor.AgentID, Scope: actor.Scope}
	return Delivery{From: Sender{AgentRef: ref, Kind: actor.Kind}, ToScope: t.RequesterScope,
		ToName: t.RequesterAgentID, Body: msg, ReplyTo: &ref, TaskID: t.TaskID}, true, nil
}

// taskVoice is the identity a report leaves under: the row the actor proved,
// or - when the deadline sweep settles a task with no caller at all - the
// task's child, falling back to the broker that was holding it. Every lookup
// is a workspace and a session together, the ledger's own pair included.
func (s *Store) taskVoice(q execQuerier, t EyesTask, a EyesActor) (RelayIdentity, error) {
	if a.SessionID != "" {
		return s.agentAt(q, a.Scope, a.SessionID)
	}
	id, err := s.agentAt(q, t.RequesterScope, "eyes-"+t.TaskID)
	if errors.Is(err, ErrNoSession) {
		return s.agentAt(q, t.LauncherScope, t.LauncherSession)
	}
	return id, err
}

// ExpireEyesTasks settles every task whose deadline has passed while it still
// holds a broker, so one that died mid-job neither strands its requester nor
// keeps a launcher out of service. A live task is failed and its requester
// told; a cancel nobody answered is simply acked, since the requester already
// asked for the stop and only the broker was owed anything. One task it
// cannot settle costs the others nothing: the sweep finishes and answers with
// what it settled plus the last error it hit.
func (s *Store) ExpireEyesTasks(now time.Time) (int, error) {
	ids, err := s.expiredTasks(now)
	if err != nil {
		return 0, err
	}
	n := 0
	var last error
	for _, id := range ids {
		body, err := json.Marshal(protocol.TaskFailedMsg{Type: protocol.TaskFailed,
			TaskID: id, Error: "deadline exceeded"})
		if err != nil {
			last = err
			continue
		}
		t, moved, err := s.moveEyesTask(id, "failed", EyesActor{system: true}, string(body))
		switch {
		// The task settled or was purged between the snapshot and the move -
		// either way there is nothing left for the deadline to do to it.
		case errors.Is(err, ErrBadTransition), errors.Is(err, ErrUnknownTask):
		case err != nil:
			last = err
		// Failing a live task and acking an abandoned cancel both settle one.
		case moved, t.State == "cancelled":
			n++
		}
	}
	return n, last
}

// expiredTasks names the tasks past their deadline that still hold a broker,
// oldest first. Its cursor closes before any of them is settled: one
// connection serves the whole store, so a read still open would block every
// write.
func (s *Store) expiredTasks(now time.Time) ([]string, error) {
	rows, err := s.db.Query(`SELECT t.task_id FROM eyes_tasks t
		WHERE `+taskHoldsBroker+` AND t.created_at + t.deadline_s < ?
		ORDER BY t.created_at`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// actsFor reports whether agentID is the owner or one of the owner's bound
// children: a subagent may cancel what its parent asked for. A parentless
// agent stores an empty parent, which must never match a row that has one.
func (s *Store) actsFor(q execQuerier, scope, agentID, ownerID string) bool {
	if agentID == ownerID {
		return true
	}
	var parentID string
	err := q.QueryRow(`SELECT p.agent_id FROM agents c
		JOIN agents p ON c.parent_session_id != '' AND p.scope = c.scope AND p.session_id = c.parent_session_id
		WHERE c.scope=? AND c.agent_id=?`, scope, agentID).Scan(&parentID)
	return err == nil && parentID == ownerID
}

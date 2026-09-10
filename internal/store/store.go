package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS agents (
  scope TEXT NOT NULL, session_id TEXT NOT NULL,
  agent_id TEXT NOT NULL, name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',
  registered_at INTEGER NOT NULL, last_seen INTEGER NOT NULL,
  PRIMARY KEY (scope, session_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_scope_name ON agents(scope, name);
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  scope TEXT NOT NULL, agent_id TEXT NOT NULL,
  tool TEXT NOT NULL, activity TEXT NOT NULL,
  files TEXT NOT NULL DEFAULT '[]', ts INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_scope_agent_ts ON events(scope, agent_id, ts DESC);
CREATE TABLE IF NOT EXISTS file_touches (
  scope TEXT NOT NULL, path TEXT NOT NULL, agent_id TEXT NOT NULL,
  action TEXT NOT NULL, ts INTEGER NOT NULL,
  PRIMARY KEY (scope, path, agent_id)
);
CREATE TABLE IF NOT EXISTS tasks (
  scope TEXT NOT NULL, agent_id TEXT NOT NULL, task_key TEXT NOT NULL,
  subject TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'pending',
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (scope, agent_id, task_key)
);
CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  scope TEXT NOT NULL, from_agent TEXT NOT NULL, to_agent TEXT,
  body TEXT NOT NULL, created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS deliveries (
  message_id INTEGER NOT NULL, agent_id TEXT NOT NULL,
  notice_sent_at INTEGER, read_at INTEGER,
  PRIMARY KEY (message_id, agent_id)
);
CREATE TABLE IF NOT EXISTS claims (
  scope TEXT NOT NULL, path TEXT NOT NULL, agent_id TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '', since INTEGER NOT NULL,
  PRIMARY KEY (scope, path)
);
CREATE TABLE IF NOT EXISTS eyes_tasks (
  -- task_id is the primary key so a launch is durably at most once: a
  -- repeated create finds the live row instead of starting a second run.
  task_id TEXT PRIMARY KEY,
  requester_scope TEXT NOT NULL, requester_agent_id TEXT NOT NULL,
  launcher_session TEXT NOT NULL, runtime TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'queued',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_eyes_tasks_launcher_state ON eyes_tasks(launcher_session, state);
`

const (
	activeWindow = 2 * time.Minute
	idleWindow   = 15 * time.Minute
	staleWindow  = 60 * time.Minute
)

type Store struct {
	db  *sql.DB
	Now func() time.Time
}

// execQuerier is *sql.DB or *sql.Tx. Writes that must be atomic take one, so
// there is a single implementation whether or not a caller already holds a
// transaction. With SetMaxOpenConns(1) a tx owns the only connection, so code
// running inside one must never reach for s.db.
type execQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer by construction
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// Guarded migration: older databases predate these columns; a duplicate
	// column error just means the migration already ran.
	for _, alter := range []string{
		`ALTER TABLE agents ADD COLUMN source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN parent_session_id TEXT NOT NULL DEFAULT ''`,
		// Relay identity: what an agent is, where it came in from, and the
		// sha256 of its per-session TCP secret (never the secret itself).
		`ALTER TABLE agents ADD COLUMN kind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN origin TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN platform TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN caps TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE agents ADD COLUMN relay_secret_hash TEXT NOT NULL DEFAULT ''`,
		// Cross-workspace mail: where the sender lives, the return address,
		// and the sender's kind denormalized at send time.
		`ALTER TABLE messages ADD COLUMN from_scope TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE messages ADD COLUMN reply_to TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE messages ADD COLUMN task_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE messages ADD COLUMN kind TEXT NOT NULL DEFAULT ''`,
		// A task carries the deadline it was created with, so expiring it is a
		// property of the row rather than of whoever happens to sweep.
		`ALTER TABLE eyes_tasks ADD COLUMN deadline_s INTEGER NOT NULL DEFAULT 300`,
		// A cancel is durable only once the broker answers it, so the task
		// records whether that answer has arrived.
		`ALTER TABLE eyes_tasks ADD COLUMN cancel_acked INTEGER NOT NULL DEFAULT 0`,
		// The workspace the chosen broker registered in, so its side of the
		// task is proved by scope and session together - a session id that is
		// registered again elsewhere inherits nothing.
		`ALTER TABLE eyes_tasks ADD COLUMN launcher_scope TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(alter); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db, Now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func agentID(sessionID string) string {
	h := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(h[:6])
}

func (s *Store) Register(scope, sessionID, source string) (string, error) {
	return s.register(s.db, scope, sessionID, source)
}

func (s *Store) register(q execQuerier, scope, sessionID, source string) (string, error) {
	now := s.Now().Unix()
	var name string
	err := q.QueryRow(`SELECT name FROM agents WHERE scope=? AND session_id=?`, scope, sessionID).Scan(&name)
	if err == nil {
		_, err = q.Exec(`UPDATE agents SET status='active', last_seen=? WHERE scope=? AND session_id=?`, now, scope, sessionID)
		return name, err
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	base := friendlyName(sessionID)
	name = base
	for n := 2; n <= 50; n++ {
		var count int
		if err := q.QueryRow(`SELECT COUNT(*) FROM agents WHERE scope=? AND name=?`, scope, name).Scan(&count); err != nil {
			return "", err
		}
		if count == 0 {
			_, err := q.Exec(`INSERT INTO agents (scope, session_id, agent_id, name, status, registered_at, last_seen, source)
				VALUES (?,?,?,?,'active',?,?,?)`, scope, sessionID, agentID(sessionID), name, now, now, source)
			if err == nil {
				return name, nil
			}
			if !isUniqueViolation(err) {
				return "", err
			}
			// Unique race: either a concurrent Register won this session's PK
			// (return its name) or took this name (try the next suffix).
			if e := q.QueryRow(`SELECT name FROM agents WHERE scope=? AND session_id=?`, scope, sessionID).Scan(&name); e == nil {
				return name, nil
			}
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
	return "", fmt.Errorf("register: no free name near %q in scope %q", base, scope)
}

// ChildSessionID derives the synthetic session key for a subagent row. The
// slash-joined form keeps child rows classified as hook-origin (no mcp- prefix).
func ChildSessionID(parentSessionID, subagentID string) string {
	return parentSessionID + "/" + subagentID
}

// RegisterChild registers (or refreshes) a subagent identity under its parent
// session, giving it its own row and therefore its own inbox. Idempotent per
// (scope, child session); names are <parent>/<agent_type|sub>-<n>. created is
// true only for the call whose INSERT won, so callers can announce it once.
func (s *Store) RegisterChild(scope, parentSessionID, subagentID, agentType string) (string, bool, error) {
	child := ChildSessionID(parentSessionID, subagentID)
	now := s.Now().Unix()
	var name string
	err := s.db.QueryRow(`SELECT name FROM agents WHERE scope=? AND session_id=?`, scope, child).Scan(&name)
	if err == nil {
		_, err = s.db.Exec(`UPDATE agents SET status='active', last_seen=? WHERE scope=? AND session_id=?`, now, scope, child)
		return name, false, err
	}
	if err != sql.ErrNoRows {
		return "", false, err
	}
	// The parent anchors the child's name; registering it also keeps the
	// parent row fresh while its subagents work.
	parentName, err := s.Register(scope, parentSessionID, "hook")
	if err != nil {
		return "", false, err
	}
	typ := strings.ToLower(agentType)
	if typ == "" {
		typ = "sub"
	}
	for n := 1; n <= 50; n++ {
		name = fmt.Sprintf("%s/%s-%d", parentName, typ, n)
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE scope=? AND name=?`, scope, name).Scan(&count); err != nil {
			return "", false, err
		}
		if count > 0 {
			continue
		}
		_, err := s.db.Exec(`INSERT INTO agents (scope, session_id, agent_id, name, status, registered_at, last_seen, source, parent_session_id)
			VALUES (?,?,?,?,'active',?,?,'hook-subagent',?)`, scope, child, agentID(child), name, now, now, parentSessionID)
		if err == nil {
			return name, true, nil
		}
		if !isUniqueViolation(err) {
			return "", false, err
		}
		// Unique race: a concurrent RegisterChild won this child's PK (return
		// its name) or took this name (try the next suffix).
		if e := s.db.QueryRow(`SELECT name FROM agents WHERE scope=? AND session_id=?`, scope, child).Scan(&name); e == nil {
			return name, false, nil
		}
	}
	return "", false, fmt.Errorf("register child: no free name under %q in scope %q", parentName, scope)
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ErrIdentityUnknown refuses a silent self-mint while hook-registered agents
// are live in the scope: the caller is almost certainly one of them and must
// name itself or bind explicitly.
var ErrIdentityUnknown = errors.New("cannot determine your identity: pass from=<your name> or call register_agent")

// RegisterIfNoLiveHook registers like Register, except a NEW identity is
// refused while the scope has live hook-registered agents. Existing rows
// refresh normally.
func (s *Store) RegisterIfNoLiveHook(scope, sessionID, source string) (string, error) {
	var name string
	err := s.db.QueryRow(`SELECT name FROM agents WHERE scope=? AND session_id=?`, scope, sessionID).Scan(&name)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if err == sql.ErrNoRows {
		live, err := s.hasLiveHookAgents(scope)
		if err != nil {
			return "", err
		}
		if live {
			return "", ErrIdentityUnknown
		}
	}
	return s.Register(scope, sessionID, source)
}

// hasLiveHookAgents reports whether the scope has any active or idle agent
// that came from a session hook. Hook origin is identified by session id
// shape - only self-minted MCP identities carry the mcp- prefix - which also
// classifies legacy rows that predate the source column. A kind-bearing row
// (a broker or an eyes child) is never a hook agent.
func (s *Store) hasLiveHookAgents(scope string) (bool, error) {
	rows, err := s.db.Query(`SELECT status, last_seen FROM agents
		WHERE scope=? AND session_id NOT LIKE 'mcp-%' AND kind=''`, scope)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var explicit string
		var seen int64
		if err := rows.Scan(&explicit, &seen); err != nil {
			return false, err
		}
		if s.live(explicit, seen) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// AgentIdentity is the whoami view of one agent row.
type AgentIdentity struct {
	Name    string
	AgentID string
	Source  string
	Parent  string // parent agent's name, set only for subagent child rows
	Kind    string
	Origin  string
}

// Identity returns the stored identity for a session.
func (s *Store) Identity(scope, sessionID string) (AgentIdentity, error) {
	var id AgentIdentity
	var parentSession string
	err := s.db.QueryRow(`SELECT name, agent_id, source, parent_session_id, kind, origin
		FROM agents WHERE scope=? AND session_id=?`, scope, sessionID).
		Scan(&id.Name, &id.AgentID, &id.Source, &parentSession, &id.Kind, &id.Origin)
	if err == sql.ErrNoRows {
		return id, fmt.Errorf("no agent for this session in this workspace")
	}
	if err != nil {
		return id, err
	}
	if parentSession != "" {
		s.db.QueryRow(`SELECT name FROM agents WHERE scope=? AND session_id=?`, scope, parentSession).Scan(&id.Parent)
	}
	return id, nil
}

// ResolveActor decides which agent a request acts as. An explicit from must
// name the caller's own row or one of its registered children, so no session
// can act as - or drain the inbox of - another agent. A caller that names no
// session is a plain unix client (the CLI, wait): there the socket
// directory's permissions are the trust boundary, so its name is taken at
// face value.
func (s *Store) ResolveActor(scope, callerSessionID, explicitFrom string) (AgentIdentity, error) {
	if explicitFrom == "" {
		return s.Identity(scope, callerSessionID)
	}
	var sessionID, parentSession string
	var callerHere bool
	err := s.db.QueryRow(`SELECT a.session_id, a.parent_session_id,
		EXISTS(SELECT 1 FROM agents c WHERE c.scope = a.scope AND c.session_id = ?) AS caller_here
		FROM agents a WHERE a.scope=? AND (a.name=? OR a.agent_id=?)`,
		callerSessionID, scope, explicitFrom, explicitFrom).Scan(&sessionID, &parentSession, &callerHere)
	if err == sql.ErrNoRows {
		return AgentIdentity{}, fmt.Errorf("no agent %q in this workspace", explicitFrom)
	}
	if err != nil {
		return AgentIdentity{}, err
	}
	// Speaking for a child needs the caller's OWN row to still be here: once a
	// parent is purged, its children are nobody's proxy.
	own := callerSessionID == sessionID || (callerHere && callerSessionID == parentSession)
	if callerSessionID != "" && !own {
		return AgentIdentity{}, ErrForeignSession
	}
	return s.Identity(scope, sessionID)
}

func (s *Store) SetStatus(scope, sessionID, status string) error {
	_, err := s.db.Exec(`UPDATE agents SET status=?, last_seen=? WHERE scope=? AND session_id=?`,
		status, s.Now().Unix(), scope, sessionID)
	return err
}

// Touch is the MCP-call heartbeat: it keeps a live row fresh and lifts sticky
// idle back to active, but never resurrects an explicitly gone agent.
func (s *Store) Touch(scope, sessionID string) error {
	_, err := s.db.Exec(`UPDATE agents SET last_seen=?, status=CASE status WHEN 'idle' THEN 'active' ELSE status END
		WHERE scope=? AND session_id=? AND status != 'gone'`, s.Now().Unix(), scope, sessionID)
	return err
}

// RecordEvent ingests one PostToolUse event and returns notices for the agent
// (unread messages, broadcasts, conflict warnings).
func (s *Store) RecordEvent(scope, sessionID string, req protocol.Request) ([]string, error) {
	if _, err := s.Register(scope, sessionID, "event"); err != nil { // auto-register + freshness
		return nil, err
	}
	now := s.Now().Unix()
	aid := agentID(sessionID)
	filesJSON, _ := json.Marshal(req.Files)
	if _, err := s.db.Exec(`INSERT INTO events (scope, agent_id, tool, activity, files, ts) VALUES (?,?,?,?,?,?)`,
		scope, aid, req.Tool, req.Activity, string(filesJSON), now); err != nil {
		return nil, err
	}
	for _, p := range req.Writes {
		if _, err := s.db.Exec(`INSERT INTO file_touches (scope, path, agent_id, action, ts) VALUES (?,?,?,'write',?)
			ON CONFLICT(scope, path, agent_id) DO UPDATE SET ts=excluded.ts`, scope, p, aid, now); err != nil {
			return nil, err
		}
	}
	if req.ReplaceTasks {
		if _, err := s.db.Exec(`DELETE FROM tasks WHERE scope=? AND agent_id=?`, scope, aid); err != nil {
			return nil, err
		}
		for _, task := range req.Tasks {
			if _, err := s.db.Exec(`INSERT INTO tasks (scope, agent_id, task_key, subject, status, updated_at)
				VALUES (?,?,?,?,?,?)`, scope, aid, task.Key, task.Subject, task.Status, now); err != nil {
				return nil, err
			}
		}
	}
	if ev := req.TaskEv; ev != nil {
		switch ev.Kind {
		case "create":
			key := ev.Key
			if key == "" {
				key = fmt.Sprintf("auto-%d", now)
			}
			if _, err := s.db.Exec(`INSERT INTO tasks (scope, agent_id, task_key, subject, status, updated_at)
				VALUES (?,?,?,?,?,?) ON CONFLICT(scope, agent_id, task_key) DO UPDATE SET subject=excluded.subject`,
				scope, aid, key, ev.Subject, ev.Status, now); err != nil {
				return nil, err
			}
		case "update":
			if _, err := s.db.Exec(`UPDATE tasks SET status=?, updated_at=? WHERE scope=? AND agent_id=? AND task_key=?`,
				ev.Status, now, scope, aid, ev.Key); err != nil {
				return nil, err
			}
		}
	}
	return s.noticesFor(scope, aid, req.Writes)
}

const conflictWindow = 30 * time.Minute

// ErrNoAgent: the workspace has no agent by that name or id. It is a sentinel
// because a purged row is a normal outcome for the deadline sweep, which must
// settle a task even when nobody is left to tell.
var ErrNoAgent = errors.New("no agent")

func (s *Store) resolveAgent(q execQuerier, scope, nameOrID string) (aid, name string, err error) {
	err = q.QueryRow(`SELECT agent_id, name FROM agents WHERE scope=? AND (name=? OR agent_id=?)`,
		scope, nameOrID, nameOrID).Scan(&aid, &name)
	if err == sql.ErrNoRows {
		return "", "", fmt.Errorf("%w %q in this workspace", ErrNoAgent, nameOrID)
	}
	return aid, name, err
}

// Sender is everything a message write needs about who is sending: the id the
// row lands under, the name a return address carries, and the kind stamped on
// it. A caller that already read the row hands one over so nothing looks it up
// twice.
type Sender struct {
	protocol.AgentRef
	Kind string
}

// resolveSender reads that whole identity in one lookup.
func (s *Store) resolveSender(q execQuerier, scope, nameOrID string) (Sender, error) {
	from := Sender{AgentRef: protocol.AgentRef{Scope: scope}}
	err := q.QueryRow(`SELECT agent_id, name, kind FROM agents WHERE scope=? AND (name=? OR agent_id=?)`,
		scope, nameOrID, nameOrID).Scan(&from.AgentID, &from.Name, &from.Kind)
	if err == sql.ErrNoRows {
		return Sender{}, fmt.Errorf("%w %q in this workspace", ErrNoAgent, nameOrID)
	}
	return from, err
}

// Delivery is one message write. The daemon fills every field from the
// authenticated sender; a client never gets to say who it is.
type Delivery struct {
	FromScope string // sender's workspace
	FromName  string // sender's name or agent id in FromScope
	From      Sender // the sender already resolved; when set, FromScope and FromName are not read
	ToScope   string // recipient workspace; empty means the sender's scope
	ToName    string // recipient name or agent id; empty means everyone live there
	Body      string
	ReplyTo   *protocol.AgentRef // return address, stamped by the daemon
	TaskID    string
}

// SendToScope is the one write path for mail, and one transaction: unicast
// when ToName is set, otherwise a broadcast to everyone else live in the
// target scope. The row lands in the RECIPIENT's scope, so peek, read, wait
// and history keep working untouched, and from_scope appears only when the
// sender is somewhere else.
func (s *Store) SendToScope(d Delivery) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.sendToScope(tx, d); err != nil {
		return err
	}
	return tx.Commit()
}

// sendToScope writes one message and its deliveries through q. Callers that
// already hold a transaction (assigning an eyes task, advancing one) pass it
// in, so a task never exists without the mail that announces it.
func (s *Store) sendToScope(q execQuerier, d Delivery) error {
	var err error
	from := d.From
	if from.AgentID == "" {
		if from, err = s.resolveSender(q, d.FromScope, d.FromName); err != nil {
			return err
		}
	}
	toScope, fromScope := d.ToScope, ""
	if toScope == "" {
		toScope = from.Scope
	}
	if toScope != from.Scope {
		fromScope = from.Scope
	}
	var toID any // NULL addresses everyone live in the scope
	var targets []string
	if d.ToName != "" {
		id, _, err := s.resolveAgent(q, toScope, d.ToName)
		if err != nil {
			return err
		}
		toID, targets = id, []string{id}
	} else if targets, err = s.liveAgents(q, toScope, from.AgentID); err != nil {
		return err
	}
	replyTo := ""
	if d.ReplyTo != nil {
		b, err := json.Marshal(d.ReplyTo)
		if err != nil {
			return err
		}
		replyTo = string(b)
	}
	res, err := q.Exec(`INSERT INTO messages
		(scope, from_agent, to_agent, body, created_at, from_scope, reply_to, task_id, kind)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		toScope, from.AgentID, toID, d.Body, s.Now().Unix(), fromScope, replyTo, d.TaskID, from.Kind)
	if err != nil {
		return err
	}
	mid, _ := res.LastInsertId()
	for _, aid := range targets {
		if _, err := q.Exec(`INSERT INTO deliveries (message_id, agent_id) VALUES (?,?)`, mid, aid); err != nil {
			return err
		}
	}
	return nil
}

// replyRef decodes a message's stamped return address. A corrupt stamp is
// dropped rather than raised: it must never swallow the message itself.
func replyRef(stamped string) *protocol.AgentRef {
	if stamped == "" {
		return nil
	}
	ref := &protocol.AgentRef{}
	if json.Unmarshal([]byte(stamped), ref) != nil {
		return nil
	}
	return ref
}

// liveAgents lists the active or idle agent ids in a scope, minus the
// sender. Collected before any write: with SetMaxOpenConns(1) an open cursor
// holds the sole connection.
func (s *Store) liveAgents(q execQuerier, scope, exceptID string) ([]string, error) {
	rows, err := q.Query(`SELECT agent_id, status, last_seen FROM agents WHERE scope=? AND agent_id != ?`, scope, exceptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var aid, explicit string
		var seen int64
		if err := rows.Scan(&aid, &explicit, &seen); err != nil {
			return nil, err
		}
		if s.live(explicit, seen) {
			out = append(out, aid)
		}
	}
	return out, rows.Err()
}

// Send is a unicast delivery inside one workspace.
func (s *Store) Send(scope, fromName, toName, body string) error {
	return s.SendToScope(Delivery{FromScope: scope, FromName: fromName, ToName: toName, Body: body})
}

// Broadcast reaches everyone else live in the workspace.
func (s *Store) Broadcast(scope, fromName, body string) error {
	return s.SendToScope(Delivery{FromScope: scope, FromName: fromName, Body: body})
}

// senderJoin resolves a message's sender to a name in the SENDER's own
// workspace, so cross-scope mail still shows a name and mail outlives its
// sender's row (COALESCE falls back to the raw id). Every inbox read shares
// it, so none of them can answer with a different name.
const senderJoin = `LEFT JOIN agents a ON a.scope = COALESCE(NULLIF(m.from_scope, ''), m.scope) AND a.agent_id = m.from_agent`

func (s *Store) Read(scope, name string) ([]protocol.Message, error) {
	return s.read(scope, name, false)
}

// ReadBroker is Read for a host broker: a launch or a cancel is consumed like
// any other mail but handed back only by the redelivery queue, which alone
// knows whether it is still owed. A stale launch read after the cancel that
// called it off would start a provider nobody wants.
func (s *Store) ReadBroker(scope, name string) ([]protocol.Message, error) {
	return s.read(scope, name, true)
}

func (s *Store) read(scope, name string, skipLedger bool) ([]protocol.Message, error) {
	aid, _, err := s.resolveAgent(s.db, scope, name)
	if err != nil {
		return nil, err
	}
	// Collect+mark must be one transaction: with SetMaxOpenConns(1) the tx
	// holds the sole connection, so a concurrent Read cannot see the same
	// unread rows and double-deliver.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`
		SELECT m.id, COALESCE(a.name, m.from_agent), m.body, m.created_at, m.to_agent IS NULL,
		       m.reply_to, m.from_scope, m.kind, m.task_id, `+taskLedgerBody+`
		FROM deliveries d
		JOIN messages m ON m.id = d.message_id
		`+senderJoin+`
		WHERE d.agent_id = ? AND m.scope = ? AND d.read_at IS NULL
		ORDER BY m.created_at, m.id`, aid, scope)
	if err != nil {
		return nil, err
	}
	var out []protocol.Message
	var seen []int64 // every row read, including the ledger's own bodies
	for rows.Next() {
		var m protocol.Message
		var replyTo string
		var ledger bool
		if err := rows.Scan(&m.ID, &m.From, &m.Body, &m.SentAt, &m.Broadcast,
			&replyTo, &m.FromScope, &m.Kind, &m.TaskID, &ledger); err != nil {
			rows.Close()
			return nil, err
		}
		m.ReplyTo = replyRef(replyTo)
		seen = append(seen, m.ID)
		if ledger && skipLedger {
			continue // the queue hands this one out, if it is still owed
		}
		out = append(out, m)
	}
	rows.Close() // release the tx's connection before the UPDATEs below
	if err := rows.Err(); err != nil {
		return nil, err
	}
	now := s.Now().Unix()
	for _, id := range seen {
		if _, err := tx.Exec(`UPDATE deliveries SET read_at=? WHERE message_id=? AND agent_id=?`, now, id, aid); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// UnreadCount reports how many deliveries for the named agent are still
// unread in this scope. Strictly read-only - it never touches notice_sent_at,
// so peeking cannot consume the once-only nudge.
func (s *Store) UnreadCount(scope, name string) (int, error) {
	info, err := s.PeekMail(scope, name, 0)
	if err != nil {
		return 0, err
	}
	return info.Unread, nil
}

// PeekInfo is a read-only summary of an agent's inbox for OpPeek / wait.
type PeekInfo struct {
	Unread    int
	HighWater int64
	IDs       []int64
	Froms     []string // unique sender names among matching unread, stable order
}

// PeekMail reports unread deliveries with message id > afterID, plus the
// agent's high-water mark (max delivered message id, read or unread).
// Strictly read-only - never touches notice_sent_at or read_at.
func (s *Store) PeekMail(scope, name string, afterID int64) (PeekInfo, error) {
	return s.peekMail(scope, name, afterID, false)
}

// PeekBrokerMail is PeekMail for a host broker: the launch and the cancel are
// not unread mail to it, because the redelivery queue is what hands those out.
func (s *Store) PeekBrokerMail(scope, name string, afterID int64) (PeekInfo, error) {
	return s.peekMail(scope, name, afterID, true)
}

func (s *Store) peekMail(scope, name string, afterID int64, skipLedger bool) (PeekInfo, error) {
	aid, _, err := s.resolveAgent(s.db, scope, name)
	if err != nil {
		return PeekInfo{}, err
	}
	var high int64
	err = s.db.QueryRow(`
		SELECT COALESCE(MAX(m.id), 0) FROM deliveries d
		JOIN messages m ON m.id = d.message_id
		WHERE d.agent_id = ? AND m.scope = ?`, aid, scope).Scan(&high)
	if err != nil {
		return PeekInfo{}, err
	}
	skip := ""
	if skipLedger {
		skip = " AND NOT " + taskLedgerBody
	}
	rows, err := s.db.Query(`
		SELECT m.id, COALESCE(a.name, m.from_agent) FROM deliveries d
		JOIN messages m ON m.id = d.message_id
		`+senderJoin+`
		WHERE d.agent_id = ? AND m.scope = ? AND d.read_at IS NULL AND m.id > ?`+skip+`
		ORDER BY m.id`, aid, scope, afterID)
	if err != nil {
		return PeekInfo{}, err
	}
	defer rows.Close()
	var info PeekInfo
	info.HighWater = high
	seenFrom := map[string]bool{}
	for rows.Next() {
		var id int64
		var from string
		if err := rows.Scan(&id, &from); err != nil {
			return PeekInfo{}, err
		}
		info.IDs = append(info.IDs, id)
		if !seenFrom[from] {
			seenFrom[from] = true
			info.Froms = append(info.Froms, from)
		}
	}
	if err := rows.Err(); err != nil {
		return PeekInfo{}, err
	}
	info.Unread = len(info.IDs)
	return info, nil
}

// PendingNotices runs the notice collect+mark for the session's agent without
// recording any event or conflict check - the drain used by touchpoints that
// carry no work (Stop, UserPromptSubmit). Nudge-once by construction: the
// same notice_sent_at marking RecordEvent uses.
func (s *Store) PendingNotices(scope, sessionID string) ([]string, error) {
	return s.noticesFor(scope, agentID(sessionID), nil)
}

// noticesFor: unread-message notices (once per message) + conflict warnings.
func (s *Store) noticesFor(scope, aid string, writes []string) ([]string, error) {
	var notices []string
	now := s.Now().Unix()
	// Collect+mark must be one transaction: with SetMaxOpenConns(1) the tx
	// holds the sole connection, so concurrent RecordEvent calls cannot both
	// see the same undelivered rows and emit duplicate notices.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`
		SELECT m.id, COALESCE(a.name, m.from_agent), m.body, m.to_agent IS NULL
		FROM deliveries d
		JOIN messages m ON m.id = d.message_id
		`+senderJoin+`
		WHERE d.agent_id = ? AND m.scope = ? AND d.notice_sent_at IS NULL AND d.read_at IS NULL
		ORDER BY m.created_at, m.id`, aid, scope)
	if err != nil {
		return nil, err
	}
	// One notice per DM sender (ids + newest body preview); broadcasts keep a
	// line each with the same preview.
	type dmAgg struct {
		ids    []int64
		newest string
	}
	dms := map[string]*dmAgg{}
	var senders []string
	var ids []int64
	type bcast struct{ from, body string }
	var bcasts []bcast
	for rows.Next() {
		var id int64
		var from, body string
		var bc bool
		if err := rows.Scan(&id, &from, &body, &bc); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		if bc {
			bcasts = append(bcasts, bcast{from, body})
			continue
		}
		agg := dms[from]
		if agg == nil {
			agg = &dmAgg{}
			dms[from] = agg
			senders = append(senders, from)
		}
		agg.ids = append(agg.ids, id)
		agg.newest = body // rows arrive oldest-first, so the last one wins
	}
	rows.Close() // release the tx's connection before the UPDATEs below
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE deliveries SET notice_sent_at=? WHERE message_id=? AND agent_id=?`, now, id, aid); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, from := range senders {
		agg := dms[from]
		plural := ""
		if len(agg.ids) > 1 {
			plural = "s"
		}
		notices = append(notices, fmt.Sprintf("[coordinator] %d new message%s from %s (ids %s) \"%s\" - call read_messages",
			len(agg.ids), plural, from, joinIDs(agg.ids), preview(agg.newest)))
	}
	for _, b := range bcasts {
		notices = append(notices, fmt.Sprintf("[coordinator] broadcast from %s \"%s\" - call read_messages", b.from, preview(b.body)))
	}
	// Conflicts: other agents' recent writes to the same paths.
	cutoff := s.Now().Add(-conflictWindow).Unix()
	for _, p := range writes {
		crows, err := s.db.Query(`
			SELECT a.name, ft.ts FROM file_touches ft
			JOIN agents a ON a.scope = ft.scope AND a.agent_id = ft.agent_id
			WHERE ft.scope=? AND ft.path=? AND ft.agent_id != ? AND ft.ts >= ?`, scope, p, aid, cutoff)
		if err != nil {
			return nil, err
		}
		for crows.Next() {
			var name string
			var ts int64
			if err := crows.Scan(&name, &ts); err != nil {
				crows.Close()
				return nil, err
			}
			notices = append(notices, fmt.Sprintf("[coordinator] heads-up: %s also edited %s %s ago", name, p, age(now-ts)))
		}
		crows.Close()
		if err := crows.Err(); err != nil {
			return nil, err
		}
	}
	return notices, nil
}

// preview flattens a message body to one notice-safe line: newlines become
// spaces and anything past ~80 chars is clipped with "...".
func preview(body string) string {
	flat := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(body)
	r := []rune(flat)
	if len(r) <= 80 {
		return flat
	}
	return string(r[:80]) + "..."
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf("%d", id)
	}
	return strings.Join(parts, ",")
}

func age(secs int64) string {
	switch {
	case secs < 60:
		return fmt.Sprintf("%ds", secs)
	case secs < 3600:
		return fmt.Sprintf("%dm", secs/60)
	default:
		return fmt.Sprintf("%dh", secs/3600)
	}
}

func (s *Store) Housekeep() error {
	now := s.Now()
	// Fail what ran out of time before anything is purged: a deadline is at
	// most 30 minutes, well inside the two hours an agent row survives, so
	// the requester is still there to be told. A task the sweep could not
	// settle is reported at the end, never by skipping the purges below.
	_, expiry := s.ExpireEyesTasks(now)
	day := int64(86400)
	stmts := []struct {
		q   string
		arg int64
	}{
		{`DELETE FROM events WHERE ts < ?`, now.Unix() - 7*day},
		{`DELETE FROM file_touches WHERE ts < ?`, now.Add(-time.Hour).Unix()},
		{`DELETE FROM messages WHERE created_at < ? AND id IN (SELECT message_id FROM deliveries GROUP BY message_id HAVING COUNT(*) = SUM(read_at IS NOT NULL))`, now.Unix() - 7*day},
		{`DELETE FROM messages WHERE created_at < ?`, now.Unix() - 30*day},
		{`DELETE FROM deliveries WHERE message_id NOT IN (SELECT id FROM messages)`, 0},
		// Agents idle-decay to gone after 2h without a heartbeat; explicit gone
		// rows younger than that keep their board slot until they age out too.
		{`DELETE FROM agents WHERE last_seen < ?`, now.Add(-2 * time.Hour).Unix()},
		// Deliveries to a purged recipient can never be read again: drop them.
		{`DELETE FROM deliveries WHERE agent_id NOT IN (SELECT agent_id FROM agents)`, 0},
		// Claims never outlive their holder: drop rows whose holder was purged,
		// marked gone, or has decayed to gone.
		{`DELETE FROM claims WHERE NOT EXISTS (
			SELECT 1 FROM agents a WHERE a.scope = claims.scope AND a.agent_id = claims.agent_id
			AND a.status != 'gone' AND a.last_seen >= ?)`, now.Add(-staleWindow).Unix()},
		{`DELETE FROM tasks WHERE updated_at < ?`, now.Unix() - 7*day},
		// An eyes task outlives its messages by a day, long enough to explain
		// a late report and short enough to stay a ledger, not a log.
		{`DELETE FROM eyes_tasks WHERE updated_at < ?`, now.Unix() - day},
	}
	for _, st := range stmts {
		var err error
		if st.arg != 0 {
			_, err = s.db.Exec(st.q, st.arg)
		} else {
			_, err = s.db.Exec(st.q)
		}
		if err != nil {
			return err
		}
	}
	return expiry
}

func (s *Store) freshStatus(explicit string, lastSeen int64) string {
	if explicit == "gone" {
		return "gone"
	}
	age := s.Now().Sub(time.Unix(lastSeen, 0))
	switch {
	case age <= activeWindow && explicit != "idle":
		return "active"
	case age <= idleWindow:
		return "idle"
	case age <= staleWindow:
		return "stale"
	default:
		return "gone"
	}
}

func (s *Store) Agents(scope string) ([]protocol.AgentInfo, error) {
	all, err := s.Board(scope, false)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, a := range all {
		if a.Status == "active" || a.Status == "idle" {
			out = append(out, a)
		}
	}
	return out, nil
}

// Board lists the scope's agents; gone rows are hidden unless includeGone.
func (s *Store) Board(scope string, includeGone bool) ([]protocol.AgentInfo, error) {
	// Read every agent row and close the cursor BEFORE per-agent enrichment:
	// with SetMaxOpenConns(1) an open Rows holds the sole connection, so any
	// QueryRow issued mid-iteration would deadlock waiting for that connection.
	rows, err := s.db.Query(`SELECT session_id, agent_id, name, status, last_seen, parent_session_id, kind, origin, platform, caps FROM agents WHERE scope=? ORDER BY registered_at`, scope)
	if err != nil {
		return nil, err
	}
	var all []protocol.AgentInfo
	var parentSessions []string
	nameBySession := map[string]string{} // every row, so a hidden parent still names its children
	for rows.Next() {
		var sid, explicit, parentSession, capsJSON string
		var a protocol.AgentInfo
		if err := rows.Scan(&sid, &a.AgentID, &a.Name, &explicit, &a.LastSeen, &parentSession,
			&a.Kind, &a.Origin, &a.Platform, &capsJSON); err != nil {
			rows.Close()
			return nil, err
		}
		a.Status = s.freshStatus(explicit, a.LastSeen)
		json.Unmarshal([]byte(capsJSON), &a.Capabilities)
		nameBySession[sid] = a.Name
		parentSessions = append(parentSessions, parentSession)
		all = append(all, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var out []protocol.AgentInfo
	for i, a := range all {
		if ps := parentSessions[i]; ps != "" {
			a.Parent = nameBySession[ps] // empty if the parent row was purged
		}
		if a.Status == "gone" && !includeGone {
			continue
		}
		out = append(out, a)
	}

	for i := range out {
		a := &out[i]
		var filesJSON string
		if err := s.db.QueryRow(`SELECT activity, files FROM events WHERE scope=? AND agent_id=? ORDER BY ts DESC, id DESC LIMIT 1`,
			scope, a.AgentID).Scan(&a.Activity, &filesJSON); err == nil {
			json.Unmarshal([]byte(filesJSON), &a.Files)
		}
		s.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE scope=? AND agent_id=? AND status='pending'`, scope, a.AgentID).Scan(&a.TasksPending)
		s.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE scope=? AND agent_id=? AND status='completed'`, scope, a.AgentID).Scan(&a.TasksDone)
		s.db.QueryRow(`SELECT subject FROM tasks WHERE scope=? AND agent_id=? AND status='in_progress' ORDER BY updated_at DESC LIMIT 1`,
			scope, a.AgentID).Scan(&a.CurrentTask)
		if crows, err := s.db.Query(`SELECT path FROM claims WHERE scope=? AND agent_id=? ORDER BY since, path`, scope, a.AgentID); err == nil {
			for crows.Next() {
				var p string
				if crows.Scan(&p) == nil {
					a.Claims = append(a.Claims, p)
				}
			}
			crows.Close()
		}
	}
	return out, nil
}

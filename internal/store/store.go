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
	now := s.Now().Unix()
	var name string
	err := s.db.QueryRow(`SELECT name FROM agents WHERE scope=? AND session_id=?`, scope, sessionID).Scan(&name)
	if err == nil {
		_, err = s.db.Exec(`UPDATE agents SET status='active', last_seen=? WHERE scope=? AND session_id=?`, now, scope, sessionID)
		return name, err
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	base := friendlyName(sessionID)
	name = base
	for n := 2; n <= 50; n++ {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE scope=? AND name=?`, scope, name).Scan(&count); err != nil {
			return "", err
		}
		if count == 0 {
			_, err := s.db.Exec(`INSERT INTO agents (scope, session_id, agent_id, name, status, registered_at, last_seen, source)
				VALUES (?,?,?,?,'active',?,?,?)`, scope, sessionID, agentID(sessionID), name, now, now, source)
			if err == nil {
				return name, nil
			}
			if !isUniqueViolation(err) {
				return "", err
			}
			// Unique race: either a concurrent Register won this session's PK
			// (return its name) or took this name (try the next suffix).
			if e := s.db.QueryRow(`SELECT name FROM agents WHERE scope=? AND session_id=?`, scope, sessionID).Scan(&name); e == nil {
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
// (scope, child session); names are <parent>/<agent_type|sub>-<n>.
func (s *Store) RegisterChild(scope, parentSessionID, subagentID, agentType string) (string, error) {
	child := ChildSessionID(parentSessionID, subagentID)
	now := s.Now().Unix()
	var name string
	err := s.db.QueryRow(`SELECT name FROM agents WHERE scope=? AND session_id=?`, scope, child).Scan(&name)
	if err == nil {
		_, err = s.db.Exec(`UPDATE agents SET status='active', last_seen=? WHERE scope=? AND session_id=?`, now, scope, child)
		return name, err
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	// The parent anchors the child's name; registering it also keeps the
	// parent row fresh while its subagents work.
	parentName, err := s.Register(scope, parentSessionID, "hook")
	if err != nil {
		return "", err
	}
	typ := strings.ToLower(agentType)
	if typ == "" {
		typ = "sub"
	}
	for n := 1; n <= 50; n++ {
		name = fmt.Sprintf("%s/%s-%d", parentName, typ, n)
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE scope=? AND name=?`, scope, name).Scan(&count); err != nil {
			return "", err
		}
		if count > 0 {
			continue
		}
		_, err := s.db.Exec(`INSERT INTO agents (scope, session_id, agent_id, name, status, registered_at, last_seen, source, parent_session_id)
			VALUES (?,?,?,?,'active',?,?,'hook-subagent',?)`, scope, child, agentID(child), name, now, now, parentSessionID)
		if err == nil {
			return name, nil
		}
		if !isUniqueViolation(err) {
			return "", err
		}
		// Unique race: a concurrent RegisterChild won this child's PK (return
		// its name) or took this name (try the next suffix).
		if e := s.db.QueryRow(`SELECT name FROM agents WHERE scope=? AND session_id=?`, scope, child).Scan(&name); e == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("register child: no free name under %q in scope %q", parentName, scope)
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
// classifies legacy rows that predate the source column.
func (s *Store) hasLiveHookAgents(scope string) (bool, error) {
	rows, err := s.db.Query(`SELECT status, last_seen FROM agents WHERE scope=? AND session_id NOT LIKE 'mcp-%'`, scope)
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
		if st := s.freshStatus(explicit, seen); st == "active" || st == "idle" {
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
}

// Identity returns the stored identity for a session.
func (s *Store) Identity(scope, sessionID string) (AgentIdentity, error) {
	var id AgentIdentity
	var parentSession string
	err := s.db.QueryRow(`SELECT name, agent_id, source, parent_session_id FROM agents WHERE scope=? AND session_id=?`,
		scope, sessionID).Scan(&id.Name, &id.AgentID, &id.Source, &parentSession)
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

func (s *Store) resolveAgent(scope, nameOrID string) (aid, name string, err error) {
	err = s.db.QueryRow(`SELECT agent_id, name FROM agents WHERE scope=? AND (name=? OR agent_id=?)`,
		scope, nameOrID, nameOrID).Scan(&aid, &name)
	if err == sql.ErrNoRows {
		return "", "", fmt.Errorf("no agent %q in this workspace", nameOrID)
	}
	return aid, name, err
}

func (s *Store) Send(scope, fromName, toName, body string) error {
	fromID, _, err := s.resolveAgent(scope, fromName)
	if err != nil {
		return err
	}
	toID, _, err := s.resolveAgent(scope, toName)
	if err != nil {
		return err
	}
	now := s.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO messages (scope, from_agent, to_agent, body, created_at) VALUES (?,?,?,?,?)`,
		scope, fromID, toID, body, now)
	if err != nil {
		return err
	}
	mid, _ := res.LastInsertId()
	_, err = s.db.Exec(`INSERT INTO deliveries (message_id, agent_id) VALUES (?,?)`, mid, toID)
	return err
}

func (s *Store) Broadcast(scope, fromName, body string) error {
	fromID, _, err := s.resolveAgent(scope, fromName)
	if err != nil {
		return err
	}
	now := s.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO messages (scope, from_agent, to_agent, body, created_at) VALUES (?,?,NULL,?,?)`,
		scope, fromID, body, now)
	if err != nil {
		return err
	}
	mid, _ := res.LastInsertId()
	// NOTE: with db.SetMaxOpenConns(1), never Exec while a rows cursor is open -
	// collect first, Close, then write (same deadlock Task 5 hit in Board).
	rows, err := s.db.Query(`SELECT agent_id, status, last_seen FROM agents WHERE scope=? AND agent_id != ?`, scope, fromID)
	if err != nil {
		return err
	}
	var targets []string
	for rows.Next() {
		var aid, explicit string
		var seen int64
		if err := rows.Scan(&aid, &explicit, &seen); err != nil {
			rows.Close()
			return err
		}
		if st := s.freshStatus(explicit, seen); st == "active" || st == "idle" {
			targets = append(targets, aid)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, aid := range targets {
		if _, err := s.db.Exec(`INSERT INTO deliveries (message_id, agent_id) VALUES (?,?)`, mid, aid); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Read(scope, name string) ([]protocol.Message, error) {
	aid, _, err := s.resolveAgent(scope, name)
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
	// LEFT JOIN: mail must stay readable after its sender is purged, falling
	// back to the raw sender id as the label.
	rows, err := tx.Query(`
		SELECT m.id, COALESCE(a.name, m.from_agent), m.body, m.created_at, m.to_agent IS NULL
		FROM deliveries d
		JOIN messages m ON m.id = d.message_id
		LEFT JOIN agents a ON a.scope = m.scope AND a.agent_id = m.from_agent
		WHERE d.agent_id = ? AND m.scope = ? AND d.read_at IS NULL
		ORDER BY m.created_at, m.id`, aid, scope)
	if err != nil {
		return nil, err
	}
	var out []protocol.Message
	for rows.Next() {
		var m protocol.Message
		if err := rows.Scan(&m.ID, &m.From, &m.Body, &m.SentAt, &m.Broadcast); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close() // release the tx's connection before the UPDATEs below
	if err := rows.Err(); err != nil {
		return nil, err
	}
	now := s.Now().Unix()
	for _, m := range out {
		if _, err := tx.Exec(`UPDATE deliveries SET read_at=? WHERE message_id=? AND agent_id=?`, now, m.ID, aid); err != nil {
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
	aid, _, err := s.resolveAgent(scope, name)
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
	rows, err := s.db.Query(`
		SELECT m.id, COALESCE(a.name, m.from_agent) FROM deliveries d
		JOIN messages m ON m.id = d.message_id
		LEFT JOIN agents a ON a.scope = m.scope AND a.agent_id = m.from_agent
		WHERE d.agent_id = ? AND m.scope = ? AND d.read_at IS NULL AND m.id > ?
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
		LEFT JOIN agents a ON a.scope = m.scope AND a.agent_id = m.from_agent
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
	return nil
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
	rows, err := s.db.Query(`SELECT session_id, agent_id, name, status, last_seen, parent_session_id FROM agents WHERE scope=? ORDER BY registered_at`, scope)
	if err != nil {
		return nil, err
	}
	var all []protocol.AgentInfo
	var parentSessions []string
	nameBySession := map[string]string{} // every row, so a hidden parent still names its children
	for rows.Next() {
		var sid, explicit, parentSession string
		var a protocol.AgentInfo
		if err := rows.Scan(&sid, &a.AgentID, &a.Name, &explicit, &a.LastSeen, &parentSession); err != nil {
			rows.Close()
			return nil, err
		}
		a.Status = s.freshStatus(explicit, a.LastSeen)
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

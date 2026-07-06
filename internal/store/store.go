package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agent-coordinator/go/internal/protocol"
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
	for n := 2; ; n++ {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE scope=? AND name=?`, scope, name).Scan(&count); err != nil {
			return "", err
		}
		if count == 0 {
			break
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
	_, err = s.db.Exec(`INSERT INTO agents (scope, session_id, agent_id, name, status, registered_at, last_seen)
		VALUES (?,?,?,?,'active',?,?)`, scope, sessionID, agentID(sessionID), name, now, now)
	return name, err
}

func (s *Store) SetStatus(scope, sessionID, status string) error {
	_, err := s.db.Exec(`UPDATE agents SET status=?, last_seen=? WHERE scope=? AND session_id=?`,
		status, s.Now().Unix(), scope, sessionID)
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
	rows, err := s.db.Query(`
		SELECT m.id, a.name, m.body, m.created_at, m.to_agent IS NULL
		FROM deliveries d
		JOIN messages m ON m.id = d.message_id
		JOIN agents a ON a.scope = m.scope AND a.agent_id = m.from_agent
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
	rows.Close() // release the sole connection before the UPDATEs below
	if err := rows.Err(); err != nil {
		return nil, err
	}
	now := s.Now().Unix()
	for _, m := range out {
		if _, err := s.db.Exec(`UPDATE deliveries SET read_at=? WHERE message_id=? AND agent_id=?`, now, m.ID, aid); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// noticesFor: unread-message notices (once per message) + conflict warnings.
func (s *Store) noticesFor(scope, aid string, writes []string) ([]string, error) {
	var notices []string
	now := s.Now().Unix()
	rows, err := s.db.Query(`
		SELECT m.id, a.name, m.to_agent IS NULL
		FROM deliveries d
		JOIN messages m ON m.id = d.message_id
		JOIN agents a ON a.scope = m.scope AND a.agent_id = m.from_agent
		WHERE d.agent_id = ? AND m.scope = ? AND d.notice_sent_at IS NULL AND d.read_at IS NULL
		ORDER BY m.created_at`, aid, scope)
	if err != nil {
		return nil, err
	}
	dmCount := map[string]int{}
	var ids []int64
	var bcasts []string
	for rows.Next() {
		var id int64
		var from string
		var bc bool
		if err := rows.Scan(&id, &from, &bc); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		if bc {
			bcasts = append(bcasts, from)
		} else {
			dmCount[from]++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for from, n := range dmCount {
		plural := ""
		if n > 1 {
			plural = "s"
		}
		notices = append(notices, fmt.Sprintf("[coordinator] %d new message%s from %s - call read_messages", n, plural, from))
	}
	for _, from := range bcasts {
		notices = append(notices, fmt.Sprintf("[coordinator] broadcast from %s - call read_messages", from))
	}
	for _, id := range ids {
		if _, err := s.db.Exec(`UPDATE deliveries SET notice_sent_at=? WHERE message_id=? AND agent_id=?`, now, id, aid); err != nil {
			return nil, err
		}
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
		{`DELETE FROM agents WHERE last_seen < ?`, now.Unix() - 7*day},
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
	all, err := s.Board(scope)
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

func (s *Store) Board(scope string) ([]protocol.AgentInfo, error) {
	// Read every agent row and close the cursor BEFORE per-agent enrichment:
	// with SetMaxOpenConns(1) an open Rows holds the sole connection, so any
	// QueryRow issued mid-iteration would deadlock waiting for that connection.
	rows, err := s.db.Query(`SELECT session_id, agent_id, name, status, last_seen FROM agents WHERE scope=? ORDER BY registered_at`, scope)
	if err != nil {
		return nil, err
	}
	var out []protocol.AgentInfo
	for rows.Next() {
		var sid, explicit string
		var a protocol.AgentInfo
		if err := rows.Scan(&sid, &a.AgentID, &a.Name, &explicit, &a.LastSeen); err != nil {
			rows.Close()
			return nil, err
		}
		a.Status = s.freshStatus(explicit, a.LastSeen)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

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
	}
	return out, nil
}

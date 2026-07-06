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

// RecordEvent ingests one PostToolUse event and returns notices for the agent.
// Notice generation (messages, conflicts) is completed in Task 6; part 1
// returns nil notices.
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

// noticesFor is completed in Task 6.
func (s *Store) noticesFor(scope, agentID string, writes []string) ([]string, error) {
	return nil, nil
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

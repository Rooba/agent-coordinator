package store

import (
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
)

// columns lists a table's column names, sorted.
func columns(t *testing.T, s *Store, table string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func hasColumn(cols []string, want string) bool {
	i := sort.SearchStrings(cols, want)
	return i < len(cols) && cols[i] == want
}

// A database written before the relay columns existed must migrate in place:
// the old row survives on the new defaults and every column is present.
func TestRelayMigrationsOnLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE agents (
		scope TEXT NOT NULL, session_id TEXT NOT NULL,
		agent_id TEXT NOT NULL, name TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'active',
		registered_at INTEGER NOT NULL, last_seen INTEGER NOT NULL,
		PRIMARY KEY (scope, session_id));
		INSERT INTO agents VALUES ('/r','s-old','aid-old','old-agent','active',1,1);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open must migrate a legacy database: %v", err)
	}
	defer s.Close()
	agentCols := columns(t, s, "agents")
	for _, col := range []string{"kind", "origin", "platform", "caps", "relay_secret_hash"} {
		if !hasColumn(agentCols, col) {
			t.Errorf("agents is missing column %q", col)
		}
	}
	msgCols := columns(t, s, "messages")
	for _, col := range []string{"from_scope", "reply_to", "task_id", "kind"} {
		if !hasColumn(msgCols, col) {
			t.Errorf("messages is missing column %q", col)
		}
	}
	var kind, caps string
	if err := s.db.QueryRow(`SELECT kind, caps FROM agents WHERE session_id='s-old'`).Scan(&kind, &caps); err != nil {
		t.Fatalf("legacy row must survive the migration: %v", err)
	}
	if kind != "" || caps != "[]" {
		t.Fatalf("legacy row defaults: kind=%q caps=%q, want \"\" and []", kind, caps)
	}
}

// Reopening a migrated database must not error, and eyes_tasks must accept a
// row with only its required columns set.
func TestMigrationsIdempotentAndEyesTasksTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("second open must tolerate already-applied migrations: %v", err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`INSERT INTO eyes_tasks (task_id, requester_scope, requester_agent_id, launcher_session, created_at, updated_at)
		VALUES ('task-abc','/r','aid-a','broker-1',10,10)`); err != nil {
		t.Fatalf("eyes_tasks insert: %v", err)
	}
	var state, runtime string
	if err := s.db.QueryRow(`SELECT state, runtime FROM eyes_tasks WHERE task_id='task-abc'`).Scan(&state, &runtime); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || runtime != "" {
		t.Fatalf("eyes_tasks defaults: state=%q runtime=%q, want queued and \"\"", state, runtime)
	}
}

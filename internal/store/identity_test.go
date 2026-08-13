package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Pre-migration agents schema, as shipped before the source and
// parent_session_id columns existed.
const legacySchema = `
CREATE TABLE agents (
  scope TEXT NOT NULL, session_id TEXT NOT NULL,
  agent_id TEXT NOT NULL, name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',
  registered_at INTEGER NOT NULL, last_seen INTEGER NOT NULL,
  PRIMARY KEY (scope, session_id)
);
CREATE UNIQUE INDEX idx_agents_scope_name ON agents(scope, name);
`

func TestOpenMigratesLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agents (scope, session_id, agent_id, name, registered_at, last_seen)
		VALUES ('/r','legacy-sess','abc','old-owl',?,?)`, time.Now().Unix(), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open over legacy schema: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	// New columns exist and default empty; the legacy row is intact.
	id, err := s.Identity("/r", "legacy-sess")
	if err != nil || id.Name != "old-owl" || id.Source != "" || id.Parent != "" {
		t.Fatalf("legacy identity: %+v err=%v", id, err)
	}
	// Re-opening (both ALTERs now duplicate) must still succeed.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen migrated db: %v", err)
	}
	s2.Close()
}

func TestRegisterPersistsSource(t *testing.T) {
	s := open(t)
	name, err := s.Register("/r", "sess-1", "hook")
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Identity("/r", "sess-1")
	if err != nil || id.Name != name || id.Source != "hook" || id.AgentID == "" {
		t.Fatalf("identity: %+v err=%v", id, err)
	}
	// Re-register must not overwrite the original provenance.
	if _, err := s.Register("/r", "sess-1", "mcp:claude"); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.Identity("/r", "sess-1"); id.Source != "hook" {
		t.Fatalf("re-register overwrote source: %+v", id)
	}
}

func TestIdentityUnknownSession(t *testing.T) {
	s := open(t)
	if _, err := s.Identity("/r", "nope"); err == nil {
		t.Fatal("want error for unknown session")
	}
}

func TestHasLiveHookAgents(t *testing.T) {
	s := open(t)
	if live, err := s.hasLiveHookAgents("/r"); err != nil || live {
		t.Fatalf("empty scope: live=%v err=%v", live, err)
	}
	// A self-minted MCP identity is not a hook row.
	s.Register("/r", "mcp-deadbeef", "mcp:codex")
	if live, _ := s.hasLiveHookAgents("/r"); live {
		t.Fatal("mcp- row must not count as hook")
	}
	s.Register("/r", "claude-uuid-1", "hook")
	if live, _ := s.hasLiveHookAgents("/r"); !live {
		t.Fatal("hook row must count")
	}
	// A gone hook row is not live.
	s.SetStatus("/r", "claude-uuid-1", "gone")
	if live, _ := s.hasLiveHookAgents("/r"); live {
		t.Fatal("gone hook row must not count")
	}
}

func TestRegisterIfNoLiveHook(t *testing.T) {
	s := open(t)
	// Empty scope: mints as usual (hookless-harness path).
	name, err := s.RegisterIfNoLiveHook("/r", "mcp-1111", "mcp:codex")
	if err != nil || name == "" {
		t.Fatalf("hookless mint: %q %v", name, err)
	}
	// Live hook row present: a NEW identity is refused, fail closed.
	s.Register("/r", "claude-uuid-1", "hook")
	if _, err := s.RegisterIfNoLiveHook("/r", "mcp-2222", "mcp:codex"); !errors.Is(err, ErrIdentityUnknown) {
		t.Fatalf("want ErrIdentityUnknown, got %v", err)
	}
	// An EXISTING row refreshes fine even while hook rows are live.
	again, err := s.RegisterIfNoLiveHook("/r", "mcp-1111", "mcp:codex")
	if err != nil || again != name {
		t.Fatalf("existing row refresh: %q %v", again, err)
	}
}

func TestIdentityParentName(t *testing.T) {
	s := open(t)
	parent, _ := s.Register("/r", "parent-sess", "hook")
	s.Register("/r", "parent-sess/agent-1", "hook-subagent")
	// Simulate what child registration (next task) will store.
	if _, err := s.db.Exec(`UPDATE agents SET parent_session_id='parent-sess' WHERE session_id='parent-sess/agent-1'`); err != nil {
		t.Fatal(err)
	}
	id, err := s.Identity("/r", "parent-sess/agent-1")
	if err != nil || id.Parent != parent {
		t.Fatalf("want parent %q, got %+v err=%v", parent, id, err)
	}
}

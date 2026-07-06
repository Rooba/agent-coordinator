package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-coordinator/go/internal/protocol"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRegisterAssignsStableUniqueNames(t *testing.T) {
	s := open(t)
	n1, err := s.Register("/repo", "sess-1", "startup")
	if err != nil || n1 == "" {
		t.Fatalf("register: %v %q", err, n1)
	}
	n1b, _ := s.Register("/repo", "sess-1", "resume") // same session re-registers
	if n1b != n1 {
		t.Fatalf("name not stable across re-register: %q vs %q", n1, n1b)
	}
	n2, _ := s.Register("/repo", "sess-2", "startup")
	if n2 == n1 {
		t.Fatal("two sessions got the same name")
	}
}

// Regression: two sessions whose friendlyName collides must get distinct
// names, backed by the unique (scope, name) index.
func TestRegisterCollidingFriendlyNames(t *testing.T) {
	s := open(t)
	base := friendlyName("seed-0")
	other := ""
	for i := 1; i < 100000; i++ {
		if id := fmt.Sprintf("seed-%d", i); friendlyName(id) == base {
			other = id
			break
		}
	}
	if other == "" {
		t.Fatal("no colliding session id found")
	}
	n1, err := s.Register("/r", "seed-0", "startup")
	if err != nil || n1 != base {
		t.Fatalf("first register: %q %v", n1, err)
	}
	n2, err := s.Register("/r", other, "startup")
	if err != nil || n2 != base+"-2" {
		t.Fatalf("colliding register must take next suffix, got %q %v", n2, err)
	}
	// The index itself must reject a duplicate name, and Register's retry
	// must recognize the driver's unique-violation error.
	_, err = s.db.Exec(`INSERT INTO agents (scope, session_id, agent_id, name, status, registered_at, last_seen)
		VALUES ('/r','dupe','dupe',?, 'active',0,0)`, base)
	if !isUniqueViolation(err) {
		t.Fatalf("want unique violation on duplicate name, got %v", err)
	}
}

func TestScopeIsolation(t *testing.T) {
	s := open(t)
	s.Register("/repo-a", "sa", "startup")
	s.Register("/repo-b", "sb", "startup")
	agents, err := s.Agents("/repo-a")
	if err != nil || len(agents) != 1 {
		t.Fatalf("want 1 agent in /repo-a, got %d (%v)", len(agents), err)
	}
}

func TestPresenceFreshness(t *testing.T) {
	s := open(t)
	now := time.Unix(1000000, 0)
	s.Now = func() time.Time { return now }
	s.Register("/r", "s1", "startup")
	now = now.Add(20 * time.Minute) // beyond idle window, inside stale window
	agents, _ := s.Agents("/r")
	if len(agents) != 0 {
		t.Fatalf("agent past 15m must drop off Agents(), got %d", len(agents))
	}
	board, _ := s.Board("/r")
	if len(board) != 1 || board[0].Status != "stale" {
		t.Fatalf("board must still show it as stale: %+v", board)
	}
}

func TestEventUpdatesBoardAndTasks(t *testing.T) {
	s := open(t)
	s.Register("/r", "s1", "startup")
	_, err := s.RecordEvent("/r", "s1", protocol.Request{
		Tool: "Edit", Activity: "Editing a.go", Files: []string{"/r/a.go"}, Writes: []string{"/r/a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	s.RecordEvent("/r", "s1", protocol.Request{Tool: "TaskCreate", Activity: "Planning: x",
		TaskEv: &protocol.TaskEvent{Kind: "create", Key: "1", Subject: "x", Status: "pending"}})
	s.RecordEvent("/r", "s1", protocol.Request{Tool: "TaskUpdate", Activity: "Updating task 1 -> in_progress",
		TaskEv: &protocol.TaskEvent{Kind: "update", Key: "1", Status: "in_progress"}})
	board, _ := s.Board("/r")
	if len(board) != 1 {
		t.Fatalf("want 1 agent, got %d", len(board))
	}
	a := board[0]
	if a.Activity != "Updating task 1 -> in_progress" || a.CurrentTask != "x" || a.TasksPending != 0 || a.TasksDone != 0 {
		t.Fatalf("board row wrong: %+v", a)
	}
	s.RecordEvent("/r", "s1", protocol.Request{Tool: "TaskUpdate", Activity: "Updating task 1 -> completed",
		TaskEv: &protocol.TaskEvent{Kind: "update", Key: "1", Status: "completed"}})
	board, _ = s.Board("/r")
	if board[0].TasksDone != 1 || board[0].CurrentTask != "" {
		t.Fatalf("completion not reflected: %+v", board[0])
	}
}

func TestEventAutoRegisters(t *testing.T) {
	s := open(t)
	// Hook may fire PostToolUse before SessionStart ever reached us (daemon was down).
	if _, err := s.RecordEvent("/r", "ghost", protocol.Request{Tool: "Read", Activity: "Reading f"}); err != nil {
		t.Fatal(err)
	}
	agents, _ := s.Agents("/r")
	if len(agents) != 1 {
		t.Fatal("event must auto-register unknown session")
	}
}

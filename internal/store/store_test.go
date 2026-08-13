package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
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
	board, _ := s.Board("/r", false)
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
	board, _ := s.Board("/r", false)
	if len(board) != 1 {
		t.Fatalf("want 1 agent, got %d", len(board))
	}
	a := board[0]
	if a.Activity != "Updating task 1 -> in_progress" || a.CurrentTask != "x" || a.TasksPending != 0 || a.TasksDone != 0 {
		t.Fatalf("board row wrong: %+v", a)
	}
	s.RecordEvent("/r", "s1", protocol.Request{Tool: "TaskUpdate", Activity: "Updating task 1 -> completed",
		TaskEv: &protocol.TaskEvent{Kind: "update", Key: "1", Status: "completed"}})
	board, _ = s.Board("/r", false)
	if board[0].TasksDone != 1 || board[0].CurrentTask != "" {
		t.Fatalf("completion not reflected: %+v", board[0])
	}
}

func TestEventReplacesTaskSnapshot(t *testing.T) {
	s := open(t)
	s.Register("/r", "s1", "startup")
	_, err := s.RecordEvent("/r", "s1", protocol.Request{
		Tool:         "update_plan",
		Activity:     "Working on: Patch",
		ReplaceTasks: true,
		Tasks: []protocol.TaskEvent{
			{Kind: "upsert", Key: "plan-0", Subject: "Inspect", Status: "completed"},
			{Kind: "upsert", Key: "plan-1", Subject: "Patch", Status: "in_progress"},
			{Kind: "upsert", Key: "plan-2", Subject: "Test", Status: "pending"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	board, _ := s.Board("/r", false)
	if len(board) != 1 || board[0].CurrentTask != "Patch" || board[0].TasksPending != 1 || board[0].TasksDone != 1 {
		t.Fatalf("snapshot not reflected: %+v", board)
	}
	_, err = s.RecordEvent("/r", "s1", protocol.Request{
		Tool: "update_plan", Activity: "Updating plan", ReplaceTasks: true,
		Tasks: []protocol.TaskEvent{{Kind: "upsert", Key: "plan-0", Subject: "Done", Status: "completed"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	board, _ = s.Board("/r", false)
	if board[0].CurrentTask != "" || board[0].TasksPending != 0 || board[0].TasksDone != 1 {
		t.Fatalf("replacement left stale tasks: %+v", board[0])
	}
}

func TestBoardExcludesGoneByDefault(t *testing.T) {
	s := open(t)
	s.Register("/r", "s-live", "startup")
	gone, _ := s.Register("/r", "s-gone", "startup")
	s.SetStatus("/r", "s-gone", "gone")
	board, err := s.Board("/r", false)
	if err != nil || len(board) != 1 || board[0].Status == "gone" {
		t.Fatalf("default board must hide gone rows: %+v err=%v", board, err)
	}
	board, err = s.Board("/r", true)
	if err != nil || len(board) != 2 {
		t.Fatalf("include_gone board must show all rows: %+v err=%v", board, err)
	}
	found := false
	for _, a := range board {
		if a.Name == gone && a.Status == "gone" {
			found = true
		}
	}
	if !found {
		t.Fatalf("gone row missing from full board: %+v", board)
	}
}

func TestHousekeepPurgesLongGoneAgents(t *testing.T) {
	s := open(t)
	now := time.Unix(3000000, 0)
	s.Now = func() time.Time { return now }
	s.Register("/r", "s-old", "startup") // last_seen stays 3h behind
	now = now.Add(2 * time.Hour)
	s.Register("/r", "s-gone-recent", "startup")
	s.SetStatus("/r", "s-gone-recent", "gone") // explicit gone, only 1h old at sweep
	now = now.Add(time.Hour)
	live, _ := s.Register("/r", "s-live", "startup")
	if err := s.Housekeep(); err != nil {
		t.Fatal(err)
	}
	board, _ := s.Board("/r", true)
	names := map[string]bool{}
	for _, a := range board {
		names[a.Name] = true
	}
	if len(board) != 2 || !names[live] {
		t.Fatalf("want live + recent-gone rows to survive: %+v", board)
	}
	if id, err := s.Identity("/r", "s-old"); err == nil {
		t.Fatalf("3h-gone agent must be purged, still have %+v", id)
	}
	if _, err := s.Identity("/r", "s-gone-recent"); err != nil {
		t.Fatalf("1h-old gone agent must survive: %v", err)
	}
}

func TestTouchLiftsIdleButNotGone(t *testing.T) {
	s := open(t)
	s.Register("/r", "s1", "startup")
	s.SetStatus("/r", "s1", "idle")
	if err := s.Touch("/r", "s1"); err != nil {
		t.Fatal(err)
	}
	board, _ := s.Board("/r", true)
	if len(board) != 1 || board[0].Status != "active" {
		t.Fatalf("touch must lift sticky idle to active: %+v", board)
	}
	s.SetStatus("/r", "s1", "gone")
	if err := s.Touch("/r", "s1"); err != nil {
		t.Fatal(err)
	}
	board, _ = s.Board("/r", true)
	if len(board) != 1 || board[0].Status != "gone" {
		t.Fatalf("touch must never resurrect a gone row: %+v", board)
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

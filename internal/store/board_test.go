package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

func TestBoardEmptyParentDoesNotJoinEmptySession(t *testing.T) {
	s := open(t)
	for _, session := range []string{"", "root"} {
		if _, err := s.Register("/r", session, "hook"); err != nil {
			t.Fatal(err)
		}
	}
	board, err := s.Board("/r", false)
	if err != nil || len(board) != 2 {
		t.Fatalf("board=%+v err=%v", board, err)
	}
	for _, a := range board {
		if a.Parent != "" {
			t.Fatalf("root agent gained a parent: %+v", a)
		}
	}
}

func TestBoardSnapshotKeepsScopedLatestState(t *testing.T) {
	s := open(t)
	s.Now = func() time.Time { return time.Unix(1000, 0) }
	parent, err := s.Register("/r", "parent", "hook")
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := s.RegisterChild("/r", "parent", "child", "worker")
	if err != nil {
		t.Fatal(err)
	}
	id := agentID(ChildSessionID("parent", "child"))
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	// The parent is hidden but still names the child. Equal event timestamps
	// select the later inserted event; foreign-scope enrichment never leaks in.
	exec(`UPDATE agents SET status='gone' WHERE scope='/r' AND session_id='parent'`)
	for _, event := range []struct{ scope, activity, files string }{
		{"/r", "old", `["old.go"]`},
		{"/r", "latest", `["new.go"]`},
		{"/foreign", "foreign", `["secret.go"]`},
	} {
		exec(`INSERT INTO events(scope,agent_id,tool,activity,files,ts) VALUES(?,?,'Read',?,?,1000)`,
			event.scope, id, event.activity, event.files)
	}
	for _, task := range []struct {
		scope, key, status string
		updated            int
	}{
		{"/r", "pending", "pending", 1}, {"/r", "done", "completed", 1},
		{"/r", "old task", "in_progress", 1}, {"/r", "current task", "in_progress", 2},
		{"/foreign", "foreign task", "in_progress", 3}, {"/foreign", "foreign pending", "pending", 3},
	} {
		exec(`INSERT INTO tasks(scope,agent_id,task_key,subject,status,updated_at) VALUES(?,?,?,?,?,?)`,
			task.scope, id, task.key, task.key, task.status, task.updated)
	}
	for _, claim := range []struct {
		scope, path string
		since       int
	}{
		{"/r", "z.go", 1}, {"/r", "b.go", 2}, {"/r", "a.go", 2}, {"/foreign", "secret.go", 0},
	} {
		exec(`INSERT INTO claims(scope,path,agent_id,note,since) VALUES(?,?,?,'',?)`, claim.scope, claim.path, id, claim.since)
	}

	board, err := s.Board("/r", false)
	if err != nil || len(board) != 1 {
		t.Fatalf("board=%+v err=%v", board, err)
	}
	a := board[0]
	if a.Name != child || a.Parent != parent || a.Activity != "latest" ||
		!reflect.DeepEqual(a.Files, []string{"new.go"}) || a.TasksPending != 1 ||
		a.TasksDone != 1 || a.CurrentTask != "current task" ||
		!reflect.DeepEqual(a.Claims, []string{"z.go", "a.go", "b.go"}) {
		t.Fatalf("wrong snapshot: %+v", a)
	}
	// Once the parent is purged, an unenriched live row and an orphaned child
	// must both remain visible, with an empty parent and empty optional fields.
	exec(`DELETE FROM agents WHERE scope='/r' AND session_id='parent'`)
	s.Now = func() time.Time { return time.Unix(1001, 0) }
	plain, err := s.Register("/r", "plain", "hook")
	if err != nil {
		t.Fatal(err)
	}
	board, err = s.Board("/r", false)
	if err != nil || len(board) != 2 || board[0].Name != child || board[1].Name != plain {
		t.Fatalf("registration ordering changed: board=%+v err=%v", board, err)
	}
	if board[0].Parent != "" {
		t.Fatalf("purged parent remained: %+v", board[0])
	}
	got := board[1]
	want := protocol.AgentInfo{AgentID: agentID("plain"), Name: plain, Status: "active", LastSeen: 1001}
	if len(got.Capabilities)+len(got.Files)+len(got.Claims) != 0 {
		t.Fatalf("unenriched row gained optional fields: %+v", got)
	}
	// Empty JSON arrays are omitted by the wire protocol, as are nil slices.
	got.Capabilities, got.Files, got.Claims = nil, nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unenriched row=%+v want=%+v", got, want)
	}
}

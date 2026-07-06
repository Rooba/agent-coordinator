package activity

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestInfer(t *testing.T) {
	cases := []struct {
		tool     string
		input    map[string]any
		activity string
		files    []string
		writes   []string
	}{
		{"Read", map[string]any{"file_path": "/r/src/main.go"}, "Reading main.go", []string{"/r/src/main.go"}, nil},
		{"Edit", map[string]any{"file_path": "/r/a.py"}, "Editing a.py", []string{"/r/a.py"}, []string{"/r/a.py"}},
		{"Write", map[string]any{"file_path": "/r/b.md"}, "Writing b.md", []string{"/r/b.md"}, []string{"/r/b.md"}},
		{"NotebookEdit", map[string]any{"notebook_path": "/r/n.ipynb"}, "Editing notebook n.ipynb", []string{"/r/n.ipynb"}, []string{"/r/n.ipynb"}},
		{"Bash", map[string]any{"command": "go test ./... && echo done padding padding"}, "Running: go test ./... && echo done padding paddi...", nil, nil},
		{"Grep", map[string]any{"pattern": "TODO"}, "Searching for 'TODO'", nil, nil},
		{"Glob", map[string]any{"pattern": "**/*.go"}, "Globbing '**/*.go'", nil, nil},
		{"Agent", map[string]any{"description": "Survey repo"}, "Delegating: Survey repo", nil, nil},
		{"WebSearch", map[string]any{"query": "sqlite wal"}, "Searching web: sqlite wal", nil, nil},
		{"WebFetch", map[string]any{"url": "https://x.test/y"}, "Fetching x.test", nil, nil},
		{"TaskCreate", map[string]any{"subject": "Fix login"}, "Planning: Fix login", nil, nil},
		{"mcp__linear__list_issues", nil, "linear: list issues", nil, nil},
		{"SomethingNew", nil, "Something new", nil, nil},
	}
	for _, c := range cases {
		a, f, w := Infer(c.tool, c.input)
		if a != c.activity || !reflect.DeepEqual(f, c.files) || !reflect.DeepEqual(w, c.writes) {
			t.Errorf("%s: got (%q,%v,%v) want (%q,%v,%v)", c.tool, a, f, w, c.activity, c.files, c.writes)
		}
	}
}

func TestTaskSignal(t *testing.T) {
	ev := TaskSignal("TaskUpdate", map[string]any{"taskId": "3", "status": "completed"}, nil)
	if ev == nil || ev.Kind != "update" || ev.Key != "3" || ev.Status != "completed" {
		t.Fatalf("got %+v", ev)
	}
	// Real TaskCreate tool_response shape, captured in spike fixture post_taskcreate.json.
	ev = TaskSignal("TaskCreate", map[string]any{"subject": "spike probe"},
		json.RawMessage(`{"task":{"id":"1","subject":"spike probe"}}`))
	if ev == nil || ev.Kind != "create" || ev.Key != "1" || ev.Subject != "spike probe" {
		t.Fatalf("got %+v", ev)
	}
	if TaskSignal("Read", map[string]any{}, nil) != nil {
		t.Fatal("Read must not produce a task signal")
	}
}

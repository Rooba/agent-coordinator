package activity

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/agent-coordinator/go/internal/protocol"
)

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Infer maps a tool call to (human activity, files touched, files written).
// Deviation from spec: Bash contributes activity only - extracting reliable
// file paths from shell strings is not worth the false positives.
func Infer(tool string, input map[string]any) (string, []string, []string) {
	fp := str(input, "file_path")
	switch tool {
	case "Read":
		return "Reading " + base(fp), one(fp), nil
	case "Edit":
		return "Editing " + base(fp), one(fp), one(fp)
	case "Write":
		return "Writing " + base(fp), one(fp), one(fp)
	case "NotebookEdit":
		np := str(input, "notebook_path")
		return "Editing notebook " + base(np), one(np), one(np)
	case "Bash":
		return "Running: " + truncate(str(input, "command"), 40), nil, nil
	case "Grep":
		return "Searching for '" + str(input, "pattern") + "'", nil, nil
	case "Glob":
		return "Globbing '" + str(input, "pattern") + "'", nil, nil
	case "Agent", "Task":
		return "Delegating: " + str(input, "description"), nil, nil
	case "WebSearch":
		return "Searching web: " + str(input, "query"), nil, nil
	case "WebFetch":
		return "Fetching " + host(str(input, "url")), nil, nil
	case "TaskCreate":
		return "Planning: " + str(input, "subject"), nil, nil
	case "TaskUpdate":
		return "Updating task " + str(input, "taskId") + " -> " + str(input, "status"), nil, nil
	}
	if rest, ok := strings.CutPrefix(tool, "mcp__"); ok {
		parts := strings.SplitN(rest, "__", 2)
		if len(parts) == 2 {
			return parts[0] + ": " + strings.ReplaceAll(parts[1], "_", " "), nil, nil
		}
	}
	return humanize(tool), nil, nil
}

// TaskSignal extracts a task event from TaskCreate/TaskUpdate calls.
func TaskSignal(tool string, input map[string]any, response json.RawMessage) *protocol.TaskEvent {
	switch tool {
	case "TaskCreate":
		key := keyFromResponse(response)
		return &protocol.TaskEvent{Kind: "create", Key: key, Subject: str(input, "subject"), Status: "pending"}
	case "TaskUpdate":
		st := str(input, "status")
		if st == "" {
			return nil // metadata-only update, ignore
		}
		return &protocol.TaskEvent{Kind: "update", Key: str(input, "taskId"), Status: st}
	}
	return nil
}

// keyFromResponse parses the TaskCreate tool response. Real shape captured in
// spike fixture post_taskcreate.json: {"task":{"id":"1","subject":"..."}}.
func keyFromResponse(response json.RawMessage) string {
	var r struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(response, &r); err == nil {
		return r.Task.ID
	}
	return ""
}

func base(p string) string {
	if p == "" {
		return "file"
	}
	return filepath.Base(p)
}

func one(p string) []string {
	if p == "" {
		return nil
	}
	return []string{p}
}

func host(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// humanize splits CamelCase ("SomethingNew" -> "Something new").
func humanize(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte(' ')
			b.WriteRune(r + 32)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

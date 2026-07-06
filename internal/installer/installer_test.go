package installer

import (
	"encoding/json"
	"strings"
	"testing"
)

const existing = `{
  "model": "claude-fable-5[1m]",
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "~/.claude/hooks/cbm-session-reminder"}]}
    ]
  },
  "effortLevel": "xhigh"
}`

func TestMergePreservesExistingAndIsIdempotent(t *testing.T) {
	out, changed, err := MergeHooks([]byte(existing), "/usr/local/bin/agent-coordinator")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	s := string(out)
	for _, want := range []string{"cbm-session-reminder", "agent-coordinator hook", "PostToolUse", "SessionEnd", "Stop", `"effortLevel": "xhigh"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	out2, changed2, err := MergeHooks(out, "/usr/local/bin/agent-coordinator")
	if err != nil || changed2 {
		t.Fatalf("second merge must be a no-op, changed=%v err=%v", changed2, err)
	}
	if string(out2) != string(out) {
		t.Fatal("idempotent merge altered output")
	}
}

func TestMergeFromEmptySettings(t *testing.T) {
	out, changed, err := MergeHooks([]byte("{}"), "/bin/ac")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !strings.Contains(string(out), "/bin/ac hook") {
		t.Fatalf("got %s", out)
	}
}

func TestRemoveHooksOnlyOurs(t *testing.T) {
	merged, _, _ := MergeHooks([]byte(existing), "/bin/ac")
	out, changed, err := RemoveHooks(merged, "/bin/ac")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	s := string(out)
	if strings.Contains(s, "/bin/ac hook") {
		t.Fatal("our hooks not removed")
	}
	if !strings.Contains(s, "cbm-session-reminder") {
		t.Fatal("removed a hook that is not ours")
	}
}

func TestUnitFiles(t *testing.T) {
	sock, svc := UnitFiles("/bin/ac")
	if !strings.Contains(sock, "ListenStream=%t/agent-coordinator.sock") {
		t.Fatalf("socket unit: %s", sock)
	}
	if !strings.Contains(svc, "ExecStart=/bin/ac daemon") {
		t.Fatalf("service unit: %s", svc)
	}
}

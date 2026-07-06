package installer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

func TestInstallUninstallRoundTrip(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const foreignCmd = "sh -c 'cat a.txt > out.log && echo done'"
	seed := `{
  "model": "claude-fable-5[1m]",
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "~/.claude/hooks/cbm-session-reminder"}]}
    ],
    "PostToolUse": [
      {"matcher": "", "hooks": [{"type": "command", "command": "` + foreignCmd + `"}]}
    ]
  },
  "effortLevel": "xhigh"
}`
	settingsPath := filepath.Join(claudeDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls [][]string
	fakeRun := func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}

	if err := Install("/bin/ac", home, fakeRun); err != nil {
		t.Fatalf("Install: %v", err)
	}

	unitDir := filepath.Join(home, ".config", "systemd", "user")
	sock, err := os.ReadFile(filepath.Join(unitDir, "agent-coordinator.socket"))
	if err != nil {
		t.Fatalf("socket unit: %v", err)
	}
	if !strings.Contains(string(sock), "ListenStream=%t/agent-coordinator.sock") {
		t.Fatalf("socket unit content: %s", sock)
	}
	svc, err := os.ReadFile(filepath.Join(unitDir, "agent-coordinator.service"))
	if err != nil {
		t.Fatalf("service unit: %v", err)
	}
	if !strings.Contains(string(svc), "ExecStart=/bin/ac daemon") {
		t.Fatalf("service unit content: %s", svc)
	}

	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(after)
	if got := strings.Count(s, `"/bin/ac hook"`); got != 4 {
		t.Fatalf("want 4 coordinator hook entries, got %d in:\n%s", got, s)
	}
	for _, event := range []string{"SessionStart", "PostToolUse", "Stop", "SessionEnd"} {
		if !strings.Contains(s, `"`+event+`"`) {
			t.Fatalf("missing event %q in:\n%s", event, s)
		}
	}
	for _, foreign := range []string{foreignCmd, "~/.claude/hooks/cbm-session-reminder"} {
		if !strings.Contains(s, foreign) {
			t.Fatalf("foreign command not preserved byte-identical, missing %q in:\n%s", foreign, s)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(after, &m); err != nil {
		t.Fatalf("settings not valid JSON after install: %v", err)
	}
	for _, want := range [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "--now", "agent-coordinator.socket"},
		{"claude", "mcp", "add", "--scope", "user", "--transport", "stdio", "agent-coordinator", "--", "/bin/ac", "mcp"},
	} {
		if !hasCall(calls, want) {
			t.Fatalf("missing recorded command %v in %v", want, calls)
		}
	}

	calls = nil
	if err := Uninstall("/bin/ac", home, fakeRun); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	after, err = os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	s = string(after)
	if strings.Contains(s, "/bin/ac hook") {
		t.Fatalf("coordinator hooks not removed:\n%s", s)
	}
	for _, foreign := range []string{foreignCmd, "~/.claude/hooks/cbm-session-reminder"} {
		if !strings.Contains(s, foreign) {
			t.Fatalf("uninstall dropped foreign command %q:\n%s", foreign, s)
		}
	}
	for _, unit := range []string{"agent-coordinator.socket", "agent-coordinator.service"} {
		if _, err := os.Stat(filepath.Join(unitDir, unit)); !os.IsNotExist(err) {
			t.Fatalf("unit file %s still present (stat err=%v)", unit, err)
		}
	}
	for _, want := range [][]string{
		{"systemctl", "--user", "disable", "--now", "agent-coordinator.socket"},
		{"claude", "mcp", "remove", "--scope", "user", "agent-coordinator"},
	} {
		if !hasCall(calls, want) {
			t.Fatalf("missing recorded command %v in %v", want, calls)
		}
	}
}

func hasCall(calls [][]string, want []string) bool {
	for _, c := range calls {
		if slices.Equal(c, want) {
			return true
		}
	}
	return false
}

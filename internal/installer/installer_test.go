package installer

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	for _, want := range []string{"cbm-session-reminder", "agent-coordinator hook", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SubagentStart", "SubagentStop", "SessionEnd", "Stop", `"effortLevel": "xhigh"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	// Bare "Read" in the PreToolUse matcher would spawn the hook on every file read.
	if !strings.Contains(s, `"mcp__agent-coordinator__read_messages|read_messages"`) || strings.Contains(s, "|Read|") {
		t.Fatalf("PreToolUse matcher must target read_messages only, not bare Read:\n%s", s)
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

// Regression: a settings.json containing the JSON literal null must not panic.
func TestMergeHooksNullSettings(t *testing.T) {
	out, changed, err := MergeHooks([]byte("null"), "/bin/ac")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !strings.Contains(string(out), "/bin/ac hook") {
		t.Fatalf("got %s", out)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
}

func TestRemoveHooksNullSettings(t *testing.T) {
	out, changed, err := RemoveHooks([]byte("null"), "/bin/ac")
	if err != nil || changed || string(out) != "null" {
		t.Fatalf("want unchanged null, got changed=%v err=%v out=%s", changed, err, out)
	}
}

func TestCodexHooksUseSupportedLifecycleEvents(t *testing.T) {
	out, changed, err := MergeCodexHooks([]byte(`{"description":"keep"}`), "/bin/ac")
	if err != nil || !changed {
		t.Fatalf("MergeCodexHooks: changed=%v err=%v", changed, err)
	}
	s := string(out)
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop"} {
		if !strings.Contains(s, `"`+event+`"`) {
			t.Fatalf("missing Codex event %q in:\n%s", event, s)
		}
	}
	if strings.Contains(s, `"SessionEnd"`) {
		t.Fatalf("Codex does not support SessionEnd:\n%s", s)
	}
	if !strings.Contains(s, `"description": "keep"`) {
		t.Fatalf("foreign Codex hook metadata was not preserved:\n%s", s)
	}
}

func TestGrokHooksInstallIdempotent(t *testing.T) {
	out, changed, err := MergeGrokHooks([]byte("{}"), "/bin/ac")
	if err != nil || !changed {
		t.Fatalf("MergeGrokHooks: changed=%v err=%v", changed, err)
	}
	s := string(out)
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "SubagentStart", "SubagentStop", "SessionEnd"} {
		if !strings.Contains(s, `"`+event+`"`) {
			t.Fatalf("missing Grok event %q in:\n%s", event, s)
		}
	}
	if !strings.Contains(s, `"/bin/ac hook"`) {
		t.Fatalf("missing hook command:\n%s", s)
	}
	again, changed, err := MergeGrokHooks(out, "/bin/ac")
	if err != nil || changed || string(again) != string(out) {
		t.Fatalf("Grok merge not idempotent: changed=%v err=%v", changed, err)
	}
}

func TestOpenCodeConfigRoundTripPreservesForeignServers(t *testing.T) {
	seed := []byte(`{"theme":"system","mcp":{"foreign":{"type":"remote","url":"https://example.test/mcp"}}}`)
	out, changed, err := mergeOpenCodeConfig(seed, "/bin/ac")
	if err != nil || !changed {
		t.Fatalf("mergeOpenCodeConfig: changed=%v err=%v", changed, err)
	}
	again, changed, err := mergeOpenCodeConfig(out, "/bin/ac")
	if err != nil || changed || string(again) != string(out) {
		t.Fatalf("OpenCode merge not idempotent: changed=%v err=%v", changed, err)
	}
	removed, changed, err := removeOpenCodeConfig(out, "/bin/ac")
	if err != nil || !changed {
		t.Fatalf("removeOpenCodeConfig: changed=%v err=%v", changed, err)
	}
	if strings.Contains(string(removed), "agent-coordinator") || !strings.Contains(string(removed), "foreign") || !strings.Contains(string(removed), "theme") {
		t.Fatalf("OpenCode removal damaged foreign config:\n%s", removed)
	}
}

func TestRegisterMCPClientsSkipsMissingClient(t *testing.T) {
	var calls [][]string
	run := func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		if name == "codex" {
			return exec.ErrNotFound
		}
		return nil
	}
	if err := registerMCPClients("/bin/ac", run); err != nil {
		t.Fatalf("missing optional client must not fail install: %v", err)
	}
	if hasCall(calls, []string{"codex", "mcp", "add", "agent-coordinator", "--", "/bin/ac", "mcp"}) {
		t.Fatalf("add attempted after missing Codex executable: %v", calls)
	}
	if !hasCall(calls, []string{"grok", "mcp", "add", "--scope", "user", "agent-coordinator", "--", "/bin/ac", "mcp"}) {
		t.Fatalf("missing Codex prevented Grok registration: %v", calls)
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

	// Systemd socket activation is a linux-only install step; on darwin and
	// windows Install must leave ~/.config/systemd untouched.
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if runtime.GOOS == "linux" {
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
	} else if _, err := os.Stat(unitDir); !os.IsNotExist(err) {
		t.Fatalf("systemd unit dir created on %s (stat err=%v)", runtime.GOOS, err)
	}

	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(after)
	if got := strings.Count(s, `"/bin/ac hook"`); got != 8 {
		t.Fatalf("want 8 coordinator hook entries, got %d in:\n%s", got, s)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "SubagentStart", "SubagentStop", "SessionEnd"} {
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
	codexHooksPath := filepath.Join(home, ".codex", "hooks.json")
	codexHooks, err := os.ReadFile(codexHooksPath)
	if err != nil {
		t.Fatalf("Codex hooks: %v", err)
	}
	if got := strings.Count(string(codexHooks), `"/bin/ac hook"`); got != 4 {
		t.Fatalf("want 4 Codex hook entries, got %d in:\n%s", got, codexHooks)
	}
	if strings.Contains(string(codexHooks), `"SessionEnd"`) {
		t.Fatalf("unsupported SessionEnd installed for Codex:\n%s", codexHooks)
	}
	grokHooksPath := filepath.Join(home, ".grok", "hooks", "agent-coordinator.json")
	grokHooks, err := os.ReadFile(grokHooksPath)
	if err != nil {
		t.Fatalf("Grok hooks: %v", err)
	}
	if got := strings.Count(string(grokHooks), `"/bin/ac hook"`); got != 8 {
		t.Fatalf("want 8 Grok hook entries, got %d in:\n%s", got, grokHooks)
	}
	openCodePath := filepath.Join(home, ".config", "opencode", "opencode.json")
	openCode, err := os.ReadFile(openCodePath)
	if err != nil || !strings.Contains(string(openCode), `"/bin/ac"`) {
		t.Fatalf("OpenCode MCP config: err=%v content=%s", err, openCode)
	}
	wantCalls := [][]string{
		{"claude", "mcp", "remove", "--scope", "user", "agent-coordinator"},
		{"claude", "mcp", "add", "--scope", "user", "--transport", "stdio", "agent-coordinator", "--", "/bin/ac", "mcp"},
		{"codex", "mcp", "remove", "agent-coordinator"},
		{"codex", "mcp", "add", "agent-coordinator", "--", "/bin/ac", "mcp"},
		{"grok", "mcp", "remove", "--scope", "user", "agent-coordinator"},
		{"grok", "mcp", "add", "--scope", "user", "agent-coordinator", "--", "/bin/ac", "mcp"},
	}
	if runtime.GOOS == "linux" {
		wantCalls = append(wantCalls,
			[]string{"systemctl", "--user", "daemon-reload"},
			[]string{"systemctl", "--user", "enable", "--now", "agent-coordinator.socket"},
			[]string{"systemctl", "--user", "try-restart", "agent-coordinator.service"},
		)
	}
	for _, want := range wantCalls {
		if !hasCall(calls, want) {
			t.Fatalf("missing recorded command %v in %v", want, calls)
		}
	}
	assertNoSystemctlOffLinux(t, calls)

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
	codexHooks, err = os.ReadFile(codexHooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(codexHooks), "/bin/ac hook") {
		t.Fatalf("coordinator Codex hooks not removed:\n%s", codexHooks)
	}
	if _, err := os.Stat(grokHooksPath); !os.IsNotExist(err) {
		// File may remain if non-empty foreign content; must not still list our hook.
		if b, rerr := os.ReadFile(grokHooksPath); rerr == nil && strings.Contains(string(b), "/bin/ac hook") {
			t.Fatalf("coordinator Grok hooks not removed:\n%s", b)
		}
	}
	openCode, err = os.ReadFile(openCodePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(openCode), "agent-coordinator") {
		t.Fatalf("coordinator OpenCode MCP config not removed:\n%s", openCode)
	}
	// Holds on every OS: linux removed the units, darwin/windows never wrote any.
	for _, unit := range []string{"agent-coordinator.socket", "agent-coordinator.service"} {
		if _, err := os.Stat(filepath.Join(unitDir, unit)); !os.IsNotExist(err) {
			t.Fatalf("unit file %s still present (stat err=%v)", unit, err)
		}
	}
	wantCalls = [][]string{
		{"claude", "mcp", "remove", "--scope", "user", "agent-coordinator"},
		{"codex", "mcp", "remove", "agent-coordinator"},
		{"grok", "mcp", "remove", "--scope", "user", "agent-coordinator"},
	}
	if runtime.GOOS == "linux" {
		wantCalls = append(wantCalls, []string{"systemctl", "--user", "disable", "--now", "agent-coordinator.socket"})
	}
	for _, want := range wantCalls {
		if !hasCall(calls, want) {
			t.Fatalf("missing recorded command %v in %v", want, calls)
		}
	}
	assertNoSystemctlOffLinux(t, calls)
}

// assertNoSystemctlOffLinux fails if a systemctl invocation was recorded on a
// non-linux OS, where the installer must not attempt systemd management.
func assertNoSystemctlOffLinux(t *testing.T, calls [][]string) {
	t.Helper()
	if runtime.GOOS == "linux" {
		return
	}
	for _, c := range calls {
		if c[0] == "systemctl" {
			t.Fatalf("systemctl invoked on %s: %v", runtime.GOOS, c)
		}
	}
}

// Without systemctl (WSL without systemd), Install must degrade: no unit
// files left behind, but hooks merged and the MCP server registered.
func TestInstallDegradesWithoutSystemctl(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd unit setup is linux-only")
	}
	home := t.TempDir()
	var calls [][]string
	run := func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		if name == "systemctl" {
			return exec.ErrNotFound
		}
		return nil
	}
	if err := Install("/bin/ac", home, run); err != nil {
		t.Fatalf("Install must degrade without systemctl, not fail: %v", err)
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	for _, unit := range []string{"agent-coordinator.socket", "agent-coordinator.service"} {
		if _, err := os.Stat(filepath.Join(unitDir, unit)); !os.IsNotExist(err) {
			t.Fatalf("unit %s left behind without systemd (stat err=%v)", unit, err)
		}
	}
	after, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("hooks not merged: %v", err)
	}
	if got := strings.Count(string(after), `"/bin/ac hook"`); got != 8 {
		t.Fatalf("want 8 coordinator hook entries, got %d in:\n%s", got, after)
	}
	if !hasCall(calls, []string{"claude", "mcp", "add", "--scope", "user", "--transport", "stdio", "agent-coordinator", "--", "/bin/ac", "mcp"}) {
		t.Fatalf("MCP server not registered: %v", calls)
	}
	// The degrade path must disable the socket (no-op here) so a real systemd
	// host is never left with a dangling sockets.target.wants symlink.
	if !hasCall(calls, []string{"systemctl", "--user", "disable", "--now", "agent-coordinator.socket"}) {
		t.Fatalf("degrade path did not disable the socket: %v", calls)
	}
}

// On a REAL systemd host a transient failure (here: try-restart, after
// daemon-reload and enable succeeded) must still degrade, but first disable
// the socket so the enablement symlink from the successful enable is cleaned
// up before the unit files are removed.
func TestInstallDegradeCleansEnablementOnTransientFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd unit setup is linux-only")
	}
	home := t.TempDir()
	var calls [][]string
	run := func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		if name == "systemctl" && len(args) > 1 && args[1] == "try-restart" {
			return exec.ErrNotFound
		}
		return nil
	}
	if err := Install("/bin/ac", home, run); err != nil {
		t.Fatalf("Install must degrade on a transient systemctl failure, not fail: %v", err)
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	for _, unit := range []string{"agent-coordinator.socket", "agent-coordinator.service"} {
		if _, err := os.Stat(filepath.Join(unitDir, unit)); !os.IsNotExist(err) {
			t.Fatalf("unit %s left behind after degrade (stat err=%v)", unit, err)
		}
	}
	disable := []string{"systemctl", "--user", "disable", "--now", "agent-coordinator.socket"}
	if !hasCall(calls, disable) {
		t.Fatalf("dangling-symlink cleanup (disable) not run: %v", calls)
	}
	// The disable must come after the enable that created the symlink.
	enableIdx, disableIdx := -1, -1
	for i, c := range calls {
		if enableIdx == -1 && len(c) > 2 && c[0] == "systemctl" && c[2] == "enable" {
			enableIdx = i
		}
		if hasCall([][]string{c}, disable) {
			disableIdx = i
		}
	}
	if enableIdx == -1 || disableIdx <= enableIdx {
		t.Fatalf("disable (%d) must follow enable (%d): %v", disableIdx, enableIdx, calls)
	}
	after, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("hooks not merged: %v", err)
	}
	if got := strings.Count(string(after), `"/bin/ac hook"`); got != 8 {
		t.Fatalf("want 8 coordinator hook entries, got %d in:\n%s", got, after)
	}
	if !hasCall(calls, []string{"claude", "mcp", "add", "--scope", "user", "--transport", "stdio", "agent-coordinator", "--", "/bin/ac", "mcp"}) {
		t.Fatalf("MCP server not registered: %v", calls)
	}
}

// A binary path with spaces (common under C:\Program Files) must be quoted in
// the hook command, and the quoted form must merge/remove symmetrically.
func TestHookCommandQuotesSpacedPath(t *testing.T) {
	const spaced = `C:\Program Files\agent-coordinator.exe`
	if got := hookCommand(spaced); got != `"C:\Program Files\agent-coordinator.exe" hook` {
		t.Fatalf("hookCommand(%q) = %q", spaced, got)
	}
	if got := hookCommand("/bin/ac"); got != "/bin/ac hook" {
		t.Fatalf("plain path must stay unquoted, got %q", got)
	}
	merged, changed, err := MergeHooks([]byte("{}"), spaced)
	if err != nil || !changed {
		t.Fatalf("merge: changed=%v err=%v", changed, err)
	}
	again, changed, err := MergeHooks(merged, spaced)
	if err != nil || changed || string(again) != string(merged) {
		t.Fatalf("quoted merge must be idempotent: changed=%v err=%v", changed, err)
	}
	out, changed, err := RemoveHooks(merged, spaced)
	if err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	if strings.Contains(string(out), "agent-coordinator.exe") {
		t.Fatalf("quoted hook not removed:\n%s", out)
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

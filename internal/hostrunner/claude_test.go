package hostrunner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestClaudeProviderBuildsChromeOnlyStdinInvocation(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workingDir := t.TempDir()
	configDir := t.TempDir()
	provider, err := NewClaudeProvider(ClaudeConfig{
		Executable:  executable,
		WorkingDir:  workingDir,
		ConfigDir:   configDir,
		Environment: []string{"PATH=/safe", "OTHER=drop", "HOME=/profile"},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: "task-000000000001", Provider: "claude", Brief: "SECRET brief with ; shell syntax", Timeout: MinTaskTimeout}
	scratch := t.TempDir()
	invocation, err := provider.Prepare(task, scratch)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{
		"--print", "--chrome", "--no-session-persistence",
		"--permission-mode", "dontAsk",
		"--tools", "",
		"--allowedTools", claudeChromeTools,
		"--output-format", "json",
		"--json-schema", reportSchema,
		"--settings", filepath.Join(scratch, "claude-settings.json"),
		"--setting-sources", "",
		"--mcp-config", filepath.Join(scratch, "claude-mcp.json"),
		"--strict-mcp-config",
	}
	if !reflect.DeepEqual(invocation.Args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", invocation.Args, wantArgs)
	}
	if strings.Contains(strings.Join(invocation.Args, "\x00"), task.Brief) {
		t.Fatal("task brief leaked into argv")
	}
	if !strings.Contains(string(invocation.Prompt), task.Brief) {
		t.Fatal("task brief was not delivered through stdin")
	}
	if !reflect.DeepEqual(invocation.Env, []string{"PATH=/safe"}) || !invocation.ReportFromStdout || invocation.ConfigKey != "CLAUDE_CONFIG_DIR" || invocation.ConfigDir != configDir {
		t.Fatalf("unexpected invocation result path/environment: %+v", invocation)
	}
	for _, name := range []string{"claude-settings.json", "claude-mcp.json"} {
		info, err := os.Stat(filepath.Join(scratch, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("isolated %s mode/error = %v/%v", name, info, err)
		}
	}
}

func TestClaudeProviderParsesStructuredOutput(t *testing.T) {
	want := successReport()
	stdout, err := json.Marshal(map[string]any{
		"type":              "result",
		"subtype":           "success",
		"is_error":          false,
		"structured_output": want,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseClaudeReport(stdout)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("parse = (%+v, %v), want %+v", got, err, want)
	}

	for _, invalid := range [][]byte{
		[]byte(`{"type":"result"}`),
		append(stdout, []byte(` {}`)...),
		[]byte(`{"type":"result","subtype":"success","structured_output":{"status":"succeeded"}}`),
		[]byte(`{"type":"assistant","subtype":"success","structured_output":{"status":"succeeded"}}`),
		[]byte(`{"type":"result","subtype":"error","structured_output":{"status":"succeeded"}}`),
		[]byte(`{"type":"result","subtype":"success","is_error":true,"structured_output":{"status":"succeeded"}}`),
	} {
		if _, err := parseClaudeReport(invalid); !errors.Is(err, ErrInvalidReport) {
			t.Fatalf("invalid Claude output error = %v", err)
		}
	}
}

func TestClaudeProviderValidatesConfiguredPaths(t *testing.T) {
	if _, err := NewClaudeProvider(ClaudeConfig{Executable: "claude", WorkingDir: t.TempDir()}); err == nil {
		t.Fatal("relative executable was accepted")
	}
	executable, _ := os.Executable()
	if _, err := NewClaudeProvider(ClaudeConfig{Executable: executable, WorkingDir: "relative"}); err == nil {
		t.Fatal("relative working directory was accepted")
	}
	if _, err := NewClaudeProvider(ClaudeConfig{Executable: executable, WorkingDir: t.TempDir()}); err == nil {
		t.Fatal("missing isolated config directory was accepted")
	}
}

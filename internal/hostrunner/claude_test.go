package hostrunner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestClaudeProviderBuildsChromeOnlyStdinInvocation(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workingDir := t.TempDir()
	configDir := t.TempDir()
	credentials := writeCredentials(t, filepath.Join(t.TempDir(), ".credentials.json"), "{}", time.Now())
	provider, err := NewClaudeProvider(ClaudeConfig{
		Executable:  executable,
		WorkingDir:  workingDir,
		ConfigDir:   configDir,
		Credentials: credentials,
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
		"--model", claudeDefaultModel,
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
	if !reflect.DeepEqual(invocation.Env, []string{"PATH=/safe"}) || !invocation.ReportFromStdout || invocation.ConfigKey != "CLAUDE_CONFIG_DIR" || invocation.ConfigDir != configDir || invocation.Warning != "" {
		t.Fatalf("unexpected invocation result path/environment: %+v", invocation)
	}

	// An explicit model replaces the default alias in the same argv slot.
	explicit, err := NewClaudeProvider(ClaudeConfig{Executable: executable, WorkingDir: workingDir, ConfigDir: configDir,
		Credentials: credentials, Model: "claude-sonnet-4-5", Environment: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	invocation, err = explicit.Prepare(task, scratch)
	if err != nil {
		t.Fatal(err)
	}
	if index := slices.Index(invocation.Args, "--model"); index < 0 || invocation.Args[index+1] != "claude-sonnet-4-5" {
		t.Fatalf("explicit model missing from args: %#v", invocation.Args)
	}
	for _, model := range []string{"sonnet --dangerously-skip-permissions", "sonnet;whoami", "opus/latest", strings.Repeat("m", MaxModelBytes+1)} {
		if _, err := NewClaudeProvider(ClaudeConfig{Executable: executable, WorkingDir: workingDir, ConfigDir: configDir, Model: model}); err == nil {
			t.Fatalf("unsafe model %q was accepted", model)
		}
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

	// A failed report that explains itself only in the summary keeps that text as its error.
	failed := []byte(`{"type":"result","subtype":"success","is_error":false,"structured_output":{"status":"failed","summary":"tools denied","observations":[],"actions":[],"evidence":[]}}`)
	if got, err := parseClaudeReport(failed); err != nil || got.Error != "tools denied" {
		t.Fatalf("failed report without error = (%+v, %v)", got, err)
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
	if _, err := NewClaudeProvider(ClaudeConfig{Executable: executable, WorkingDir: t.TempDir(), ConfigDir: t.TempDir(),
		Credentials: ".credentials.json"}); err == nil {
		t.Fatal("relative credentials path was accepted")
	}
}

func writeCredentials(t *testing.T, path, content string, modified time.Time) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudeProviderRefreshesIsolatedCredentials(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: "task-000000000002", Provider: "claude", Brief: "read the page", Timeout: MinTaskTimeout}
	stale, recent := time.Now().Add(-2*time.Hour), time.Now().Add(-time.Minute)
	for _, testCase := range []struct {
		name           string
		source, copied string // empty content means the file is absent
		freshCopy      bool
		want           string
		fails, warns   bool
	}{
		{name: "source newer", source: "main login", copied: "stale login", want: "main login"},
		{name: "no isolated copy", source: "main login", want: "main login"},
		{name: "copy newer", source: "main login", copied: "self-refreshed", freshCopy: true, want: "self-refreshed"},
		{name: "source missing", copied: "self-refreshed", want: "self-refreshed", warns: true},
		{name: "both missing", fails: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home, configDir := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			if testCase.source != "" {
				writeCredentials(t, filepath.Join(home, claudeHomeDir, claudeCredentialsFile), testCase.source, recent)
			}
			destination := filepath.Join(configDir, claudeCredentialsFile)
			if testCase.copied != "" {
				modified := stale
				if testCase.freshCopy {
					modified = time.Now()
				}
				writeCredentials(t, destination, testCase.copied, modified)
			}
			provider, err := NewClaudeProvider(ClaudeConfig{Executable: executable, WorkingDir: t.TempDir(), ConfigDir: configDir})
			if err != nil {
				t.Fatal(err)
			}
			invocation, err := provider.Prepare(task, t.TempDir())
			if testCase.fails {
				if err == nil {
					t.Fatal("a turn with no login at all was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (invocation.Warning != "") != testCase.warns {
				t.Fatalf("warning = %q, want any: %v", invocation.Warning, testCase.warns)
			}
			data, err := os.ReadFile(destination)
			if err != nil || string(data) != testCase.want {
				t.Fatalf("isolated credentials = (%q, %v), want %q", data, err, testCase.want)
			}
			if info, err := os.Stat(destination); err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("isolated credentials mode/error = %v/%v", info, err)
			}
		})
	}
}

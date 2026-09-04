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

func TestCodexProviderBuildsFixedStdinInvocation(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workingDir := t.TempDir()
	configDir := t.TempDir()
	provider, err := NewCodexProvider(CodexConfig{
		Executable:  executable,
		WorkingDir:  workingDir,
		ConfigDir:   configDir,
		Environment: []string{"PATH=/safe", "OTHER=drop", "HOME=/profile", "CODEX_HOME=/ambient"},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: "task-000000000001", Provider: "codex", Brief: "SECRET brief with ; shell syntax", Timeout: MinTaskTimeout}
	scratch := t.TempDir()
	invocation, err := provider.Prepare(task, scratch)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{
		"exec", "--ephemeral", "--json", "--sandbox", "read-only",
		"--cd", workingDir,
		"--output-schema", filepath.Join(scratch, "report-schema.json"),
		"--output-last-message", filepath.Join(scratch, "report.json"),
		"-",
	}
	if !reflect.DeepEqual(invocation.Args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", invocation.Args, wantArgs)
	}
	if strings.Contains(strings.Join(invocation.Args, "\x00"), task.Brief) {
		t.Fatal("task brief leaked into argv")
	}
	if !strings.Contains(string(invocation.Prompt), task.Brief) || invocation.Args[len(invocation.Args)-1] != "-" {
		t.Fatal("task brief was not delivered through stdin")
	}
	if !reflect.DeepEqual(invocation.Env, []string{"PATH=/safe"}) || invocation.ConfigKey != "CODEX_HOME" || invocation.ConfigDir != configDir {
		t.Fatalf("environment/config = %v/%s=%q", invocation.Env, invocation.ConfigKey, invocation.ConfigDir)
	}
	var schema any
	data, err := os.ReadFile(filepath.Join(scratch, "report-schema.json"))
	if err != nil || json.Unmarshal(data, &schema) != nil {
		t.Fatalf("invalid schema: %v", err)
	}
}

func TestCodexProviderParsesStrictReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	data, _ := json.Marshal(successReport())
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := readReport(path)
	if err != nil || report.Status != ReportSucceeded {
		t.Fatalf("parse = (%+v, %v)", report, err)
	}

	if err := os.WriteFile(path, append(data[:len(data)-1], []byte(`,"extra":true}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readReport(path); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("unknown field error = %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", MaxReportBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readReport(path); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("oversized report error = %v", err)
	}
}

func TestCodexProviderValidatesConfiguredPaths(t *testing.T) {
	if _, err := NewCodexProvider(CodexConfig{Executable: "codex", WorkingDir: t.TempDir()}); err == nil {
		t.Fatal("relative executable was accepted")
	}
	executable, _ := os.Executable()
	if _, err := NewCodexProvider(CodexConfig{Executable: executable, WorkingDir: "relative"}); err == nil {
		t.Fatal("relative working directory was accepted")
	}
	if _, err := NewCodexProvider(CodexConfig{Executable: executable, WorkingDir: t.TempDir()}); err == nil {
		t.Fatal("missing isolated config directory was accepted")
	}
}

func TestProviderEnvironmentPolicy(t *testing.T) {
	credentials := []string{
		"AC_TOKEN=one", "AC_SESSION_SECRET=two", "AC_ADDR=three", "AC_SCOPE=four",
		"AC_KIND=five", "AC_RELAY_LISTEN=six", "AC_RELAY_INSECURE=seven",
	}
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{"ambient", append([]string{"PATH=/safe", "CODEX_HOME=/profile", "HOME=/home", "USERPROFILE=/user", "APPDATA=/app", "LOCALAPPDATA=/local", "OTHER=drop"}, credentials...), []string{"PATH=/safe"}},
		{"explicit", append([]string{"TEMP=/safe", "CODEX_HOME=/profile", "APPDATA=/app", "OTHER=drop"}, credentials...), []string{"TEMP=/safe"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := filterEnvironment(test.input); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("environment = %v, want %v", got, test.want)
			}
		})
	}
	for _, entry := range credentials {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "CODEX_HOME"} {
		t.Setenv(key, "/profile")
	}
	ambient := providerEnvironment(nil)
	for _, entry := range ambient {
		key, _, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if coordinatorEnvironment(key) || key == "HOME" || key == "USERPROFILE" || key == "APPDATA" || key == "LOCALAPPDATA" || key == "CODEX_HOME" {
			t.Fatalf("ambient private value survived: %s", key)
		}
	}
}

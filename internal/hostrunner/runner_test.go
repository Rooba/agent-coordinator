package hostrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type helperProvider struct {
	mode         string
	promptPath   string
	readyPath    string
	childPIDPath string
	environment  []string
	stdoutReport bool
	decode       func()
}

func (p *helperProvider) Name() string { return "fake" }

func (p *helperProvider) Prepare(task Task, scratchDir string) (Invocation, error) {
	executable, err := os.Executable()
	if err != nil {
		return Invocation{}, err
	}
	resultPath := filepath.Join(scratchDir, "report.json")
	return Invocation{
		Executable: executable,
		Args: []string{
			"-test.run=^TestHostrunnerHelperProcess$", "--", p.mode,
			resultPath, p.promptPath, p.readyPath, p.childPIDPath,
		},
		Dir:    scratchDir,
		Env:    append(os.Environ(), p.environment...),
		Prompt: []byte("broker prompt:\n" + task.Brief),
		Decode: func([]byte) (Report, error) {
			if p.decode != nil {
				p.decode()
			}
			return readReport(resultPath)
		},
		ReportFromStdout: p.stdoutReport,
	}, nil
}

func newHelperRunner(t *testing.T, provider *helperProvider, outputLimit int) *Runner {
	t.Helper()
	registry, err := NewRegistry(provider)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(registry, Options{
		DefaultTimeout: MinTaskTimeout,
		OutputLimit:    outputLimit,
		TempDir:        t.TempDir(),
		waitDelay:      100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func helperTask(id uint64) Task {
	return Task{ID: fmt.Sprintf("task-%012x", id), Provider: "fake", Brief: "inspect the browser"}
}

func TestRunnerExecutesAndParsesReport(t *testing.T) {
	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	runner := newHelperRunner(t, &helperProvider{mode: "success", promptPath: promptPath}, 1024)
	result, err := runner.Run(context.Background(), helperTask(1))
	if err != nil {
		t.Fatal(err)
	}
	if result.Report.Status != ReportSucceeded || string(result.Stdout) != "stdout" || string(result.Stderr) != "stderr" {
		t.Fatalf("unexpected result: %+v", result)
	}
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(prompt), "broker prompt:\ninspect the browser"; got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
	if _, busy := runner.Busy(); busy {
		t.Fatal("runner stayed busy after completion")
	}
}

func TestRunnerBoundsStdoutAndStderr(t *testing.T) {
	runner := newHelperRunner(t, &helperProvider{mode: "large"}, 17)
	result, err := runner.Run(context.Background(), helperTask(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Stdout) != 17 || !result.StdoutTruncated || len(result.Stderr) != 17 || !result.StderrTruncated {
		t.Fatalf("output was not bounded: stdout=%d/%v stderr=%d/%v", len(result.Stdout), result.StdoutTruncated, len(result.Stderr), result.StderrTruncated)
	}
}

func TestRunnerRejectsTruncatedStdoutReport(t *testing.T) {
	runner := newHelperRunner(t, &helperProvider{mode: "large", stdoutReport: true}, 17)
	result, err := runner.Run(context.Background(), helperTask(10))
	if !errors.Is(err, ErrOutputLimit) || !result.StdoutTruncated || result.Report.Status != "" {
		t.Fatalf("overflow result = (%+v, %v)", result, err)
	}
}

func TestRunnerTimeoutTerminatesProcess(t *testing.T) {
	runner := newHelperRunner(t, &helperProvider{mode: "sleep"}, 1024)
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := runner.Run(ctx, helperTask(3))
	if !errors.Is(err, context.DeadlineExceeded) || result.Report.Status != "" {
		t.Fatalf("timeout result = (%+v, %v)", result, err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("timed out process was not terminated promptly")
	}
}

func TestRunnerBusyAndExplicitCancel(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	runner := newHelperRunner(t, &helperProvider{mode: "sleep", readyPath: readyPath}, 1024)
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	first := helperTask(4)
	go func() {
		result, err := runner.Run(context.Background(), first)
		done <- outcome{result, err}
	}()
	waitForFile(t, readyPath)
	if id, busy := runner.Busy(); !busy || id != first.ID {
		t.Fatalf("busy = (%q, %v)", id, busy)
	}
	if _, err := runner.Run(context.Background(), helperTask(5)); !errors.Is(err, ErrBusy) {
		t.Fatalf("second task error = %v", err)
	}
	if err := runner.Cancel("other"); !errors.Is(err, ErrTaskNotActive) {
		t.Fatalf("wrong-id cancel = %v", err)
	}
	if err := runner.Cancel(first.ID); err != nil {
		t.Fatal(err)
	}
	finished := <-done
	if !errors.Is(finished.err, context.Canceled) || finished.result.Report.Status != "" {
		t.Fatalf("cancel result = (%+v, %v)", finished.result, finished.err)
	}
}

func TestRunnerKeepsCompletedResultWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	provider := &helperProvider{mode: "success", decode: cancel}
	runner := newHelperRunner(t, provider, 1024)
	result, err := runner.Run(ctx, helperTask(11))
	if err != nil || result.Report.Status != ReportSucceeded || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("completed cancellation result = (%+v, %v), context=%v", result, err, ctx.Err())
	}
}

func TestAwaitProcessKeepsNilExitWhenCancellationWins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	cancel()
	waitErr, interrupted := awaitProcess(ctx, waited, func() { waited <- nil })
	if waitErr != nil || !interrupted {
		t.Fatalf("awaitProcess = (%v, %v), want (nil, true)", waitErr, interrupted)
	}
}

func TestRunnerWaitDelayBoundsInheritedPipes(t *testing.T) {
	runner := newHelperRunner(t, &helperProvider{mode: "orphan-pipes"}, 1024)
	started := time.Now()
	result, err := runner.Run(context.Background(), helperTask(12))
	if !errors.Is(err, exec.ErrWaitDelay) || result.Report.Status != "" {
		t.Fatalf("inherited pipe result = (%+v, %v)", result, err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("descendant-held pipes blocked beyond WaitDelay")
	}
}

func TestRunnerPreservesStructuredFailureAndRejectsBadReport(t *testing.T) {
	runner := newHelperRunner(t, &helperProvider{mode: "exit"}, 1024)
	result, err := runner.Run(context.Background(), helperTask(6))
	if err == nil || result.Report.Status != "" {
		t.Fatalf("exit result = (%+v, %v)", result, err)
	}

	runner = newHelperRunner(t, &helperProvider{mode: "bad-report"}, 1024)
	result, err = runner.Run(context.Background(), helperTask(7))
	if !errors.Is(err, ErrInvalidReport) || result.Report.Status != "" {
		t.Fatalf("bad report result = (%+v, %v)", result, err)
	}
}

func TestRunnerRejectsUnknownProviderAndBadOptions(t *testing.T) {
	registry, _ := NewRegistry(namedProvider("known"))
	runner, err := NewRunner(registry, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), Task{ID: "task-000000000008", Provider: "unknown", Brief: "brief"}); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("unknown provider error = %v", err)
	}
	if _, err := NewRunner(registry, Options{DefaultTimeout: -time.Second}); err == nil {
		t.Fatal("invalid timeout options were accepted")
	}
}

func TestSanitizeChildEnvironmentDropsRelayValues(t *testing.T) {
	got := sanitizeChildEnvironment([]string{
		"PATH=/safe", "AC_TOKEN=secret", "ac_session_secret=secret", "AC_ADDR=address",
		"AC_SCOPE=scope", "AC_KIND=eyes", "AC_RELAY_PORT=1234", "HOME=/profile",
		"USERPROFILE=/profile", "APPDATA=/profile", "LOCALAPPDATA=/profile", "CODEX_HOME=/profile", "OTHER=value", "malformed",
	})
	want := []string{"PATH=/safe"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("sanitized environment = %v, want %v", got, want)
	}
}

func TestRunnerDoesNotExposeRelayEnvironment(t *testing.T) {
	provider := &helperProvider{mode: "environment", environment: []string{
		"AC_TOKEN=one", "AC_SESSION_SECRET=two", "AC_ADDR=three", "AC_SCOPE=four", "AC_KIND=five", "AC_RELAY_PORT=six",
		"HOME=/home", "USERPROFILE=/user", "APPDATA=/app", "LOCALAPPDATA=/local", "CODEX_HOME=/codex", "CLAUDE_CONFIG_DIR=/claude",
	}}
	runner := newHelperRunner(t, provider, 1024)
	result, err := runner.Run(context.Background(), helperTask(9))
	if err != nil || result.Report.Status != ReportSucceeded {
		t.Fatalf("environment result = (%+v, %v)", result, err)
	}
}

func TestHostrunnerHelperProcess(t *testing.T) {
	for _, arg := range os.Args {
		if arg == "--" {
			os.Exit(runHostrunnerHelper())
		}
	}
}

func runHostrunnerHelper() int {
	args := os.Args
	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || len(args) < separator+6 {
		return 90
	}
	mode, resultPath, promptPath := args[separator+1], args[separator+2], args[separator+3]
	readyPath, childPIDPath := args[separator+4], args[separator+5]
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 91
	}
	if promptPath != "" {
		if err := os.WriteFile(promptPath, prompt, 0o600); err != nil {
			return 92
		}
	}
	if readyPath != "" {
		if err := os.WriteFile(readyPath, []byte("ready"), 0o600); err != nil {
			return 93
		}
	}
	switch mode {
	case "success":
		fmt.Fprint(os.Stdout, "stdout")
		fmt.Fprint(os.Stderr, "stderr")
		return writeHelperReport(resultPath, successReport(), 0)
	case "large":
		fmt.Fprint(os.Stdout, strings.Repeat("o", 4096))
		fmt.Fprint(os.Stderr, strings.Repeat("e", 4096))
		return writeHelperReport(resultPath, successReport(), 0)
	case "sleep":
		time.Sleep(30 * time.Second)
		return 94
	case "exit":
		report := Report{Status: ReportFailed, Summary: "failed", Observations: []string{}, Actions: []string{}, Evidence: []string{}, Error: "provider failed"}
		return writeHelperReport(resultPath, report, 7)
	case "bad-report":
		if err := os.WriteFile(resultPath, []byte(`{"status":"succeeded","summary":"ok","observations":[],"actions":[],"evidence":[],"error":"","extra":true}`), 0o600); err != nil {
			return 95
		}
		return 0
	case "environment":
		for _, key := range []string{
			"AC_TOKEN", "AC_SESSION_SECRET", "AC_ADDR", "AC_SCOPE", "AC_KIND", "AC_RELAY_PORT",
			"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "CODEX_HOME", "CLAUDE_CONFIG_DIR",
		} {
			if os.Getenv(key) != "" {
				return 102
			}
		}
		return writeHelperReport(resultPath, successReport(), 0)
	case "orphan-pipes":
		child := exec.Command(os.Args[0], "-test.run=^TestHostrunnerHelperProcess$", "--", "leaf", "", "", "", "")
		child.Env, child.Stdout, child.Stderr = os.Environ(), os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			return 103
		}
		return writeHelperReport(resultPath, successReport(), 0)
	case "tree":
		child := exec.Command(os.Args[0], "-test.run=^TestHostrunnerHelperProcess$", "--", "leaf", "", "", "", childPIDPath)
		child.Env = os.Environ()
		if err := child.Start(); err != nil {
			return 96
		}
		time.Sleep(30 * time.Second)
		return 97
	case "leaf":
		if childPIDPath != "" {
			if err := os.WriteFile(childPIDPath, []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
				return 98
			}
		}
		time.Sleep(30 * time.Second)
		return 99
	default:
		return 100
	}
}

func writeHelperReport(path string, report Report, exitCode int) int {
	data, err := json.Marshal(report)
	if err != nil || os.WriteFile(path, data, 0o600) != nil {
		return 101
	}
	return exitCode
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

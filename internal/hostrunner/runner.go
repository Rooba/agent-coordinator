package hostrunner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeout   = MinTaskTimeout
	maxEnvelopeBytes = 64 << 10
	defaultOutput    = MaxReportBytes + maxEnvelopeBytes
	defaultWaitDelay = 2 * time.Second
	maxOutput        = 16 << 20
	maxPrompt        = 128 << 10
)

var (
	ErrBusy          = errors.New("host runner is busy")
	ErrTaskNotActive = errors.New("host task is not active")
	ErrOutputLimit   = errors.New("provider report exceeded the output limit")
)

type Options struct {
	DefaultTimeout time.Duration
	OutputLimit    int
	TempDir        string
	waitDelay      time.Duration // deterministic test seam; production uses defaultWaitDelay
}

// Result carries a Report only when Run returns nil. Any error is an
// infrastructure task.failed condition; bounded output remains diagnostic.
type Result struct {
	Report          Report
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	Warning         string // non-fatal preparation diagnostic from the provider
}

// Runner remembers only the active task. Durable completed-ID deduplication
// and at-most-once delivery belong to the broker/store boundary.
type Runner struct {
	registry *Registry
	opts     Options
	mu       sync.Mutex
	activeID string
	cancel   context.CancelFunc
}

func NewRunner(registry *Registry, opts Options) (*Runner, error) {
	if registry == nil {
		return nil, errors.New("host runner requires a provider registry")
	}
	if opts.DefaultTimeout == 0 {
		opts.DefaultTimeout = defaultTimeout
	}
	if opts.OutputLimit == 0 {
		opts.OutputLimit = defaultOutput
	}
	if opts.waitDelay == 0 {
		opts.waitDelay = defaultWaitDelay
	}
	if opts.DefaultTimeout < MinTaskTimeout || opts.DefaultTimeout > MaxTaskTimeout || opts.DefaultTimeout%time.Second != 0 {
		return nil, errors.New("invalid host runner timeout")
	}
	if opts.OutputLimit < 1 || opts.OutputLimit > maxOutput || opts.waitDelay < 0 {
		return nil, errors.New("invalid host runner output or wait limit")
	}
	if opts.TempDir != "" && !filepath.IsAbs(opts.TempDir) {
		return nil, errors.New("host runner temp directory must be absolute")
	}
	return &Runner{registry: registry, opts: opts}, nil
}

func (r *Runner) Busy() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeID, r.activeID != ""
}

func (r *Runner) Cancel(taskID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeID != taskID || r.cancel == nil {
		return fmt.Errorf("%w: %s", ErrTaskNotActive, taskID)
	}
	r.cancel()
	return nil
}

func (r *Runner) Run(ctx context.Context, task Task) (Result, error) {
	if err := task.Validate(); err != nil {
		return Result{}, err
	}
	provider, err := r.registry.Provider(task.Provider)
	if err != nil {
		return Result{}, err
	}
	timeout := task.Timeout
	if timeout == 0 {
		timeout = r.opts.DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := r.reserve(task.ID, cancel); err != nil {
		return Result{}, err
	}
	defer r.release()

	scratch, err := os.MkdirTemp(r.opts.TempDir, "agent-coordinator-host-")
	if err != nil {
		return Result{}, fmt.Errorf("provider scratch directory: %w", err)
	}
	defer os.RemoveAll(scratch)
	invocation, err := provider.Prepare(task, scratch)
	if err != nil {
		return Result{}, fmt.Errorf("prepare provider: %w", err)
	}
	if err := validateInvocation(invocation); err != nil {
		return Result{}, fmt.Errorf("prepare provider: %w", err)
	}
	if err := runCtx.Err(); err != nil {
		return Result{}, err
	}

	stdout, stderr := newCappedBuffer(r.opts.OutputLimit), newCappedBuffer(r.opts.OutputLimit)
	cmd := exec.Command(invocation.Executable, invocation.Args...)
	cmd.Dir, cmd.Env, cmd.Stdin = invocation.Dir, sanitizeChildEnvironment(invocation.Env), bytes.NewReader(invocation.Prompt)
	if invocation.ConfigKey != "" {
		cmd.Env = append(cmd.Env, invocation.ConfigKey+"="+invocation.ConfigDir)
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = r.opts.waitDelay
	guard, err := newProcessGuard()
	if err != nil {
		return Result{}, fmt.Errorf("create process guard: %w", err)
	}
	defer guard.Close()
	guard.Configure(cmd)
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start provider: %w", err)
	}
	if err := guard.Attach(cmd.Process); err != nil {
		guard.Terminate()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return capture(stdout, stderr), fmt.Errorf("isolate provider: %w", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	waitErr, interrupted := awaitProcess(runCtx, waited, func() {
		guard.Terminate()
		_ = cmd.Process.Kill()
	})
	guard.Close() // also retires descendants after a clean root exit
	result := capture(stdout, stderr)
	result.Warning = invocation.Warning
	if interrupted && waitErr != nil {
		return result, runCtx.Err()
	}
	if waitErr != nil {
		return result, fmt.Errorf("provider %s exited unsuccessfully: %w", task.Provider, waitErr)
	}
	if invocation.ReportFromStdout && result.StdoutTruncated {
		return result, ErrOutputLimit
	}
	report, err := invocation.Decode(result.Stdout)
	if err != nil {
		return result, err
	}
	result.Report = report
	return result, nil
}

func awaitProcess(ctx context.Context, waited <-chan error, terminate func()) (waitErr error, interrupted bool) {
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		select {
		case waitErr = <-waited: // completion already won; retain its result
		default:
			interrupted = true
			terminate()
			waitErr = <-waited
		}
	}
	return waitErr, interrupted
}

func (r *Runner) reserve(taskID string, cancel context.CancelFunc) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeID != "" {
		return fmt.Errorf("%w: %s", ErrBusy, r.activeID)
	}
	r.activeID, r.cancel = taskID, cancel
	return nil
}

func (r *Runner) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.activeID, r.cancel = "", nil
}

func validateInvocation(inv Invocation) error {
	switch {
	case !filepath.IsAbs(inv.Executable) || !filepath.IsAbs(inv.Dir):
		return errors.New("provider executable and working directory must be absolute")
	case len(inv.Prompt) == 0 || len(inv.Prompt) > maxPrompt || bytes.IndexByte(inv.Prompt, 0) >= 0:
		return errors.New("provider prompt is empty, oversized, or contains NUL")
	case inv.Decode == nil:
		return errors.New("provider result decoder is missing")
	case inv.ConfigKey != "" && inv.ConfigKey != "CODEX_HOME" && inv.ConfigKey != "CLAUDE_CONFIG_DIR":
		return errors.New("unsupported provider config environment")
	case (inv.ConfigKey == "") != (inv.ConfigDir == "") || inv.ConfigDir != "" && !validProviderPath(inv.ConfigDir, true):
		return errors.New("provider config directory must be an existing absolute directory")
	}
	for _, values := range [][]string{inv.Args, inv.Env} {
		for _, value := range values {
			if strings.ContainsRune(value, 0) {
				return errors.New("provider invocation contains NUL")
			}
		}
	}
	return nil
}

type processGuard interface {
	Configure(*exec.Cmd)
	Attach(*os.Process) error
	Terminate()
	Close()
}

type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newCappedBuffer(limit int) *cappedBuffer { return &cappedBuffer{limit: limit} }

func (b *cappedBuffer) Write(data []byte) (int, error) {
	length := len(data)
	if remaining := b.limit - b.buf.Len(); len(data) > remaining {
		b.truncated = true
		if remaining < 0 {
			remaining = 0
		}
		data = data[:remaining]
	}
	_, _ = b.buf.Write(data)
	return length, nil
}

func capture(stdout, stderr *cappedBuffer) Result {
	return Result{
		Stdout: append([]byte(nil), stdout.buf.Bytes()...), StdoutTruncated: stdout.truncated,
		Stderr: append([]byte(nil), stderr.buf.Bytes()...), StderrTruncated: stderr.truncated,
	}
}

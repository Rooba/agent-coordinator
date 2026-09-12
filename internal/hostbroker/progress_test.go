package hostbroker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/hostrunner"
	"github.com/Rooba/agent-coordinator/internal/protocol"
)

// childFrame is one progress frame with the session identity that sent it.
type childFrame struct {
	session Session
	body    protocol.TaskProgressMsg
}

// A running task reports in under its own child identity, carrying a turn count
// and a tool NAME and nothing else. A burst of samples between two frames
// collapses to the newest one: a task row has to look alive and current, not
// replay every turn.
func TestBrokerSendsThrottledProgressAsTheChild(t *testing.T) {
	// The deadline is only a backstop: a run that never reports in fails here
	// instead of hanging the package.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	launch := launchMessage("task-000000000001")
	store := &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher-fixed", SessionSecret: testLauncherSecret}}
	runner := &fakeRunner{wait: true}
	for turn := 1; turn <= 20; turn++ {
		runner.samples = append(runner.samples, hostrunner.Progress{Turns: turn, Tool: "mcp__claude-in-chrome__navigate"})
	}
	var mu sync.Mutex
	var progress []childFrame
	read := false
	relay := relayFunc(func(_ context.Context, session Session, request protocol.Request) (protocol.Response, error) {
		switch {
		case request.Op == protocol.OpRegister && request.Kind == protocol.KindLauncher:
			return protocol.Response{OK: true, Name: "host-one", AgentID: "launcher-agent"}, nil
		case request.Op == protocol.OpRegister && request.Kind == protocol.KindEyes:
			return protocol.Response{OK: true, Name: "eyes-one", AgentID: "eyes-agent", SessionSecret: testChildSecret}, nil
		case request.Op == protocol.OpRead && !read:
			read = true
			return protocol.Response{OK: true, Messages: []protocol.Message{launchEnvelope(t, launch)}}, nil
		case request.Op == protocol.OpSendWorkspace:
			var body protocol.TaskProgressMsg
			if json.Unmarshal([]byte(request.Body), &body) == nil && body.Type == protocol.TaskProgress {
				mu.Lock()
				progress = append(progress, childFrame{session, body})
				mu.Unlock()
				cancel()
			}
			return protocol.Response{OK: true}, nil
		}
		return protocol.Response{OK: true}, nil
	})
	options := testOptions()
	options.ProgressInterval = 20 * time.Millisecond
	broker, err := New(relay, store, newMemoryJournal(), runner,
		func(context.Context) ([]string, error) { return []string{"browser.chrome", "provider.claude"}, nil }, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(progress) == 0 || len(progress) >= len(runner.samples) {
		t.Fatalf("%d samples produced %d progress frames, want a throttled few", len(runner.samples), len(progress))
	}
	sent := progress[0]
	if sent.session.ID != "eyes-"+launch.TaskID || sent.session.Secret != testChildSecret {
		t.Fatalf("progress was not sent as the task's child: %+v", sent.session)
	}
	if sent.body.Type != protocol.TaskProgress || sent.body.TaskID != launch.TaskID ||
		sent.body.Turns != len(runner.samples) || sent.body.Tool != "mcp__claude-in-chrome__navigate" {
		t.Fatalf("progress body = %+v, want the newest sample", sent.body)
	}
	raw, err := json.Marshal(sent.body)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testToken, testLauncherSecret, testChildSecret, launch.Brief} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("progress body leaked %q", secret)
		}
	}
}

// blockedProgressRunner reports one sample, waits until the broker's progress
// frame is stuck in the relay, and only then finishes.
type blockedProgressRunner struct {
	inFlight <-chan struct{}
	succeed  bool
}

func (r *blockedProgressRunner) Run(ctx context.Context, _ hostrunner.Task, progress hostrunner.ProgressFunc) (hostrunner.Result, error) {
	progress(hostrunner.Progress{Turns: 1, Tool: "mcp__claude-in-chrome__navigate"})
	select {
	case <-r.inFlight:
	case <-ctx.Done():
		return hostrunner.Result{}, ctx.Err()
	}
	if !r.succeed {
		<-ctx.Done()
		return hostrunner.Result{}, ctx.Err()
	}
	return hostrunner.Result{Report: successReportForBroker()}, nil
}

func (r *blockedProgressRunner) Cancel(string) error { return nil }

// Progress is best-effort telemetry and must never gate the outcome: a frame
// still stuck in the relay is CANCELLED when the run ends, so the terminal
// result goes out at once instead of waiting out the progress call timeout.
func TestBrokerProgressNeverDelaysTerminalDelivery(t *testing.T) {
	for _, mode := range []struct {
		name     string
		succeed  bool
		terminal string
	}{
		{"successful run", true, protocol.TaskResult},
		{"cancelled run", false, protocol.TaskFailed},
	} {
		t.Run(mode.name, func(t *testing.T) {
			// The backstop is longer than one progress call timeout, so a run
			// that waits the blocked frame out fails here instead of passing.
			ctx, cancel := context.WithTimeout(context.Background(), 2*callTimeout)
			defer cancel()
			launch := launchMessage("task-000000000001")
			store := &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher-fixed", SessionSecret: testLauncherSecret}}
			inFlight, entered := make(chan struct{}), sync.Once{}
			runner := &blockedProgressRunner{inFlight: inFlight, succeed: mode.succeed}
			var mu sync.Mutex
			var blockedAt, deliveredAt time.Time
			read := false
			relay := relayFunc(func(callCtx context.Context, _ Session, request protocol.Request) (protocol.Response, error) {
				switch {
				case request.Op == protocol.OpRegister && request.Kind == protocol.KindLauncher:
					return protocol.Response{OK: true, Name: "host-one", AgentID: "launcher-agent"}, nil
				case request.Op == protocol.OpRegister && request.Kind == protocol.KindEyes:
					return protocol.Response{OK: true, Name: "eyes-one", AgentID: "eyes-agent", SessionSecret: testChildSecret}, nil
				case request.Op == protocol.OpRead && !read:
					read = true
					return protocol.Response{OK: true, Messages: []protocol.Message{launchEnvelope(t, launch)}}, nil
				case request.Op == protocol.OpSendWorkspace:
					var header struct {
						Type string `json:"type"`
					}
					_ = json.Unmarshal([]byte(request.Body), &header)
					switch header.Type {
					case protocol.TaskProgress:
						entered.Do(func() {
							mu.Lock()
							blockedAt = time.Now()
							mu.Unlock()
							close(inFlight)
						})
						if !mode.succeed {
							cancel() // a cancelled task must not wait the frame out either
						}
						<-callCtx.Done() // released only when the reporter cancels this call
						return protocol.Response{}, callCtx.Err()
					case mode.terminal:
						mu.Lock()
						deliveredAt = time.Now()
						mu.Unlock()
						cancel()
					}
				}
				return protocol.Response{OK: true}, nil
			})
			options := testOptions()
			options.ProgressInterval = time.Millisecond
			broker, err := New(relay, store, newMemoryJournal(), runner,
				func(context.Context) ([]string, error) { return []string{"browser.chrome", "provider.claude"}, nil }, options)
			if err != nil {
				t.Fatal(err)
			}
			if err := broker.Run(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Run error = %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if blockedAt.IsZero() {
				t.Fatal("no progress frame ever entered the relay")
			}
			if deliveredAt.IsZero() {
				t.Fatalf("no %s frame was delivered while progress was blocked", mode.terminal)
			}
			// A quarter of the progress call timeout is generous for a
			// delivery that is meant to be immediate, and unreachable for one
			// that waits a blocked frame out.
			if held := deliveredAt.Sub(blockedAt); held > callTimeout/4 {
				t.Fatalf("terminal delivery waited %s behind a blocked progress frame", held)
			}
		})
	}
}

// frameLog records the progress frames a run produced, in order.
type frameLog struct {
	mu     sync.Mutex
	frames []protocol.TaskProgressMsg
}

func (l *frameLog) add(frame protocol.TaskProgressMsg) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.frames = append(l.frames, frame)
	return len(l.frames)
}

func (l *frameLog) all() []protocol.TaskProgressMsg {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]protocol.TaskProgressMsg(nil), l.frames...)
}

// runStalledTask runs one task whose provider emits the given samples and then
// goes silent until it is cancelled, cancelling once stopAfter frames arrived.
func runStalledTask(t *testing.T, samples []hostrunner.Progress, interval time.Duration, stopAfter int) *frameLog {
	t.Helper()
	// The deadline is only a backstop: a run that never reports in fails here
	// instead of hanging the package.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	launch := launchMessage("task-000000000001")
	store := &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher-fixed", SessionSecret: testLauncherSecret}}
	runner := &fakeRunner{samples: samples, wait: true}
	log := &frameLog{}
	read := false
	relay := relayFunc(func(_ context.Context, _ Session, request protocol.Request) (protocol.Response, error) {
		switch {
		case request.Op == protocol.OpRegister && request.Kind == protocol.KindLauncher:
			return protocol.Response{OK: true, Name: "host-one", AgentID: "launcher-agent"}, nil
		case request.Op == protocol.OpRegister && request.Kind == protocol.KindEyes:
			return protocol.Response{OK: true, Name: "eyes-one", AgentID: "eyes-agent", SessionSecret: testChildSecret}, nil
		case request.Op == protocol.OpRead && !read:
			read = true
			return protocol.Response{OK: true, Messages: []protocol.Message{launchEnvelope(t, launch)}}, nil
		case request.Op == protocol.OpSendWorkspace:
			var frame protocol.TaskProgressMsg
			if json.Unmarshal([]byte(request.Body), &frame) == nil && frame.Type == protocol.TaskProgress && log.add(frame) >= stopAfter {
				cancel()
			}
		}
		return protocol.Response{OK: true}, nil
	})
	options := testOptions()
	options.ProgressInterval = interval
	broker, err := New(relay, store, newMemoryJournal(), runner,
		func(context.Context) ([]string, error) { return []string{"browser.chrome", "provider.claude"}, nil }, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	return log
}

// A task that is merely slow has to stay visibly alive: frames keep arriving on
// the clock with no stream events at all, and they report the turn count that
// was actually observed - which is what keeps the requester's inbox quiet,
// since the daemon only mails on a turn advance. The frames stop with the run.
func TestBrokerKeepsAStalledTaskVisiblyAlive(t *testing.T) {
	const interval = 5 * time.Millisecond
	assertStopped := func(t *testing.T, log *frameLog) {
		t.Helper()
		settled := len(log.all()) // Run returned, so the task goroutine is already reaped
		time.Sleep(20 * interval)
		if now := len(log.all()); now != settled {
			t.Fatalf("%d frames arrived after the task ended", now-settled)
		}
	}
	t.Run("silent from the start", func(t *testing.T) {
		log := runStalledTask(t, nil, interval, 3)
		frames := log.all()
		if len(frames) < 3 {
			t.Fatalf("a quiet task produced %d liveness frames", len(frames))
		}
		for _, frame := range frames {
			if frame.Turns != 0 || frame.Tool != "" {
				t.Fatalf("liveness frame invented progress: %+v", frame)
			}
		}
		assertStopped(t, log)
	})
	t.Run("silent after one sample", func(t *testing.T) {
		sample := hostrunner.Progress{Turns: 3, Tool: "mcp__claude-in-chrome__navigate"}
		log := runStalledTask(t, []hostrunner.Progress{sample}, interval, 3)
		frames := log.all()
		if len(frames) < 3 {
			t.Fatalf("a stalled task produced %d liveness frames", len(frames))
		}
		for _, frame := range frames {
			if frame.Turns != sample.Turns || frame.Tool != sample.Tool {
				t.Fatalf("liveness frame changed the last observed action: %+v", frame)
			}
		}
		assertStopped(t, log)
	})
}

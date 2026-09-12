package hostbroker

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/hostrunner"
	"github.com/Rooba/agent-coordinator/internal/protocol"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var (
	testLauncherSecret = strings.Repeat("a", 64)
	testChildSecret    = strings.Repeat("b", 64)
)

type memoryCredentials struct {
	mu    sync.Mutex
	value Credential
	saves int
}

func (s *memoryCredentials) Load(context.Context) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value, nil
}

func (s *memoryCredentials) Update(_ context.Context, update func(*Credential) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	return update(&s.value)
}

type memoryJournal struct {
	mu        sync.Mutex
	records   map[string]TaskRecord
	puts      map[string]int
	failState string
	failPuts  int
}

func newMemoryJournal() *memoryJournal {
	return &memoryJournal{records: make(map[string]TaskRecord), puts: make(map[string]int)}
}

func (j *memoryJournal) Get(id string) (TaskRecord, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	record, ok := j.records[id]
	return record, ok
}

func (j *memoryJournal) Put(record TaskRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.puts[record.State]++
	if record.State == j.failState && j.failPuts > 0 {
		j.failPuts--
		return errors.New("journal unavailable")
	}
	j.records[record.Launch.TaskID] = record
	return nil
}

func (j *memoryJournal) Records() []TaskRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	result := make([]TaskRecord, 0, len(j.records))
	for _, record := range j.records {
		result = append(result, record)
	}
	return result
}

type relayFunc func(context.Context, Session, protocol.Request) (protocol.Response, error)

func (f relayFunc) Call(ctx context.Context, session Session, request protocol.Request) (protocol.Response, error) {
	return f(ctx, session, request)
}

type fakeRunner struct {
	mu      sync.Mutex
	runs    []hostrunner.Task
	cancels []string
	result  hostrunner.Result
	err     error
	wait    bool
	samples []hostrunner.Progress
}

func (r *fakeRunner) Run(ctx context.Context, task hostrunner.Task, progress hostrunner.ProgressFunc) (hostrunner.Result, error) {
	r.mu.Lock()
	r.runs = append(r.runs, task)
	wait, result, err, samples := r.wait, r.result, r.err, r.samples
	r.mu.Unlock()
	for _, sample := range samples {
		if progress != nil {
			progress(sample)
		}
	}
	if wait {
		<-ctx.Done()
		return hostrunner.Result{}, ctx.Err()
	}
	return result, err
}

func (r *fakeRunner) Cancel(taskID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels = append(r.cancels, taskID)
	return nil
}

func launchMessage(id string) protocol.TaskLaunchMsg {
	return protocol.TaskLaunchMsg{
		Type: protocol.TaskLaunch, TaskID: id, Runtime: "claude", Scope: "/workspace",
		Brief: "inspect the browser", ReplyTo: protocol.AgentRef{Name: "requester", AgentID: "agent-1", Scope: "/workspace"},
		DeadlineS: 300,
	}
}

func successReportForBroker() hostrunner.Report {
	return hostrunner.Report{Status: hostrunner.ReportSucceeded, Summary: "done", Observations: []string{}, Actions: []string{}, Evidence: []string{}}
}

func messageBody(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func launchEnvelope(t *testing.T, launch protocol.TaskLaunchMsg) protocol.Message {
	t.Helper()
	return protocol.Message{Body: messageBody(t, launch), TaskID: launch.TaskID, FromScope: launch.Scope, ReplyTo: &launch.ReplyTo}
}

func cancelEnvelope(t *testing.T, launch protocol.TaskLaunchMsg) protocol.Message {
	t.Helper()
	return protocol.Message{Body: messageBody(t, protocol.TaskCancelMsg{Type: protocol.TaskCancel, TaskID: launch.TaskID}),
		TaskID: launch.TaskID, FromScope: launch.Scope, ReplyTo: &launch.ReplyTo}
}

func testOptions() Options {
	return Options{
		ComputerName: "TESTHOST", DefaultProvider: "claude", PollInterval: 100 * time.Millisecond,
		BackoffMin: time.Millisecond, BackoffMax: 4 * time.Millisecond,
		Jitter: func(time.Duration) time.Duration { return 0 },
		AcquireLock: func() (Unlock, error) {
			return func() error { return nil }, nil
		},
		Sleep: func(ctx context.Context, _ time.Duration) error {
			runtime.Gosched()
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				return nil
			}
		},
	}
}

func TestBrokerRefusesSecondProcessBeforeRegistration(t *testing.T) {
	calls := 0
	opts := testOptions()
	opts.AcquireLock = func() (Unlock, error) { return nil, ErrBrokerAlreadyRunning }
	broker, err := New(relayFunc(func(context.Context, Session, protocol.Request) (protocol.Response, error) {
		calls++
		return protocol.Response{OK: true}, nil
	}), &memoryCredentials{}, newMemoryJournal(), &fakeRunner{}, func(context.Context) ([]string, error) { return nil, nil }, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Run(context.Background()); !errors.Is(err, ErrBrokerAlreadyRunning) {
		t.Fatalf("Run error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("relay called %d times before lock", calls)
	}
}

func TestBrokerReleasesInstanceLockAfterShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	releases := 0
	opts := testOptions()
	opts.AcquireLock = func() (Unlock, error) { return func() error { releases++; return nil }, nil }
	relay := relayFunc(func(_ context.Context, _ Session, request protocol.Request) (protocol.Response, error) {
		if request.Op == protocol.OpRegister {
			return protocol.Response{OK: true, Name: "host"}, nil
		}
		if request.Op == protocol.OpRead {
			cancel()
		}
		return protocol.Response{OK: true}, nil
	})
	broker, err := New(relay, &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher", SessionSecret: testLauncherSecret}},
		newMemoryJournal(), &fakeRunner{}, func(context.Context) ([]string, error) { return nil, nil }, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Run(ctx); !errors.Is(err, context.Canceled) || releases != 1 {
		t.Fatalf("Run = %v, lock releases = %d", err, releases)
	}
}

func TestBrokerResultAndFailureUseOwnedIdentities(t *testing.T) {
	for _, test := range []struct {
		name, terminal string
		result         hostrunner.Result
		err            error
	}{
		{name: "result", terminal: protocol.TaskResult, result: hostrunner.Result{Report: hostrunner.Report{
			Status: hostrunner.ReportSucceeded, Summary: "loaded", Observations: []string{"page"}, Actions: []string{}, Evidence: []string{"title"},
		}}},
		{name: "invalid report", terminal: protocol.TaskFailed, err: hostrunner.ErrInvalidReport},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			launch := launchMessage("task-000000000001")
			store := &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher-fixed", SessionSecret: testLauncherSecret}}
			journal, runner := newMemoryJournal(), &fakeRunner{result: test.result, err: test.err}
			journal.failState = recordTerminal
			if test.name == "invalid report" {
				journal.failState = recordDelivered
			}
			journal.failPuts = 1
			var mu sync.Mutex
			var calls []struct {
				session Session
				request protocol.Request
			}
			var terminalBodies []string
			rotatedToken := strings.Repeat("c", 64)
			read := false
			relay := relayFunc(func(_ context.Context, session Session, request protocol.Request) (protocol.Response, error) {
				mu.Lock()
				calls = append(calls, struct {
					session Session
					request protocol.Request
				}{session, request})
				defer mu.Unlock()
				switch {
				case request.Op == protocol.OpRegister && request.Kind == protocol.KindLauncher:
					return protocol.Response{OK: true, Name: "host-one", AgentID: "launcher-agent"}, nil
				case request.Op == protocol.OpRead && !read:
					read = true
					return protocol.Response{OK: true, Messages: []protocol.Message{launchEnvelope(t, launch)}}, nil
				case request.Op == protocol.OpRead:
					return protocol.Response{OK: true}, nil
				case request.Op == protocol.OpRegister && request.Kind == protocol.KindEyes:
					return protocol.Response{OK: true, Name: "eyes-one", AgentID: "eyes-agent", SessionSecret: testChildSecret}, nil
				case request.Op == protocol.OpSendWorkspace:
					var header struct {
						Type string `json:"type"`
					}
					if err := json.Unmarshal([]byte(request.Body), &header); err != nil {
						t.Fatal(err)
					}
					if header.Type == test.terminal {
						terminalBodies = append(terminalBodies, request.Body)
						if len(terminalBodies) == 1 {
							if test.name == "result" {
								store.mu.Lock()
								store.value.Token = rotatedToken
								store.mu.Unlock()
								return protocol.Response{}, &RelayError{message: "transition temporarily unavailable"}
							}
							return protocol.Response{}, errors.New("ack lost")
						}
						if test.name == "result" && session.Token != rotatedToken {
							t.Fatalf("terminal retry used stale token: %+v", session)
						}
						cancel()
					}
					return protocol.Response{OK: true}, nil
				default:
					return protocol.Response{OK: true}, nil
				}
			})
			broker, err := New(relay, store, journal, runner, func(context.Context) ([]string, error) {
				return []string{"provider.claude", "browser.chrome"}, nil
			}, testOptions())
			if err != nil {
				t.Fatal(err)
			}
			if err := broker.Run(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Run error = %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			var accepted, terminal bool
			for _, call := range calls {
				if call.request.Op == protocol.OpRegister && call.request.Kind == protocol.KindLauncher {
					if call.request.Scope != "host:TESTHOST" || call.session.Token != testToken || call.request.Platform != "windows" ||
						!slicesEqual(call.request.Capabilities, []string{"browser.chrome", "provider.claude"}) {
						t.Fatalf("launcher registration = %+v / %+v", call.session, call.request)
					}
				}
				if call.request.Kind == protocol.KindEyes {
					if call.session.ID != "eyes-"+launch.TaskID || call.session.Secret != testLauncherSecret || call.request.AuthSessionID != "launcher-fixed" {
						t.Fatalf("child registration identity = %+v / %+v", call.session, call.request)
					}
				}
				if call.request.Op != protocol.OpSendWorkspace {
					continue
				}
				var header struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal([]byte(call.request.Body), &header)
				switch header.Type {
				case protocol.TaskAccepted:
					accepted = call.session.ID == "launcher-fixed"
				case test.terminal:
					terminal = call.session.ID == "eyes-"+launch.TaskID && call.session.Secret == testChildSecret
					if strings.Contains(call.request.Body, testToken) || strings.Contains(call.request.Body, testLauncherSecret) {
						t.Fatal("terminal body leaked relay credentials")
					}
				}
			}
			if !accepted || !terminal || store.saves != 0 {
				t.Fatalf("accepted=%v terminal=%v credential saves=%d", accepted, terminal, store.saves)
			}
			if len(terminalBodies) != 2 || terminalBodies[0] != terminalBodies[1] {
				t.Fatalf("terminal retries = %q", terminalBodies)
			}
			if len(runner.runs) != 1 || runner.runs[0].Brief != launch.Brief {
				t.Fatalf("runner tasks = %+v", runner.runs)
			}
			if journal.puts[journal.failState] < 2 {
				t.Fatalf("%s journal attempts = %d", journal.failState, journal.puts[journal.failState])
			}
		})
	}
}

func TestBrokerReconnectsWithStableIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher-fixed", SessionSecret: testLauncherSecret}}
	attempts := 0
	relay := relayFunc(func(_ context.Context, session Session, request protocol.Request) (protocol.Response, error) {
		if request.Op == protocol.OpRegister {
			attempts++
			if attempts < 3 {
				return protocol.Response{}, errors.New("offline")
			}
			if session.ID != "launcher-fixed" || session.Secret != testLauncherSecret {
				t.Fatalf("registration identity changed: %+v", session)
			}
			return protocol.Response{OK: true, Name: "host"}, nil
		}
		if request.Op == protocol.OpRead {
			cancel()
		}
		return protocol.Response{OK: true}, nil
	})
	opts := testOptions()
	var waits []time.Duration
	opts.Sleep = func(ctx context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	broker, err := New(relay, store, newMemoryJournal(), &fakeRunner{}, func(context.Context) ([]string, error) { return nil, nil }, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	if attempts != 3 || len(waits) < 2 || waits[0] != time.Millisecond || waits[1] != 2*time.Millisecond {
		t.Fatalf("attempts=%d waits=%v", attempts, waits)
	}
}

func TestBrokerReloadsRotatedTokenWhileRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rotated := strings.Repeat("c", 64)
	store := &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher-fixed", SessionSecret: testLauncherSecret}}
	registrations := 0
	relay := relayFunc(func(_ context.Context, session Session, request protocol.Request) (protocol.Response, error) {
		switch request.Op {
		case protocol.OpRegister:
			registrations++
			want := testToken
			if registrations > 1 {
				want = rotated
			}
			if session.Token != want || session.ID != "launcher-fixed" || session.Secret != testLauncherSecret {
				t.Fatalf("registration %d identity = %+v", registrations, session)
			}
			return protocol.Response{OK: true, Name: "host"}, nil
		case protocol.OpRead:
			if session.Token == testToken {
				if err := store.Update(context.Background(), func(credential *Credential) error {
					credential.Token = rotated
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				return protocol.Response{}, &RelayError{message: "unauthorized"}
			}
			cancel()
		}
		return protocol.Response{OK: true}, nil
	})
	broker, err := New(relay, store, newMemoryJournal(), &fakeRunner{}, func(context.Context) ([]string, error) { return nil, nil }, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	if registrations != 2 {
		t.Fatalf("launcher registrations = %d", registrations)
	}
}

func TestBrokerRetriesLostInitialRegistrationWithPremintedSecret(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher-fixed", SessionSecret: testLauncherSecret}}
	var registrations []Session
	relay := relayFunc(func(_ context.Context, session Session, request protocol.Request) (protocol.Response, error) {
		switch request.Op {
		case protocol.OpRegister:
			registrations = append(registrations, session)
			if len(registrations) == 1 {
				return protocol.Response{}, errors.New("registration response lost")
			}
			return protocol.Response{OK: true, Name: "host"}, nil
		case protocol.OpRead:
			cancel()
		}
		return protocol.Response{OK: true}, nil
	})
	broker, err := New(relay, store, newMemoryJournal(), &fakeRunner{}, func(context.Context) ([]string, error) { return nil, nil }, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	if len(registrations) != 2 || registrations[0] != registrations[1] || registrations[0].Secret != testLauncherSecret || store.saves != 0 {
		t.Fatalf("registrations=%+v stored=%+v", registrations, store.value)
	}
}

func TestBrokerDoesNotRegisterBeforeReadinessProbe(t *testing.T) {
	calls := 0
	broker, err := New(relayFunc(func(context.Context, Session, protocol.Request) (protocol.Response, error) {
		calls++
		return protocol.Response{OK: true}, nil
	}), &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher", SessionSecret: testLauncherSecret}},
		newMemoryJournal(), &fakeRunner{}, func(context.Context) ([]string, error) { return nil, errors.New("not ready") }, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "readiness probe") {
		t.Fatalf("Run error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("relay called %d times before readiness", calls)
	}
}

func TestBrokerRejectsMissingPremintedSecretBeforeRegistration(t *testing.T) {
	calls := 0
	broker, err := New(relayFunc(func(context.Context, Session, protocol.Request) (protocol.Response, error) {
		calls++
		return protocol.Response{OK: true}, nil
	}), &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher"}}, newMemoryJournal(), &fakeRunner{},
		func(context.Context) ([]string, error) { return nil, nil }, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Run(context.Background()); err == nil || calls != 0 {
		t.Fatalf("Run = %v, relay calls = %d", err, calls)
	}
}

func TestBrokerRejectsForgedOuterTaskMetadata(t *testing.T) {
	launch := launchMessage("task-000000000005")
	for _, test := range []struct {
		name   string
		mutate func(*protocol.Message)
	}{
		{name: "task id", mutate: func(message *protocol.Message) { message.TaskID = "task-000000000006" }},
		{name: "scope", mutate: func(message *protocol.Message) { message.FromScope = "/forged" }},
		{name: "reply target", mutate: func(message *protocol.Message) {
			message.ReplyTo = &protocol.AgentRef{Name: "attacker", Scope: launch.Scope}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal := newMemoryJournal()
			broker, err := New(relayFunc(func(context.Context, Session, protocol.Request) (protocol.Response, error) {
				return protocol.Response{OK: true}, nil
			}), &memoryCredentials{}, journal, &fakeRunner{}, func(context.Context) ([]string, error) { return nil, nil }, testOptions())
			if err != nil {
				t.Fatal(err)
			}
			message := launchEnvelope(t, launch)
			test.mutate(&message)
			if err := broker.handleMessage(context.Background(), message); err != nil {
				t.Fatal(err)
			}
			if len(journal.Records()) != 0 {
				t.Fatal("forged task was journaled")
			}
		})
	}
}

func TestBrokerUnknownCancelDurablyAcknowledgesAsLauncher(t *testing.T) {
	launch := launchMessage("task-000000000009")
	message := cancelEnvelope(t, launch)
	journal, runner := newMemoryJournal(), &fakeRunner{}
	store := &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher-fixed", SessionSecret: testLauncherSecret}}
	var sends []protocol.Request
	relay := relayFunc(func(_ context.Context, session Session, request protocol.Request) (protocol.Response, error) {
		if request.Op != protocol.OpSendWorkspace {
			return protocol.Response{OK: true}, nil
		}
		if session.ID != "launcher-fixed" || session.Secret != testLauncherSecret {
			t.Fatalf("cancel acknowledgement identity = %+v", session)
		}
		sends = append(sends, request)
		if record, ok := journal.Get(launch.TaskID); !ok || record.State != recordTerminal {
			t.Fatalf("cancel was not durable before send: %+v, %v", record, ok)
		}
		if len(sends) == 1 {
			return protocol.Response{}, errors.New("ack response lost")
		}
		return protocol.Response{OK: true}, nil
	})
	broker, err := New(relay, store, journal, runner, func(context.Context) ([]string, error) { return nil, nil }, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.refreshCredential(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := broker.handleMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if len(sends) != 2 || !reflect.DeepEqual(sends[0], sends[1]) {
		t.Fatalf("cancel acknowledgement retries = %+v", sends)
	}
	var failure protocol.TaskFailedMsg
	if err := json.Unmarshal([]byte(sends[0].Body), &failure); err != nil || failure.Type != protocol.TaskFailed ||
		failure.TaskID != launch.TaskID || failure.Error != "cancelled" || sends[0].TaskID != launch.TaskID ||
		sends[0].Target == nil || !reflect.DeepEqual(*sends[0].Target, launch.ReplyTo) {
		t.Fatalf("cancel acknowledgement = %+v / %v", sends[0], err)
	}
	record, ok := journal.Get(launch.TaskID)
	if !ok || record.State != recordDelivered || len(record.Terminal) != 0 || record.Launch.Brief != "" {
		t.Fatalf("acknowledged cancel tombstone = %+v, %v", record, ok)
	}
	if len(runner.runs) != 0 || len(runner.cancels) != 0 {
		t.Fatalf("unknown cancel touched runner: runs=%+v cancels=%+v", runner.runs, runner.cancels)
	}
}

func TestBrokerIgnoresUntrustedUnknownCancel(t *testing.T) {
	launch := launchMessage("task-00000000000a")
	for _, test := range []struct {
		name   string
		mutate func(*protocol.Message)
	}{
		{name: "unknown field", mutate: func(message *protocol.Message) {
			message.Body = strings.TrimSuffix(message.Body, "}") + `,"extra":true}`
		}},
		{name: "outer task id", mutate: func(message *protocol.Message) { message.TaskID = "task-00000000000b" }},
		{name: "invalid task id", mutate: func(message *protocol.Message) {
			message.TaskID = "not-a-task"
			message.Body = messageBody(t, protocol.TaskCancelMsg{Type: protocol.TaskCancel, TaskID: message.TaskID})
		}},
		{name: "missing scope", mutate: func(message *protocol.Message) { message.FromScope = "" }},
		{name: "missing reply", mutate: func(message *protocol.Message) { message.ReplyTo = nil }},
		{name: "foreign reply", mutate: func(message *protocol.Message) { message.ReplyTo.Scope = "/forged" }},
		{name: "empty reply", mutate: func(message *protocol.Message) { message.ReplyTo.Name, message.ReplyTo.AgentID = "", "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := cancelEnvelope(t, launch)
			test.mutate(&message)
			journal, sends := newMemoryJournal(), 0
			broker, err := New(relayFunc(func(context.Context, Session, protocol.Request) (protocol.Response, error) {
				sends++
				return protocol.Response{OK: true}, nil
			}), &memoryCredentials{}, journal, &fakeRunner{}, func(context.Context) ([]string, error) { return nil, nil }, testOptions())
			if err != nil {
				t.Fatal(err)
			}
			if err := broker.handleMessage(context.Background(), message); err != nil {
				t.Fatal(err)
			}
			if sends != 0 || len(journal.Records()) != 0 {
				t.Fatalf("untrusted cancel was handled: sends=%d records=%+v", sends, journal.Records())
			}
		})
	}
}

func TestBrokerStopsOnPermanentChildRegistrationRejection(t *testing.T) {
	launch := launchMessage("task-000000000006")
	journal, runner := newMemoryJournal(), &fakeRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	read, childRegistrations := false, 0
	relay := relayFunc(func(_ context.Context, session Session, request protocol.Request) (protocol.Response, error) {
		switch {
		case request.Op == protocol.OpRegister && request.Kind == protocol.KindLauncher:
			return protocol.Response{OK: true, Name: "host"}, nil
		case request.Op == protocol.OpRead && !read:
			read = true
			return protocol.Response{OK: true, Messages: []protocol.Message{launchEnvelope(t, launch)}}, nil
		case request.Op == protocol.OpRegister && request.Kind == protocol.KindEyes:
			childRegistrations++
			return protocol.Response{}, &RelayError{message: "task assignment rejected"}
		case request.Op == protocol.OpSendWorkspace:
			if session.ID != "launcher-fixed" || !strings.Contains(request.Body, protocol.TaskFailed) || !strings.Contains(request.Body, "task assignment rejected") {
				t.Fatalf("launcher failure = %+v / %s", session, request.Body)
			}
			cancel()
			return protocol.Response{OK: true}, nil
		default:
			return protocol.Response{OK: true}, nil
		}
	})
	broker, err := New(relay, &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher-fixed", SessionSecret: testLauncherSecret}},
		journal, runner, func(context.Context) ([]string, error) { return nil, nil }, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	err = broker.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	record, exists := journal.Get(launch.TaskID)
	if !exists || record.State != recordDelivered || len(runner.runs) != 0 || childRegistrations != 1 {
		t.Fatalf("record=%+v exists=%v runs=%d child registrations=%d", record, exists, len(runner.runs), childRegistrations)
	}
}

func TestBrokerBusyCancellationAndDuplicate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, second := launchMessage("task-000000000002"), launchMessage("task-000000000003")
	runner := &fakeRunner{wait: true}
	store := &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher-fixed", SessionSecret: testLauncherSecret}}
	journal := newMemoryJournal()
	var mu sync.Mutex
	read, busyFailed, firstFailed := 0, false, false
	relay := relayFunc(func(_ context.Context, session Session, request protocol.Request) (protocol.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case request.Op == protocol.OpRegister && request.Kind == protocol.KindLauncher:
			return protocol.Response{OK: true, Name: "host", AgentID: "launcher"}, nil
		case request.Op == protocol.OpRegister && request.Kind == protocol.KindEyes:
			return protocol.Response{OK: true, Name: session.ID, AgentID: session.ID, SessionSecret: testChildSecret}, nil
		case request.Op == protocol.OpRead:
			read++
			switch read {
			case 1:
				return protocol.Response{OK: true, Messages: []protocol.Message{launchEnvelope(t, first), launchEnvelope(t, first), launchEnvelope(t, second)}}, nil
			case 2:
				return protocol.Response{OK: true, Messages: []protocol.Message{cancelEnvelope(t, first)}}, nil
			default:
				return protocol.Response{OK: true}, nil
			}
		case request.Op == protocol.OpSendWorkspace:
			var failure protocol.TaskFailedMsg
			if json.Unmarshal([]byte(request.Body), &failure) == nil && failure.Type == protocol.TaskFailed {
				if failure.TaskID == second.TaskID {
					busyFailed = failure.Error == "eyes busy" && session.ID == "eyes-"+second.TaskID
				}
				if failure.TaskID == first.TaskID {
					firstFailed = session.ID == "eyes-"+first.TaskID
				}
				if busyFailed && firstFailed {
					cancel()
				}
			}
			return protocol.Response{OK: true}, nil
		default:
			return protocol.Response{OK: true}, nil
		}
	})
	broker, err := New(relay, store, journal, runner, func(context.Context) ([]string, error) { return []string{"browser.chrome"}, nil }, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	if !busyFailed || !firstFailed {
		t.Fatalf("busy failed=%v first failed=%v", busyFailed, firstFailed)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.runs) != 1 || len(runner.cancels) == 0 || runner.cancels[0] != first.TaskID {
		t.Fatalf("runs=%+v cancels=%+v", runner.runs, runner.cancels)
	}
}

func TestBrokerShutdownLeavesOfflineTerminalDurableWithoutRerun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	launch := launchMessage("task-000000000008")
	journal, runner := newMemoryJournal(), &fakeRunner{result: hostrunner.Result{Report: successReportForBroker()}}
	read, terminalAttempts := false, 0
	relay := relayFunc(func(_ context.Context, session Session, request protocol.Request) (protocol.Response, error) {
		switch {
		case request.Op == protocol.OpRegister && request.Kind == protocol.KindLauncher:
			return protocol.Response{OK: true, Name: "host"}, nil
		case request.Op == protocol.OpRead && !read:
			read = true
			return protocol.Response{OK: true, Messages: []protocol.Message{launchEnvelope(t, launch)}}, nil
		case request.Op == protocol.OpRegister && request.Kind == protocol.KindEyes:
			return protocol.Response{OK: true, Name: session.ID, AgentID: "child", SessionSecret: testChildSecret}, nil
		case request.Op == protocol.OpSendWorkspace && strings.Contains(request.Body, protocol.TaskResult):
			terminalAttempts++
			cancel()
			return protocol.Response{}, errors.New("relay offline")
		default:
			return protocol.Response{OK: true}, nil
		}
	})
	broker, err := New(relay, &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher", SessionSecret: testLauncherSecret}},
		journal, runner, func(context.Context) ([]string, error) { return nil, nil }, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	record, _ := journal.Get(launch.TaskID)
	if len(runner.runs) != 1 || terminalAttempts != 1 || record.State != recordTerminal || len(record.Terminal) == 0 {
		t.Fatalf("runs=%d terminal attempts=%d record=%+v", len(runner.runs), terminalAttempts, record)
	}
}

func TestBrokerRecoversAcceptedWithoutRerun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	launch := launchMessage("task-000000000004")
	journal := newMemoryJournal()
	journal.records[launch.TaskID] = TaskRecord{
		Launch: launch, Child: protocol.AgentRef{Name: "eyes", Scope: launch.Scope},
		ChildSession: "eyes-" + launch.TaskID, ChildSecret: testChildSecret, State: recordAccepted,
	}
	runner := &fakeRunner{}
	store := &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher-fixed", SessionSecret: testLauncherSecret}}
	relay := relayFunc(func(_ context.Context, session Session, request protocol.Request) (protocol.Response, error) {
		switch {
		case request.Op == protocol.OpRegister:
			return protocol.Response{OK: true, Name: "host"}, nil
		case request.Op == protocol.OpSendWorkspace:
			if session.ID != "eyes-"+launch.TaskID || !strings.Contains(request.Body, "execution was not repeated") {
				t.Fatalf("recovery delivery = %+v / %s", session, request.Body)
			}
			cancel()
			return protocol.Response{OK: true}, nil
		default:
			return protocol.Response{OK: true}, nil
		}
	})
	broker, err := New(relay, store, journal, runner, func(context.Context) ([]string, error) { return nil, nil }, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	if len(runner.runs) != 0 {
		t.Fatal("accepted task was rerun")
	}
	record, _ := journal.Get(launch.TaskID)
	if record.State != recordDelivered {
		t.Fatal("recovered terminal was not journaled as delivered")
	}
}

func TestBrokerRecoversReceivedWithReissuedChildSecret(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	launch := launchMessage("task-000000000007")
	journal := newMemoryJournal()
	journal.records[launch.TaskID] = TaskRecord{Launch: launch, State: recordReceived}
	runner := &fakeRunner{}
	var childRegistrations, terminalSends int
	relay := relayFunc(func(_ context.Context, session Session, request protocol.Request) (protocol.Response, error) {
		switch {
		case request.Op == protocol.OpRegister && request.Kind == protocol.KindLauncher:
			return protocol.Response{OK: true, Name: "host"}, nil
		case request.Op == protocol.OpRegister && request.Kind == protocol.KindEyes:
			childRegistrations++
			if session.ID != "eyes-"+launch.TaskID || session.Secret != testLauncherSecret || request.AuthSessionID != "launcher-fixed" {
				t.Fatalf("recovery child registration = %+v / %+v", session, request)
			}
			return protocol.Response{OK: true, Name: session.ID, AgentID: "child", SessionSecret: testChildSecret}, nil
		case request.Op == protocol.OpSendWorkspace:
			terminalSends++
			if session.ID != "eyes-"+launch.TaskID || session.Secret != testChildSecret || !strings.Contains(request.Body, "execution was not started") {
				t.Fatalf("recovery terminal = %+v / %s", session, request.Body)
			}
			cancel()
			return protocol.Response{OK: true}, nil
		default:
			return protocol.Response{OK: true}, nil
		}
	})
	broker, err := New(relay, &memoryCredentials{value: Credential{Token: testToken, LauncherSession: "launcher-fixed", SessionSecret: testLauncherSecret}},
		journal, runner, func(context.Context) ([]string, error) { return nil, nil }, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	record, _ := journal.Get(launch.TaskID)
	if childRegistrations != 1 || terminalSends != 1 || record.State != recordDelivered || len(runner.runs) != 0 {
		t.Fatalf("child registrations=%d terminal sends=%d state=%s runs=%d", childRegistrations, terminalSends, record.State, len(runner.runs))
	}
}

func TestRetryUsesBoundedExponentialBackoff(t *testing.T) {
	var waits []time.Duration
	opts := testOptions()
	opts.BackoffMin, opts.BackoffMax = time.Second, 4*time.Second
	opts.Sleep = func(_ context.Context, duration time.Duration) error { waits = append(waits, duration); return nil }
	broker, err := New(relayFunc(func(context.Context, Session, protocol.Request) (protocol.Response, error) {
		return protocol.Response{}, nil
	}), &memoryCredentials{}, newMemoryJournal(), &fakeRunner{}, func(context.Context) ([]string, error) { return nil, nil }, opts)
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	if err := broker.retry(context.Background(), func(context.Context) error {
		attempts++
		if attempts < 5 {
			return errors.New("offline")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	if !slicesEqual(waits, want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
}

func TestRetryStopsOnRelayRejection(t *testing.T) {
	waits, attempts := 0, 0
	opts := testOptions()
	opts.Sleep = func(context.Context, time.Duration) error { waits++; return nil }
	broker, err := New(relayFunc(func(context.Context, Session, protocol.Request) (protocol.Response, error) {
		return protocol.Response{}, nil
	}),
		&memoryCredentials{}, newMemoryJournal(), &fakeRunner{}, func(context.Context) ([]string, error) { return nil, nil }, opts)
	if err != nil {
		t.Fatal(err)
	}
	err = broker.retry(context.Background(), func(context.Context) error {
		attempts++
		return &RelayError{message: "invalid transition"}
	})
	if err == nil || attempts != 1 || waits != 0 {
		t.Fatalf("retry = (%v, attempts=%d, waits=%d)", err, attempts, waits)
	}
}

func slicesEqual[T comparable](left, right []T) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

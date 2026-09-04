package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/hostbroker"
	"github.com/Rooba/agent-coordinator/internal/hostrunner"
	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/store"
)

const e2eProviderMarker = "hostbroker-e2e-provider"

type e2eProvider struct {
	mode, runPath, readyPath string
}

func (p e2eProvider) Name() string { return "claude" }

func (p e2eProvider) Prepare(task hostrunner.Task, scratch string) (hostrunner.Invocation, error) {
	executable, err := os.Executable()
	if err != nil {
		return hostrunner.Invocation{}, err
	}
	return hostrunner.Invocation{
		Executable: executable,
		Args:       []string{"-test.run=^TestHostBrokerE2EProviderProcess$", "--", e2eProviderMarker, p.mode, p.runPath, p.readyPath},
		Dir:        scratch,
		Env:        os.Environ(),
		Prompt:     []byte(task.Brief),
		Decode: func(stdout []byte) (hostrunner.Report, error) {
			var report hostrunner.Report
			if err := json.Unmarshal(stdout, &report); err != nil {
				return report, err
			}
			return report, report.Validate()
		},
		ReportFromStdout: true,
	}, nil
}

// TestHostBrokerE2EProviderProcess is the inert stand-in for a paid browser
// provider. The parent test invokes this binary explicitly; a normal test run
// returns without side effects.
func TestHostBrokerE2EProviderProcess(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 5 || args[1] != e2eProviderMarker {
		return
	}
	mode, runPath, readyPath := args[2], args[3], args[4]
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil || !strings.Contains(string(prompt), "browser") {
		fmt.Fprintln(os.Stderr, "invalid provider prompt")
		os.Exit(2)
	}
	file, err := os.OpenFile(runPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(2)
	}
	_, err = fmt.Fprintln(file, os.Getpid())
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		os.Exit(2)
	}
	switch mode {
	case "success":
		fmt.Fprint(os.Stdout, `{"status":"succeeded","summary":"browser ready","observations":["login visible"],"actions":[],"evidence":["page title"]}`)
		os.Exit(0)
	case "block":
		if err := os.WriteFile(readyPath, []byte("ready"), 0o600); err != nil {
			os.Exit(2)
		}
		for {
			time.Sleep(time.Hour)
		}
	default:
		os.Exit(2)
	}
}

type e2eCredentials struct {
	mu    sync.Mutex
	value hostbroker.Credential
}

func (s *e2eCredentials) Load(context.Context) (hostbroker.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value, nil
}

func (s *e2eCredentials) Update(_ context.Context, update func(*hostbroker.Credential) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return update(&s.value)
}

type e2eJournal struct {
	mu      sync.Mutex
	records map[string]hostbroker.TaskRecord
}

func newE2EJournal() *e2eJournal {
	return &e2eJournal{records: make(map[string]hostbroker.TaskRecord)}
}

func (j *e2eJournal) Get(taskID string) (hostbroker.TaskRecord, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	record, ok := j.records[taskID]
	return record, ok
}

func (j *e2eJournal) Put(record hostbroker.TaskRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.records[record.Launch.TaskID] = record
	return nil
}

func (j *e2eJournal) Records() []hostbroker.TaskRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	records := make([]hostbroker.TaskRecord, 0, len(j.records))
	for _, record := range j.records {
		records = append(records, record)
	}
	return records
}

type runningE2EBroker struct {
	done chan struct{}
	err  error
}

func startE2EBroker(t *testing.T, addr, token, computer, session, secret string, provider e2eProvider, journal *e2eJournal) (*runningE2EBroker, *hostrunner.Runner) {
	t.Helper()
	client, err := hostbroker.NewClient(addr)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := hostrunner.NewRegistry(provider)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := hostrunner.NewRunner(registry, hostrunner.Options{TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	credentials := &e2eCredentials{value: hostbroker.Credential{
		Token: token, LauncherSession: session, SessionSecret: secret,
	}}
	broker, err := hostbroker.New(client, credentials, journal, runner,
		func(context.Context) ([]string, error) { return []string{"browser.chrome", "provider.claude"}, nil },
		hostbroker.Options{ComputerName: computer, DefaultProvider: "claude", PollInterval: 100 * time.Millisecond,
			BackoffMin: 5 * time.Millisecond, BackoffMax: 25 * time.Millisecond,
			Jitter:      func(time.Duration) time.Duration { return 0 },
			AcquireLock: func() (hostbroker.Unlock, error) { return func() error { return nil }, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	running := &runningE2EBroker{done: make(chan struct{})}
	go func() {
		running.err = broker.Run(ctx)
		close(running.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-running.done:
			if running.err != nil && !errors.Is(running.err, context.Canceled) {
				t.Errorf("host broker stopped: %v", running.err)
			}
		case <-time.After(8 * time.Second):
			t.Error("host broker did not stop")
		}
	})
	return running, runner
}

func awaitE2E(t *testing.T, running *runningE2EBroker, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		if running != nil {
			select {
			case <-running.done:
				t.Fatalf("host broker exited early: %v", running.err)
			default:
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for host broker state")
}

func lostLauncherRegistration(t *testing.T, addr, token, computer, session string, st *store.Store) (hostbroker.Credential, store.AgentIdentity) {
	t.Helper()
	secret := premintSecret(session)
	tcpHangUp(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "host:" + computer,
		SessionID: session, Kind: protocol.KindLauncher, Platform: "windows",
		Capabilities: []string{"browser.chrome", "provider.claude"}, Token: token, SessionSecret: secret})
	identity := awaitRow(t, st, "host:"+computer, session)
	return hostbroker.Credential{Token: token, LauncherSession: session, SessionSecret: secret}, identity
}

func requestE2EEyes(t *testing.T, sock, scope, session string) protocol.Response {
	t.Helper()
	response := roundTrip(t, sock, protocol.Request{Op: protocol.OpRequestEyes, Scope: scope,
		SessionID: session, Brief: "inspect the browser login", Runtime: "claude", DeadlineS: 300,
		ReplyTo: &protocol.AgentRef{Name: "forged", AgentID: "deadbeef0000", Scope: "/forged"}})
	if !response.OK || !isTaskID(response.TaskID) {
		t.Fatalf("request_eyes: %+v", response)
	}
	return response
}

func providerRuns(path string) int {
	data, _ := os.ReadFile(path)
	return strings.Count(string(data), "\n")
}

func TestHostBrokerE2EHappyPathSurvivesLostResponses(t *testing.T) {
	sock, addr, token, st := relayDaemon(t)
	const computer, launcherSession = "E2EHAPPY", "launcher-e2e-happy"
	credential, firstIdentity := lostLauncherRegistration(t, addr, token, computer, launcherSession, st)
	requester := registerUnix(t, sock, "/e2e/happy", "requester-happy")
	request := requestE2EEyes(t, sock, requester.Scope, "requester-happy")
	pending, err := st.PendingLaunches(launcherSession)
	if err != nil || len(pending) != 1 || pending[0].TaskID != request.TaskID {
		t.Fatalf("pending launch: %+v (%v)", pending, err)
	}
	tcpHangUp(t, addr, protocol.Request{Op: protocol.OpRead, SessionID: launcherSession,
		Token: token, SessionSecret: credential.SessionSecret})
	awaitE2E(t, nil, func() bool {
		unread, err := st.UnreadCount("host:"+computer, firstIdentity.Name)
		return err == nil && unread == 0
	})
	redelivered, err := st.PendingLaunches(launcherSession)
	if err != nil || len(redelivered) != 1 || redelivered[0].ID != pending[0].ID {
		t.Fatalf("redelivered launch: %+v (%v)", redelivered, err)
	}

	runPath := filepath.Join(t.TempDir(), "runs")
	journal := newE2EJournal()
	running, _ := startE2EBroker(t, addr, token, computer, launcherSession, credential.SessionSecret,
		e2eProvider{mode: "success", runPath: runPath}, journal)
	awaitE2E(t, running, func() bool {
		task, err := st.EyesTask(request.TaskID)
		record, exists := journal.Get(request.TaskID)
		return err == nil && task.State == "done" && exists && record.State == "delivered"
	})
	identity, err := st.Identity("host:"+computer, launcherSession)
	if err != nil || identity != firstIdentity {
		t.Fatalf("launcher retry changed identity: first=%+v retry=%+v (%v)", firstIdentity, identity, err)
	}
	if runs := providerRuns(runPath); runs != 1 {
		t.Fatalf("provider executions = %d, want 1", runs)
	}

	read := roundTrip(t, sock, protocol.Request{Op: protocol.OpRead, Scope: requester.Scope,
		SessionID: "requester-happy"})
	if !read.OK || len(read.Messages) != 2 {
		t.Fatalf("requester inbox: %+v", read)
	}
	var accepted protocol.TaskAcceptedMsg
	if err := json.Unmarshal([]byte(read.Messages[0].Body), &accepted); err != nil ||
		accepted.Type != protocol.TaskAccepted || accepted.TaskID != request.TaskID ||
		read.Messages[0].Kind != protocol.KindLauncher || read.Messages[0].FromScope != "host:"+computer {
		t.Fatalf("launcher acceptance: %+v / %+v (%v)", read.Messages[0], accepted, err)
	}
	var result protocol.TaskResultMsg
	report := read.Messages[1]
	if err := json.Unmarshal([]byte(report.Body), &result); err != nil || result.Type != protocol.TaskResult ||
		result.TaskID != request.TaskID || result.Status != hostrunner.ReportSucceeded || result.Summary != "browser ready" ||
		report.Kind != protocol.KindEyes || report.TaskID != request.TaskID || report.FromScope != "" ||
		report.ReplyTo == nil || report.ReplyTo.Name != report.From {
		t.Fatalf("child result: %+v / %+v (%v)", report, result, err)
	}
	if accepted.Child == nil || report.ReplyTo.AgentID != accepted.Child.AgentID {
		t.Fatalf("accepted child and result sender disagree: %+v / %+v", accepted.Child, report.ReplyTo)
	}
}

func TestHostBrokerE2ECancelStopsProviderAndAcknowledges(t *testing.T) {
	sock, addr, token, st := relayDaemon(t)
	const computer, launcherSession = "E2ECANCEL", "launcher-e2e-cancel"
	secret := premintSecret(launcherSession)
	providerDir := t.TempDir()
	runPath, readyPath := filepath.Join(providerDir, "runs"), filepath.Join(providerDir, "ready")
	journal := newE2EJournal()
	running, runner := startE2EBroker(t, addr, token, computer, launcherSession, secret,
		e2eProvider{mode: "block", runPath: runPath, readyPath: readyPath}, journal)
	awaitE2E(t, running, func() bool {
		_, err := st.Identity("host:"+computer, launcherSession)
		return err == nil
	})
	requester := registerUnix(t, sock, "/e2e/cancel", "requester-cancel")
	request := requestE2EEyes(t, sock, requester.Scope, "requester-cancel")
	awaitE2E(t, running, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	})
	cancel := roundTrip(t, sock, protocol.Request{Op: protocol.OpCancelEyes, Scope: requester.Scope,
		SessionID: "requester-cancel", TaskID: request.TaskID})
	if !cancel.OK || cancel.TaskID != request.TaskID {
		t.Fatalf("cancel_eyes: %+v", cancel)
	}
	awaitE2E(t, running, func() bool {
		task, taskErr := st.EyesTask(request.TaskID)
		pending, pendingErr := st.PendingCancels(launcherSession)
		record, exists := journal.Get(request.TaskID)
		_, busy := runner.Busy()
		return taskErr == nil && task.State == "cancelled" && pendingErr == nil && len(pending) == 0 &&
			exists && record.State == "delivered" && !busy
	})
	if runs := providerRuns(runPath); runs != 1 {
		t.Fatalf("provider executions = %d, want 1", runs)
	}
	record, exists := journal.Get(request.TaskID)
	if !exists || record.State != "delivered" || len(record.Terminal) != 0 {
		t.Fatalf("cancel journal: %+v, exists=%v", record, exists)
	}
	read := roundTrip(t, sock, protocol.Request{Op: protocol.OpRead, Scope: requester.Scope,
		SessionID: "requester-cancel"})
	if !read.OK || len(read.Messages) != 1 {
		t.Fatalf("cancel must add no requester terminal: %+v", read)
	}
	var accepted protocol.TaskAcceptedMsg
	if err := json.Unmarshal([]byte(read.Messages[0].Body), &accepted); err != nil || accepted.Type != protocol.TaskAccepted {
		t.Fatalf("requester cancellation inbox: %+v (%v)", read.Messages, err)
	}
}

func TestHostBrokerE2ECancelBeforeObservedLaunchStartsNoProvider(t *testing.T) {
	sock, addr, token, st := relayDaemon(t)
	const computer, launcherSession = "E2EEARLY", "launcher-e2e-early-cancel"
	credential, launcher := lostLauncherRegistration(t, addr, token, computer, launcherSession, st)
	requester := registerUnix(t, sock, "/e2e/early", "requester-early")
	request := requestE2EEyes(t, sock, requester.Scope, "requester-early")
	tcpHangUp(t, addr, protocol.Request{Op: protocol.OpRead, SessionID: launcherSession,
		Token: token, SessionSecret: credential.SessionSecret})
	awaitE2E(t, nil, func() bool {
		unread, err := st.UnreadCount("host:"+computer, launcher.Name)
		return err == nil && unread == 0
	})
	if response := roundTrip(t, sock, protocol.Request{Op: protocol.OpCancelEyes, Scope: requester.Scope,
		SessionID: "requester-early", TaskID: request.TaskID}); !response.OK {
		t.Fatalf("cancel_eyes: %+v", response)
	}
	pending, err := st.PendingCancels(launcherSession)
	if err != nil || len(pending) != 1 || pending[0].TaskID != request.TaskID {
		t.Fatalf("pending cancel: %+v (%v)", pending, err)
	}
	runPath := filepath.Join(t.TempDir(), "runs")
	journal := newE2EJournal()
	running, _ := startE2EBroker(t, addr, token, computer, launcherSession, credential.SessionSecret,
		e2eProvider{mode: "success", runPath: runPath}, journal)
	awaitE2E(t, running, func() bool {
		pending, err := st.PendingCancels(launcherSession)
		record, exists := journal.Get(request.TaskID)
		return err == nil && len(pending) == 0 && exists && record.State == "delivered"
	})
	task, err := st.EyesTask(request.TaskID)
	if err != nil || task.State != "cancelled" {
		t.Fatalf("cancelled task: %+v (%v)", task, err)
	}
	if runs := providerRuns(runPath); runs != 0 {
		t.Fatalf("cancelled unseen launch ran provider %d times", runs)
	}
	record, exists := journal.Get(request.TaskID)
	if !exists || record.State != "delivered" || record.ChildSession != "" || len(record.Terminal) != 0 {
		t.Fatalf("unknown cancel journal: %+v, exists=%v", record, exists)
	}
	read := roundTrip(t, sock, protocol.Request{Op: protocol.OpRead, Scope: requester.Scope,
		SessionID: "requester-early"})
	if !read.OK || len(read.Messages) != 0 {
		t.Fatalf("early cancel must not mail a terminal: %+v", read)
	}
	if _, err := st.Identity(requester.Scope, "eyes-"+request.TaskID); err == nil {
		t.Fatal("early cancel must not create an eyes child")
	}
}

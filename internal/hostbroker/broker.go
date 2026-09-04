package hostbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Rooba/agent-coordinator/internal/hostrunner"
	"github.com/Rooba/agent-coordinator/internal/protocol"
)

const (
	idlePoll       = 5 * time.Second
	busyPoll       = 2 * time.Second
	minBackoff     = 250 * time.Millisecond
	maxBackoff     = 5 * time.Second
	maxMessageBody = 128 << 10
)

type Runner interface {
	Run(context.Context, hostrunner.Task) (hostrunner.Result, error)
	Cancel(string) error
}

type Probe func(context.Context) ([]string, error)
type Unlock func() error

var ErrBrokerAlreadyRunning = errors.New("another host broker is already running for this user")

type Options struct {
	ComputerName    string
	DefaultProvider string
	PollInterval    time.Duration
	BackoffMin      time.Duration
	BackoffMax      time.Duration
	Sleep           func(context.Context, time.Duration) error
	Jitter          func(time.Duration) time.Duration
	AcquireLock     func() (Unlock, error)
}

type Broker struct {
	relay   Relay
	store   CredentialStore
	journal Journal
	runner  Runner
	probe   Probe
	opts    Options

	mu              sync.Mutex
	credential      Credential
	credentialDirty bool
	launcher        protocol.AgentRef
	capabilities    []string
	activeID        string
	activeCancel    context.CancelFunc
	workers         sync.WaitGroup
}

func New(relay Relay, store CredentialStore, journal Journal, runner Runner, probe Probe, opts Options) (*Broker, error) {
	if relay == nil || store == nil || journal == nil || runner == nil || probe == nil {
		return nil, errors.New("host broker requires relay, credentials, journal, runner, and readiness probe")
	}
	computer := strings.TrimSpace(opts.ComputerName)
	if computer == "" || len(computer) > 128 || strings.ContainsAny(computer, "\x00/\\") {
		return nil, errors.New("invalid Windows computer name")
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = idlePoll
	}
	if opts.PollInterval < 100*time.Millisecond || opts.PollInterval > idlePoll {
		return nil, errors.New("poll interval must be between 100ms and 5s")
	}
	if opts.BackoffMin == 0 {
		opts.BackoffMin = minBackoff
	}
	if opts.BackoffMax == 0 {
		opts.BackoffMax = maxBackoff
	}
	if opts.BackoffMin <= 0 || opts.BackoffMin > opts.BackoffMax || opts.BackoffMax > time.Minute {
		return nil, errors.New("invalid reconnect backoff")
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepContext
	}
	if opts.Jitter == nil {
		opts.Jitter = func(max time.Duration) time.Duration {
			if max <= 0 {
				return 0
			}
			return time.Duration(rand.Int64N(int64(max) + 1))
		}
	}
	if opts.AcquireLock == nil {
		return nil, errors.New("host broker requires a single-instance lock")
	}
	return &Broker{relay: relay, store: store, journal: journal, runner: runner, probe: probe, opts: opts}, nil
}

func (b *Broker) Run(ctx context.Context) error {
	unlock, err := b.opts.AcquireLock()
	if err != nil {
		return fmt.Errorf("acquire host broker instance lock: %w", err)
	}
	if unlock == nil {
		return errors.New("acquire host broker instance lock: no release handle")
	}
	defer unlock()
	if err := b.refreshCredential(ctx); err != nil {
		return err
	}
	capabilities, err := b.probe(ctx)
	if err != nil {
		return fmt.Errorf("host readiness probe: %w", err)
	}
	b.capabilities, err = normalizeCapabilities(capabilities)
	if err != nil {
		return err
	}
	if err := b.retry(ctx, b.registerLauncher); err != nil {
		return err
	}
	defer b.shutdown()
	if err := b.recover(ctx); err != nil {
		return err
	}

	for {
		response, err := b.call(ctx, b.launcherSession(), protocol.Request{Op: protocol.OpRead, Scope: b.hostScope(), From: b.launcherRef().Name})
		if err != nil {
			if err := b.retry(ctx, b.registerLauncher); err != nil {
				return err
			}
		} else {
			for _, message := range response.Messages {
				if err := b.handleMessage(ctx, message); err != nil {
					return err
				}
			}
		}
		delay := b.opts.PollInterval
		if b.isBusy() && delay > busyPoll {
			delay = busyPoll
		}
		if err := b.opts.Sleep(ctx, delay); err != nil {
			return err
		}
	}
}

func (b *Broker) registerLauncher(ctx context.Context) error {
	request := protocol.Request{
		Op: protocol.OpRegister, Scope: b.hostScope(), Source: "hostbroker", Kind: protocol.KindLauncher,
		Platform: "windows", Capabilities: append([]string(nil), b.capabilities...),
	}
	var current Session
	var response protocol.Response
	err := b.retryFreshAuthorization(ctx, func() error {
		current = b.launcherSession()
		var err error
		response, err = b.call(ctx, current, request)
		return err
	})
	if err != nil {
		return err
	}
	if response.Name == "" || response.SessionSecret != "" && len(response.SessionSecret) < 32 {
		return errors.New("relay returned an incomplete launcher identity")
	}
	if response.SessionSecret != "" && response.SessionSecret != current.Secret {
		b.mu.Lock()
		b.credential.SessionSecret = response.SessionSecret
		b.credentialDirty = true
		b.mu.Unlock()
		if err := b.flushCredential(ctx); err != nil {
			return err
		}
	}
	if b.launcherSession().Secret == "" {
		return errors.New("relay returned no launcher session secret")
	}
	b.mu.Lock()
	b.launcher = protocol.AgentRef{Name: response.Name, AgentID: response.AgentID, Scope: b.hostScope()}
	b.mu.Unlock()
	return nil
}

func (b *Broker) handleMessage(ctx context.Context, message protocol.Message) error {
	body := message.Body
	if len(body) == 0 || len(body) > maxMessageBody {
		return nil
	}
	var header struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(body), &header) != nil {
		return nil
	}
	switch header.Type {
	case protocol.TaskLaunch:
		var launch protocol.TaskLaunchMsg
		if decodeStrict(body, &launch) == nil && message.TaskID == launch.TaskID && message.FromScope == launch.Scope &&
			message.ReplyTo != nil && reflect.DeepEqual(*message.ReplyTo, launch.ReplyTo) {
			return b.handleLaunch(ctx, launch)
		}
	case protocol.TaskCancel:
		var cancel protocol.TaskCancelMsg
		record, exists := b.journal.Get(message.TaskID)
		if decodeStrict(body, &cancel) == nil && exists && cancel.Type == protocol.TaskCancel && cancel.TaskID == message.TaskID &&
			message.FromScope == record.Launch.Scope && message.ReplyTo != nil && reflect.DeepEqual(*message.ReplyTo, record.Launch.ReplyTo) {
			b.cancel(message.TaskID)
		}
	}
	return nil
}

func (b *Broker) handleLaunch(ctx context.Context, launch protocol.TaskLaunchMsg) error {
	provider := launch.Runtime
	if provider == "" {
		provider = b.opts.DefaultProvider
	}
	launch.Runtime = provider
	task := hostrunner.Task{ID: launch.TaskID, Provider: provider, Brief: launch.Brief, Timeout: time.Duration(launch.DeadlineS) * time.Second}
	if launch.Type != protocol.TaskLaunch || launch.Scope == "" || launch.ReplyTo.Scope != launch.Scope ||
		(launch.ReplyTo.AgentID == "" && launch.ReplyTo.Name == "") || task.Validate() != nil {
		return nil
	}
	if record, exists := b.journal.Get(launch.TaskID); exists {
		if !reflect.DeepEqual(record.Launch, launch) {
			return nil
		}
		if record.State == recordTerminal {
			b.start(func() { _ = b.superviseTerminal(ctx, record) })
		}
		return nil
	}
	record := TaskRecord{Launch: launch, State: recordReceived}
	if active, busy := b.active(); busy {
		if active == launch.TaskID {
			return nil
		}
		record.State, record.Terminal = recordTerminal, failedBody(launch.TaskID, "eyes busy")
		if err := b.journal.Put(record); err != nil {
			return err
		}
		return b.prepareTerminal(ctx, record)
	}
	if err := b.journal.Put(record); err != nil {
		return err
	}
	taskCtx, cancel := context.WithCancel(ctx)
	if !b.activate(launch.TaskID, cancel) {
		cancel()
		record.State, record.Terminal = recordTerminal, failedBody(launch.TaskID, "eyes busy")
		if err := b.journal.Put(record); err != nil {
			return err
		}
		return b.prepareTerminal(ctx, record)
	}
	child, err := b.registerChild(ctx, launch)
	if err != nil {
		b.deactivate(launch.TaskID)
		cancel()
		if !retryable(err) {
			return b.failWithoutChild(ctx, record, fmt.Errorf("register eyes child: %w", err))
		}
		return fmt.Errorf("register eyes child: %w", err)
	}
	record.Child, record.ChildSession, record.ChildSecret, record.State = child.ref, child.session.ID, child.session.Secret, recordAccepted
	if err := b.journal.Put(record); err != nil {
		b.deactivate(launch.TaskID)
		cancel()
		b.deregisterBounded(child.session, launch.Scope)
		return err
	}
	b.start(func() {
		defer b.deactivate(launch.TaskID)
		defer cancel()
		b.execute(ctx, taskCtx, record, child, task)
	})
	return nil
}

func (b *Broker) active() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activeID, b.activeID != ""
}

func (b *Broker) execute(lifecycle, taskCtx context.Context, record TaskRecord, child childIdentity, task hostrunner.Task) {
	launch := record.Launch
	accepted, _ := json.Marshal(protocol.TaskAcceptedMsg{Type: protocol.TaskAccepted, TaskID: launch.TaskID, Child: &child.ref})
	if err := b.retry(taskCtx, func(ctx context.Context) error {
		return b.retryFreshAuthorization(ctx, func() error {
			return b.send(ctx, b.launcherSession(), launch.ReplyTo, launch.TaskID, accepted)
		})
	}); err != nil {
		record.State, record.Terminal = recordTerminal, failedBody(launch.TaskID, err)
	} else {
		result, runErr := b.runner.Run(taskCtx, task)
		if runErr != nil {
			record.State, record.Terminal = recordTerminal, failedBody(launch.TaskID, runErr)
		} else {
			record.State, record.Terminal = recordTerminal, resultBody(launch.TaskID, result.Report)
		}
	}
	_ = b.superviseTerminal(lifecycle, record)
}

func (b *Broker) prepareTerminal(ctx context.Context, record TaskRecord) error {
	child, err := b.registerChild(ctx, record.Launch)
	if err != nil {
		if !retryable(err) {
			return b.failWithoutChild(ctx, record, fmt.Errorf("register terminal eyes child: %w", err))
		}
		return fmt.Errorf("register terminal eyes child: %w", err)
	}
	record.Child, record.ChildSession, record.ChildSecret = child.ref, child.session.ID, child.session.Secret
	if err := b.journal.Put(record); err != nil {
		b.deregisterBounded(child.session, record.Launch.Scope)
		return err
	}
	b.start(func() { _ = b.superviseTerminal(ctx, record) })
	return nil
}

func (b *Broker) failWithoutChild(ctx context.Context, record TaskRecord, failure error) error {
	if len(record.Terminal) == 0 {
		record.State, record.Terminal = recordTerminal, failedBody(record.Launch.TaskID, failure)
	}
	return b.superviseTerminal(ctx, record)
}

type childIdentity struct {
	ref     protocol.AgentRef
	session Session
}

func (b *Broker) registerChild(ctx context.Context, launch protocol.TaskLaunchMsg) (childIdentity, error) {
	id := "eyes-" + launch.TaskID
	var launcher Session
	var response protocol.Response
	err := b.retry(ctx, func(callCtx context.Context) error {
		return b.retryFreshAuthorization(callCtx, func() error {
			launcher = b.launcherSession()
			auth := Session{Token: launcher.Token, ID: id, Secret: launcher.Secret}
			request := protocol.Request{
				Op: protocol.OpRegister, Scope: launch.Scope, Source: "hostbroker", Kind: protocol.KindEyes,
				AuthSessionID: launcher.ID, Platform: "windows", Capabilities: append([]string(nil), b.capabilities...),
			}
			var err error
			response, err = b.call(callCtx, auth, request)
			return err
		})
	})
	if err != nil {
		return childIdentity{}, err
	}
	if response.Name == "" || len(response.SessionSecret) < 32 || len(response.SessionSecret) > 512 || strings.ContainsAny(response.SessionSecret, "\x00\r\n \t") {
		return childIdentity{}, errors.New("relay returned incomplete eyes identity")
	}
	return childIdentity{
		ref:     protocol.AgentRef{Name: response.Name, AgentID: response.AgentID, Scope: launch.Scope},
		session: Session{Token: launcher.Token, ID: id, Secret: response.SessionSecret},
	}, nil
}

func (b *Broker) recover(ctx context.Context) error {
	for _, record := range b.journal.Records() {
		if record.State == recordDelivered {
			continue
		}
		if record.State == recordTerminal && record.ChildSession == "" {
			if err := b.superviseTerminal(ctx, record); err != nil {
				return err
			}
			continue
		}
		if record.ChildSession == "" {
			child, err := b.registerChild(ctx, record.Launch)
			if err != nil {
				if !retryable(err) {
					if err := b.failWithoutChild(ctx, record, fmt.Errorf("register recovery eyes child: %w", err)); err != nil {
						return err
					}
					continue
				}
				return err
			}
			record.Child, record.ChildSession, record.ChildSecret = child.ref, child.session.ID, child.session.Secret
		}
		if record.State != recordTerminal {
			failure := "broker restarted before accepting task; execution was not started"
			if record.State == recordAccepted {
				failure = "broker restarted after accepting task; execution was not repeated"
			}
			record.State, record.Terminal = recordTerminal, failedBody(record.Launch.TaskID, failure)
		}
		if err := b.superviseTerminal(ctx, record); err != nil {
			return err
		}
	}
	return nil
}

func (b *Broker) superviseTerminal(ctx context.Context, record TaskRecord) error {
	durable := context.WithoutCancel(ctx)
	if err := b.retryAll(durable, func(context.Context) error { return b.journal.Put(record) }); err != nil {
		return err
	}
	if record.ChildSession == "" {
		return b.deliverLauncherTerminal(ctx, durable, record)
	}
	return b.deliverTerminal(ctx, durable, record)
}

func (b *Broker) deliverLauncherTerminal(ctx, durable context.Context, record TaskRecord) error {
	if err := b.retryAll(ctx, func(callCtx context.Context) error {
		return b.withFreshToken(callCtx, func(session Session) error {
			return b.send(callCtx, session, record.Launch.ReplyTo, record.Launch.TaskID, record.Terminal)
		})
	}); err != nil {
		return err
	}
	return b.retryAll(durable, func(context.Context) error {
		return b.journal.Put(TaskRecord{Launch: record.Launch, State: recordDelivered})
	})
}

func (b *Broker) deliverTerminal(ctx, durable context.Context, record TaskRecord) error {
	var session Session
	if err := b.retryAll(ctx, func(callCtx context.Context) error {
		return b.withFreshToken(callCtx, func(launcher Session) error {
			session = Session{Token: launcher.Token, ID: record.ChildSession, Secret: record.ChildSecret}
			return b.send(callCtx, session, record.Launch.ReplyTo, record.Launch.TaskID, record.Terminal)
		})
	}); err != nil {
		return err
	}
	tombstone := TaskRecord{Launch: record.Launch, State: recordDelivered}
	if err := b.retryAll(durable, func(context.Context) error { return b.journal.Put(tombstone) }); err != nil {
		return err
	}
	b.deregisterBounded(session, record.Launch.Scope)
	return nil
}

func (b *Broker) send(ctx context.Context, session Session, target protocol.AgentRef, taskID string, body []byte) error {
	_, err := b.call(ctx, session, protocol.Request{
		Op: protocol.OpSendWorkspace, Target: &target, Body: string(body), TaskID: taskID,
	})
	return err
}

func (b *Broker) call(ctx context.Context, session Session, request protocol.Request) (protocol.Response, error) {
	return b.relay.Call(ctx, session, request)
}

func (b *Broker) retry(ctx context.Context, operation func(context.Context) error) error {
	return b.retryWhile(ctx, operation, retryable)
}

func (b *Broker) retryAll(ctx context.Context, operation func(context.Context) error) error {
	return b.retryWhile(ctx, operation, func(error) bool { return true })
}

func (b *Broker) retryWhile(ctx context.Context, operation func(context.Context) error, shouldRetry func(error) bool) error {
	delay := b.opts.BackoffMin
	for {
		if err := operation(ctx); err == nil {
			return nil
		} else if !shouldRetry(err) {
			return err
		}
		wait := delay + b.opts.Jitter(delay/4)
		if wait < delay || wait > b.opts.BackoffMax {
			wait = b.opts.BackoffMax
		}
		if err := b.opts.Sleep(ctx, wait); err != nil {
			return err
		}
		if delay < b.opts.BackoffMax/2 {
			delay *= 2
		} else {
			delay = b.opts.BackoffMax
		}
	}
}

func (b *Broker) retryFreshAuthorization(ctx context.Context, operation func() error) error {
	if err := b.refreshCredential(ctx); err != nil {
		return err
	}
	token := b.launcherSession().Token
	err := operation()
	if retryable(err) {
		return err
	}
	if refreshErr := b.refreshCredential(ctx); refreshErr != nil {
		return refreshErr
	}
	if b.launcherSession().Token == token {
		return err
	}
	return operation()
}

func (b *Broker) withFreshToken(ctx context.Context, operation func(Session) error) error {
	if err := b.refreshCredential(ctx); err != nil {
		return err
	}
	return operation(b.launcherSession())
}

func (b *Broker) refreshCredential(ctx context.Context) error {
	if err := b.flushCredential(ctx); err != nil {
		return err
	}
	stored, err := b.store.Load(ctx)
	if err != nil {
		return err
	}
	if err := stored.Validate(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.credential.LauncherSession == "" {
		b.credential = stored
		return nil
	}
	if stored.LauncherSession != b.credential.LauncherSession || stored.SessionSecret != b.credential.SessionSecret {
		return errors.New("stored launcher identity changed while broker was running")
	}
	b.credential.Token = stored.Token
	return nil
}

func (b *Broker) flushCredential(ctx context.Context) error {
	b.mu.Lock()
	credential, dirty := b.credential, b.credentialDirty
	b.mu.Unlock()
	if !dirty {
		return nil
	}
	err := b.store.Update(ctx, func(stored *Credential) error {
		if stored.LauncherSession != credential.LauncherSession || stored.SessionSecret != "" && stored.SessionSecret != credential.SessionSecret {
			return errors.New("stored launcher identity changed while broker was running")
		}
		credential.Token = stored.Token
		*stored = credential
		return stored.Validate()
	})
	if err != nil {
		return fmt.Errorf("persist launcher credential: %w", err)
	}
	b.mu.Lock()
	if b.credential.LauncherSession == credential.LauncherSession && b.credential.SessionSecret == credential.SessionSecret {
		b.credential.Token = credential.Token
		b.credentialDirty = false
	}
	b.mu.Unlock()
	return nil
}

func (b *Broker) launcherSession() Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Session{Token: b.credential.Token, ID: b.credential.LauncherSession, Secret: b.credential.SessionSecret}
}

func (b *Broker) launcherRef() protocol.AgentRef {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.launcher
}

func (b *Broker) hostScope() string { return "host:" + b.opts.ComputerName }

func (b *Broker) activate(taskID string, cancel context.CancelFunc) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.activeID != "" {
		return false
	}
	b.activeID, b.activeCancel = taskID, cancel
	return true
}

func (b *Broker) deactivate(taskID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.activeID == taskID {
		b.activeID, b.activeCancel = "", nil
	}
}

func (b *Broker) cancel(taskID string) {
	b.mu.Lock()
	cancel := b.activeCancel
	active := b.activeID == taskID && cancel != nil
	b.mu.Unlock()
	if active {
		cancel()
		_ = b.runner.Cancel(taskID)
	}
}

func (b *Broker) isBusy() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activeID != ""
}

func (b *Broker) start(work func()) {
	b.workers.Add(1)
	go func() {
		defer b.workers.Done()
		work()
	}()
}

func (b *Broker) shutdown() {
	b.mu.Lock()
	cancel, taskID := b.activeCancel, b.activeID
	b.mu.Unlock()
	if cancel != nil {
		cancel()
		_ = b.runner.Cancel(taskID)
	}
	b.workers.Wait()
	b.deregisterBounded(b.launcherSession(), b.hostScope())
}

func (b *Broker) deregister(ctx context.Context, session Session, scope string) {
	_, _ = b.call(ctx, session, protocol.Request{Op: protocol.OpDeregister, Scope: scope})
}

func (b *Broker) deregisterBounded(session Session, scope string) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	b.deregister(ctx, session, scope)
}

func normalizeCapabilities(values []string) ([]string, error) {
	result := append([]string(nil), values...)
	slices.Sort(result)
	result = slices.Compact(result)
	for _, value := range result {
		if value == "" || len(value) > 128 || strings.ContainsAny(value, "\x00\r\n \t") {
			return nil, errors.New("readiness probe returned an invalid capability")
		}
	}
	return result, nil
}

func decodeStrict(body string, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("task envelope contains trailing data")
	}
	return nil
}

func resultBody(taskID string, report hostrunner.Report) json.RawMessage {
	body, _ := json.Marshal(protocol.TaskResultMsg{
		Type: protocol.TaskResult, TaskID: taskID, Status: report.Status, Summary: report.Summary,
		Observations: report.Observations, Actions: report.Actions, Evidence: report.Evidence, Error: report.Error,
	})
	return body
}

func failedBody(taskID string, failure any) json.RawMessage {
	message := strings.TrimSpace(fmt.Sprint(failure))
	if message == "" {
		message = "host task failed"
	}
	if len(message) > 4096 {
		message = message[:4096]
	}
	body, _ := json.Marshal(protocol.TaskFailedMsg{Type: protocol.TaskFailed, TaskID: taskID, Error: message})
	return body
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

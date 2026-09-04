package store

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

// seedTask writes an eyes_tasks row directly: the busy and staleness rules
// are about the row, not about how it was created.
func seedTask(t *testing.T, s *Store, taskID, launcherSession, state string, updatedAt int64) {
	t.Helper()
	seedTaskIn(t, s, "/r", taskID, launcherSession, state, updatedAt)
}

// seedTaskIn is the same for a requester in some other workspace.
func seedTaskIn(t *testing.T, s *Store, scope, taskID, launcherSession, state string, updatedAt int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO eyes_tasks
		(task_id, requester_scope, requester_agent_id, launcher_session, runtime, state, created_at, updated_at)
		VALUES (?,?,?,?,'claude',?,?,?)`, taskID, scope, "aid-a", launcherSession, state, updatedAt, updatedAt); err != nil {
		t.Fatal(err)
	}
}

// The launcher busy check runs on every pick, so eyes_tasks carries the index
// that backs it.
func TestEyesTasksLauncherStateIndex(t *testing.T) {
	s := open(t)
	var name string
	if err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='eyes_tasks'
		AND name='idx_eyes_tasks_launcher_state'`).Scan(&name); err != nil {
		t.Fatalf("eyes_tasks(launcher_session, state) index: %v", err)
	}
}

// A launcher that stopped polling cannot pick up a task: the window is 30s,
// not the 2 minute presence window.
func TestPickLauncherRequiresRecentPoll(t *testing.T) {
	s := open(t)
	now := time.Unix(1000000, 0)
	s.Now = func() time.Time { return now }
	registerBroker(t, s, "host:BOX", "broker-1")
	l, err := s.PickLauncher()
	if err != nil || l.SessionID != "broker-1" || l.Scope != "host:BOX" {
		t.Fatalf("fresh launcher: %+v (%v)", l, err)
	}
	now = now.Add(45 * time.Second) // still "active" for presence, but no longer polling
	if _, err := s.PickLauncher(); !errors.Is(err, ErrNoLauncher) {
		t.Fatalf("stale launcher must not be picked, got %v", err)
	}
}

// Only a relay-registered launcher in a reserved host: scope is a broker.
// AC_KIND is plumbed from local callers, so a local agent calling itself a
// launcher must never absorb a brief.
func TestPickLauncherRequiresRelayOriginAndHostScope(t *testing.T) {
	s := open(t)
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "/r", SessionID: "local-1",
		Kind: protocol.KindLauncher}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PickLauncher(); !errors.Is(err, ErrNoLauncher) {
		t.Fatalf("a local kind=launcher row must not be pickable, got %v", err)
	}
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "/r", SessionID: "relay-1",
		Kind: protocol.KindLauncher, Origin: "relay", Secret: brokerSecret("relay-1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PickLauncher(); !errors.Is(err, ErrNoLauncher) {
		t.Fatalf("a launcher outside a host: scope must not be pickable, got %v", err)
	}
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "Host:BOX", SessionID: "case-1",
		Kind: protocol.KindLauncher, Origin: "relay", Secret: brokerSecret("case-1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PickLauncher(); !errors.Is(err, ErrNoLauncher) {
		t.Fatalf("host: is one exact prefix - a scope ListWorkspaces lists must not be pickable, got %v", err)
	}
	registerBroker(t, s, "host:BOX", "broker-1")
	if l, err := s.PickLauncher(); err != nil || l.SessionID != "broker-1" {
		t.Fatalf("the host broker: %+v (%v)", l, err)
	}
}

// A launcher running a task is busy: the pick skips it, and when every live
// launcher is busy the caller gets ErrEyesBusy rather than "no host launcher".
func TestPickLauncherSkipsBusyLauncher(t *testing.T) {
	s := open(t)
	now := time.Unix(3000000, 0)
	s.Now = func() time.Time { return now }
	registerBroker(t, s, "host:BOX", "broker-1")
	now = now.Add(time.Second) // broker-2 is the more recently seen launcher
	registerBroker(t, s, "host:BOX", "broker-2")
	seedTask(t, s, "task-1", "broker-2", "queued", now.Unix())
	l, err := s.PickLauncher()
	if err != nil || l.SessionID != "broker-1" {
		t.Fatalf("a busy launcher must be skipped: %+v (%v)", l, err)
	}
	seedTask(t, s, "task-2", "broker-1", "accepted", now.Unix())
	if _, err := s.PickLauncher(); !errors.Is(err, ErrEyesBusy) {
		t.Fatalf("every launcher busy must be ErrEyesBusy, got %v", err)
	}
	if _, err := s.db.Exec(`UPDATE eyes_tasks SET state='done' WHERE task_id='task-1'`); err != nil {
		t.Fatal(err)
	}
	if l, err := s.PickLauncher(); err != nil || l.SessionID != "broker-2" {
		t.Fatalf("a finished task frees its launcher: %+v (%v)", l, err)
	}
}

// Eyes work means driving a browser, so a broker that runs a provider but
// cannot reach Chrome takes no brief at all - it is "no matching provider",
// not a launcher that happens to be free.
func TestPickLauncherRequiresTheBrowserCap(t *testing.T) {
	s := open(t)
	now := time.Unix(4000000, 0)
	s.Now = func() time.Time { return now }
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "browserless",
		Kind: protocol.KindLauncher, Origin: "relay", Secret: brokerSecret("browserless"),
		Capabilities: []string{"provider.claude"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PickLauncher(); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("a broker without browser.chrome must be ErrNoProvider, got %v", err)
	}
	registerBroker(t, s, "host:BOX", "broker-1")
	if l, err := s.PickLauncher(); err != nil || l.SessionID != "broker-1" {
		t.Fatalf("the broker that can drive Chrome: %+v (%v)", l, err)
	}
	// Precedence still runs match, then busy: the browserless row cannot stand
	// in for the one broker that could have taken the job.
	seedTask(t, s, "task-1", "broker-1", "accepted", now.Unix())
	if _, err := s.PickLauncher(); !errors.Is(err, ErrEyesBusy) {
		t.Fatalf("the only capable broker is busy, got %v", err)
	}
}

// One request_eyes is one transaction: reserve the launcher, queue the task
// and deliver task.launch, or do none of it.
func TestAssignEyesTaskQueuesAndDelivers(t *testing.T) {
	s := open(t)
	now := time.Unix(5000000, 0)
	s.Now = func() time.Time { return now }
	requester, _ := s.Register("/r", "s-a", "hook")
	broker := registerBroker(t, s, "host:BOX", "broker-1")
	ref := protocol.AgentRef{Name: requester, AgentID: agentID("s-a"), Scope: "/r"}

	task, launcher, err := s.AssignEyesTask(EyesRequest{Requester: ref, Runtime: "claude", Brief: "read the page"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(task.TaskID, "task-") || len(task.TaskID) != len("task-")+12 {
		t.Fatalf("task id %q must be task- plus 12 hex", task.TaskID)
	}
	if _, err := hex.DecodeString(task.TaskID[len("task-"):]); err != nil {
		t.Fatalf("task id %q must be hex: %v", task.TaskID, err)
	}
	if task.State != "queued" || task.LauncherSession != "broker-1" || task.RequesterScope != "/r" ||
		task.RequesterAgentID != ref.AgentID || task.Runtime != "claude" || task.UpdatedAt != now.Unix() {
		t.Fatalf("queued task: %+v", task)
	}
	if launcher.Name != broker.Name || launcher.Scope != "host:BOX" {
		t.Fatalf("launcher ref: %+v", launcher)
	}
	// The launch belongs to the ledger, which knows whether it is still owed;
	// it is never ordinary mail a broker could read after a cancel.
	if inbox, err := s.ReadBroker("host:BOX", broker.Name); err != nil || len(inbox) != 0 {
		t.Fatalf("a broker's poll never sees the raw launch: %+v (%v)", inbox, err)
	}
	owed, err := s.PendingLaunches("broker-1")
	if err != nil || len(owed) != 1 {
		t.Fatalf("launcher queue: %d (%v)", len(owed), err)
	}
	m := owed[0]
	if m.TaskID != task.TaskID || m.From != requester || m.FromScope != "/r" || m.ReplyTo == nil || m.ReplyTo.AgentID != ref.AgentID {
		t.Fatalf("launch mail: %+v (reply_to %+v)", m, m.ReplyTo)
	}
	var body protocol.TaskLaunchMsg
	if err := json.Unmarshal([]byte(m.Body), &body); err != nil {
		t.Fatalf("launch body %q: %v", m.Body, err)
	}
	if body.Type != protocol.TaskLaunch || body.TaskID != task.TaskID || body.Runtime != "claude" ||
		body.Scope != "/r" || body.Brief != "read the page" || body.DeadlineS != 300 || body.ReplyTo.AgentID != ref.AgentID {
		t.Fatalf("launch body: %+v", body)
	}
	// The launcher now holds a live task, so a second request is refused and
	// writes nothing.
	if _, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Brief: "again"}); !errors.Is(err, ErrEyesBusy) {
		t.Fatalf("a busy launcher must be ErrEyesBusy, got %v", err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM eyes_tasks`); n != 1 {
		t.Fatalf("a refused request must queue nothing, got %d tasks", n)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("a refused request must send nothing, got %d messages", n)
	}
}

// deadline_s defaults to 300 and is capped at 1800 - the requester's bound on
// a host job, applied where the launch body is built.
func TestAssignEyesTaskCapsDeadline(t *testing.T) {
	s := open(t)
	requester, _ := s.Register("/r", "s-a", "hook")
	registerBroker(t, s, "host:BOX", "broker-1")
	ref := protocol.AgentRef{Name: requester, AgentID: agentID("s-a"), Scope: "/r"}
	if _, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Brief: "b", DeadlineS: 9999}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := s.PendingLaunches("broker-1")
	var body protocol.TaskLaunchMsg
	if len(msgs) != 1 || json.Unmarshal([]byte(msgs[0].Body), &body) != nil || body.DeadlineS != 1800 {
		t.Fatalf("deadline must be capped at 1800: %+v", body)
	}
}

// No broker means no task and no mail: the caller has to say the host is not
// running rather than silently queue work nobody will see.
func TestAssignEyesTaskWithoutLauncherWritesNothing(t *testing.T) {
	s := open(t)
	requester, _ := s.Register("/r", "s-a", "hook")
	ref := protocol.AgentRef{Name: requester, AgentID: agentID("s-a"), Scope: "/r"}
	if _, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Brief: "b"}); !errors.Is(err, ErrNoLauncher) {
		t.Fatalf("no launcher: %v", err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM eyes_tasks`); n != 0 {
		t.Fatalf("nothing must be queued, got %d tasks", n)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Fatalf("nothing must be sent, got %d messages", n)
	}
}

// Transitions are validated against the actor and the current state, and the
// state change plus the requester's mail are one write - so a re-sent report
// is a no-op, not a second message.
func TestTransitionEyesTaskEdgesActorsAndIdempotency(t *testing.T) {
	s := open(t)
	requester, _ := s.Register("/r", "s-a", "hook")
	broker := registerBroker(t, s, "host:BOX", "broker-1")
	ref := protocol.AgentRef{Name: requester, AgentID: agentID("s-a"), Scope: "/r"}
	task, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	childSession := "eyes-" + task.TaskID
	child, err := s.ReissueEyesChild(task.TaskID, "broker-1", broker.Secret)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.TransitionEyesTask("task-nope", relayActor("host:BOX", "broker-1"), "accepted", "{}"); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("unknown task: %v", err)
	}
	if _, _, err := s.TransitionEyesTask(task.TaskID, relayActor("/r", childSession), "accepted", "{}"); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("only the assigned launcher may accept, got %v", err)
	}
	if _, _, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "queued", "{}"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("queued is not a target state, got %v", err)
	}
	if _, _, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "done", "{}"); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("only the child reports done, got %v", err)
	}

	accepted, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "accepted",
		`{"type":"task.accepted","task_id":"`+task.TaskID+`"}`)
	if err != nil || !ok || accepted.State != "accepted" {
		t.Fatalf("accept: %+v ok=%v (%v)", accepted, ok, err)
	}
	if n, _ := s.UnreadCount("/r", requester); n != 1 {
		t.Fatalf("the requester must be told once, unread=%d", n)
	}
	if _, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "accepted", "{}"); err != nil || ok {
		t.Fatalf("a repeated accept must be a quiet no-op: ok=%v (%v)", ok, err)
	}
	if n, _ := s.UnreadCount("/r", requester); n != 1 {
		t.Fatalf("a repeat must not send a second mail, unread=%d", n)
	}

	report := `{"type":"task.result","task_id":"` + task.TaskID + `","status":"ok"}`
	done, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("/r", childSession), "done", report)
	if err != nil || !ok || done.State != "done" {
		t.Fatalf("done: %+v ok=%v (%v)", done, ok, err)
	}
	if _, _, err := s.TransitionEyesTask(task.TaskID, relayActor("/r", childSession), "failed", "{}"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("done is terminal, got %v", err)
	}
	msgs, err := s.Read("/r", requester)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("requester mail: %d (%v)", len(msgs), err)
	}
	if msgs[0].From != broker.Name || msgs[0].FromScope != "host:BOX" || msgs[0].Kind != protocol.KindLauncher ||
		msgs[0].TaskID != task.TaskID {
		t.Fatalf("the accept mail comes from the launcher: %+v", msgs[0])
	}
	if msgs[1].From != child.Name || msgs[1].Kind != protocol.KindEyes || msgs[1].Body != report ||
		msgs[1].ReplyTo == nil || msgs[1].ReplyTo.Name != child.Name {
		t.Fatalf("the report comes from the child: %+v (reply_to %+v)", msgs[1], msgs[1].ReplyTo)
	}
	// A settled task frees its launcher for the next brief.
	if l, err := s.PickLauncher(); err != nil || l.SessionID != "broker-1" {
		t.Fatalf("a finished task frees the launcher: %+v (%v)", l, err)
	}
}

// A launcher that never accepted can still fail its task cleanly.
func TestTransitionEyesTaskLauncherMayFailQueued(t *testing.T) {
	s := open(t)
	requester, _ := s.Register("/r", "s-a", "hook")
	registerBroker(t, s, "host:BOX", "broker-1")
	ref := protocol.AgentRef{Name: requester, AgentID: agentID("s-a"), Scope: "/r"}
	task, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	failed, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "failed",
		`{"type":"task.failed","task_id":"`+task.TaskID+`","error":"no provider"}`)
	if err != nil || !ok || failed.State != "failed" {
		t.Fatalf("queued -> failed by the launcher: %+v ok=%v (%v)", failed, ok, err)
	}
	if n, _ := s.UnreadCount("/r", requester); n != 1 {
		t.Fatalf("the requester must hear about the failure, unread=%d", n)
	}
}

// A task is owned by its requester (or one of that agent's bound children),
// and Housekeep sweeps it after a day.
func TestEyesTaskOwnershipAndHousekeep(t *testing.T) {
	s := open(t)
	now := time.Unix(2000000, 0)
	s.Now = func() time.Time { return now }
	nA, _ := s.Register("/r", "s-a", "hook")
	childName, err := s.RegisterChild("/r", "s-a", "sub1", "Explore")
	if err != nil {
		t.Fatal(err)
	}
	nB, _ := s.Register("/r", "s-b", "hook")
	registerBroker(t, s, "host:BOX", "broker-1")
	requester := protocol.AgentRef{Name: nA, AgentID: agentID("s-a"), Scope: "/r"}
	task, _, err := s.AssignEyesTask(EyesRequest{Requester: requester, Runtime: "claude", Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.EyesTask(task.TaskID)
	if err != nil || got.State != "queued" || got.LauncherSession != "broker-1" || got.CreatedAt != now.Unix() {
		t.Fatalf("stored task: %+v (%v)", got, err)
	}
	if _, err := s.EyesTask("task-nope"); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("unknown task: %v", err)
	}
	stranger := protocol.AgentRef{Name: nB, AgentID: agentID("s-b"), Scope: "/r"}
	if _, err := s.CancelEyesTask(task.TaskID, localActor(stranger)); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("a stranger must not cancel: %v", err)
	}
	child := protocol.AgentRef{Name: childName, AgentID: agentID(ChildSessionID("s-a", "sub1")), Scope: "/r"}
	cancelled, err := s.CancelEyesTask(task.TaskID, localActor(child))
	if err != nil || cancelled.State != "cancelled" || cancelled.LauncherSession != "broker-1" {
		t.Fatalf("a bound child may cancel its parent's task: %+v (%v)", cancelled, err)
	}
	now = now.Add(25 * time.Hour)
	if err := s.Housekeep(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EyesTask(task.TaskID); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("a task older than 24h must be purged, got %v", err)
	}
}

// liveTask sets up the usual cast: a requester, a host broker, and one queued
// task whose child session has already been minted.
func liveTask(t *testing.T, s *Store) (protocol.AgentRef, RelayResult, EyesTask, RelayResult) {
	t.Helper()
	name, _ := s.Register("/r", "s-a", "hook")
	broker := registerBroker(t, s, "host:BOX", "broker-1")
	requester := protocol.AgentRef{Name: name, AgentID: agentID("s-a"), Scope: "/r"}
	task, _, err := s.AssignEyesTask(EyesRequest{Requester: requester, Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.ReissueEyesChild(task.TaskID, "broker-1", broker.Secret)
	if err != nil {
		t.Fatal(err)
	}
	return requester, broker, task, child
}

// The child owns the outcome and the launcher owns the ack: a result needs an
// accepted task, and the launcher may fail only a task it never accepted -
// once it has one, the child is the only thing that can report.
func TestTransitionEyesTaskEdgeTable(t *testing.T) {
	s := open(t)
	_, _, task, _ := liveTask(t, s)
	child := "eyes-" + task.TaskID
	if _, _, err := s.TransitionEyesTask(task.TaskID, relayActor("/r", child), "done", "{}"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("a queued task has no result to report, got %v", err)
	}
	if _, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("/r", child), "failed", "{}"); err != nil || !ok {
		t.Fatalf("the child may fail a queued task: ok=%v (%v)", ok, err)
	}

	s2 := open(t)
	_, _, task2, _ := liveTask(t, s2)
	if _, ok, err := s2.TransitionEyesTask(task2.TaskID, relayActor("host:BOX", "broker-1"), "accepted", "{}"); err != nil || !ok {
		t.Fatalf("accept: ok=%v (%v)", ok, err)
	}
	if _, _, err := s2.TransitionEyesTask(task2.TaskID, relayActor("host:BOX", "broker-1"), "failed", "{}"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("an accepted task fails through its child, got %v", err)
	}
	if _, ok, err := s2.TransitionEyesTask(task2.TaskID, relayActor("/r", "eyes-"+task2.TaskID), "failed", "{}"); err != nil || !ok {
		t.Fatalf("the child may fail an accepted task: ok=%v (%v)", ok, err)
	}
}

// Cancel is a lifecycle move like any other: the requester's side only, the
// launcher's task.cancel mail in the same write, a repeat that changes
// nothing, and no cancel at all once the task has settled.
func TestCancelEyesTaskIsATransition(t *testing.T) {
	s := open(t)
	requester, broker, task, _ := liveTask(t, s)
	otherName, _ := s.Register("/r", "s-b", "hook")
	stranger := protocol.AgentRef{Name: otherName, AgentID: agentID("s-b"), Scope: "/r"}
	if _, err := s.CancelEyesTask(task.TaskID, localActor(stranger)); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("a stranger must not cancel, got %v", err)
	}
	cancelled, err := s.CancelEyesTask(task.TaskID, localActor(requester))
	if err != nil || cancelled.State != "cancelled" {
		t.Fatalf("cancel: %+v (%v)", cancelled, err)
	}
	if inbox, err := s.ReadBroker("host:BOX", broker.Name); err != nil || len(inbox) != 0 {
		t.Fatalf("a broker's poll sees neither raw body: %+v (%v)", inbox, err)
	}
	msgs, err := s.PendingCancels("broker-1")
	if err != nil || len(msgs) != 1 {
		t.Fatalf("the launcher is owed the cancel: %d (%v)", len(msgs), err)
	}
	var body protocol.TaskCancelMsg
	if err := json.Unmarshal([]byte(msgs[0].Body), &body); err != nil {
		t.Fatalf("cancel body %q: %v", msgs[0].Body, err)
	}
	if body.Type != protocol.TaskCancel || body.TaskID != task.TaskID ||
		msgs[0].TaskID != task.TaskID || msgs[0].From != requester.Name || msgs[0].FromScope != "/r" {
		t.Fatalf("cancel mail: %+v body %+v", msgs[0], body)
	}
	if again, err := s.CancelEyesTask(task.TaskID, localActor(requester)); err != nil || again.State != "cancelled" {
		t.Fatalf("a repeated cancel must be a quiet no-op: %+v (%v)", again, err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM messages WHERE task_id=?`, task.TaskID); n != 2 {
		t.Fatalf("a repeat must not send a second cancel, got %d task messages", n)
	}
	// A cancelled task still reserves its broker: the provider keeps running
	// until the broker reports that it stopped, and that ack is what frees it.
	if _, err := s.PickLauncher(); !errors.Is(err, ErrEyesBusy) {
		t.Fatalf("an unacked cancel still holds the launcher, got %v", err)
	}
	if _, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("/r", "eyes-"+task.TaskID), "failed",
		`{"type":"task.failed","task_id":"`+task.TaskID+`","error":"cancelled"}`); err != nil || ok {
		t.Fatalf("the child's ack: ok=%v (%v)", ok, err)
	}
	if l, err := s.PickLauncher(); err != nil || l.SessionID != "broker-1" {
		t.Fatalf("the ack frees the launcher: %+v (%v)", l, err)
	}

	s2 := open(t)
	requester2, _, task2, _ := liveTask(t, s2)
	if _, ok, err := s2.TransitionEyesTask(task2.TaskID, relayActor("host:BOX", "broker-1"), "accepted", "{}"); err != nil || !ok {
		t.Fatalf("accept: ok=%v (%v)", ok, err)
	}
	if got, err := s2.CancelEyesTask(task2.TaskID, localActor(requester2)); err != nil || got.State != "cancelled" {
		t.Fatalf("an accepted task is still cancellable: %+v (%v)", got, err)
	}
	// A cancelled task is settled, so the child's report moves nothing - it
	// acknowledges the cancel instead of reopening the lifecycle.
	if got, ok, err := s2.TransitionEyesTask(task2.TaskID, relayActor("/r", "eyes-"+task2.TaskID), "done", "{}"); err != nil || ok || got.State != "cancelled" {
		t.Fatalf("a cancelled task is settled: %+v ok=%v (%v)", got, ok, err)
	}
}

// A brief names the runtime it needs, so the broker that takes it must
// advertise that provider. "no host launcher", "no matching provider" and
// "eyes busy" are three different answers, in that order of precedence.
func TestAssignEyesTaskMatchesProvider(t *testing.T) {
	s := open(t)
	name, _ := s.Register("/r", "s-a", "hook")
	ref := protocol.AgentRef{Name: name, AgentID: agentID("s-a"), Scope: "/r"}
	if _, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Runtime: "claude", Brief: "b"}); !errors.Is(err, ErrNoLauncher) {
		t.Fatalf("no broker at all: %v", err)
	}
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay", Platform: "windows",
		Secret: brokerSecret("broker-1"), Capabilities: []string{"browser.chrome", "provider.claude"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Runtime: "codex", Brief: "b"}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("a broker without the provider: %v", err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM eyes_tasks`); n != 0 {
		t.Fatalf("an unmatched request must queue nothing, got %d tasks", n)
	}
	task, launcher, err := s.AssignEyesTask(EyesRequest{Requester: ref, Runtime: "claude", Brief: "b"})
	if err != nil || launcher.Scope != "host:BOX" {
		t.Fatalf("the advertised provider matches: %+v (%v)", launcher, err)
	}
	// A second broker with a different provider: busy still beats "no
	// provider" for a runtime that exists, and a runtime nobody has is
	// ErrNoProvider even while a free broker is polling.
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-2",
		Kind: protocol.KindLauncher, Origin: "relay", Platform: "windows",
		Secret: brokerSecret("broker-2"), Capabilities: []string{"browser.chrome", "provider.codex"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Runtime: "claude", Brief: "b"}); !errors.Is(err, ErrEyesBusy) {
		t.Fatalf("the only claude broker is busy: %v", err)
	}
	if _, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Runtime: "grok", Brief: "b"}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("nobody runs grok: %v", err)
	}
	// No runtime asked for means any live broker will do.
	if _, l, err := s.AssignEyesTask(EyesRequest{Requester: ref, Brief: "b"}); err != nil || l.Name == "" {
		t.Fatalf("an unqualified brief takes any broker: %+v (%v)", l, err)
	}
	if _, err := s.EyesTask(task.TaskID); err != nil {
		t.Fatalf("the first task must still be there: %v", err)
	}
}

// A launcher that lost its inbox read can ask for its queued work again: the
// launch mail stays pending until the task leaves 'queued', and it is listed
// oldest task first.
func TestPendingLaunches(t *testing.T) {
	s := open(t)
	now := time.Unix(6000000, 0)
	s.Now = func() time.Time { return now }
	requester, broker, task, _ := liveTask(t, s)

	pending, err := s.PendingLaunches("broker-1")
	if err != nil || len(pending) != 1 || pending[0].TaskID != task.TaskID {
		t.Fatalf("queued launch: %+v (%v)", pending, err)
	}
	var body protocol.TaskLaunchMsg
	if err := json.Unmarshal([]byte(pending[0].Body), &body); err != nil || body.Type != protocol.TaskLaunch {
		t.Fatalf("launch body %q: %v", pending[0].Body, err)
	}
	if pending[0].From != requester.Name || pending[0].FromScope != "/r" || pending[0].ReplyTo == nil {
		t.Fatalf("launch mail: %+v", pending[0])
	}
	// A launch is the only pending work: other mail carrying the same task id
	// is not a brief to run.
	if err := s.SendToScope(Delivery{FromScope: "/r", FromName: requester.Name, ToScope: "host:BOX",
		ToName: broker.Name, Body: `{"type":"task.cancel","task_id":"` + task.TaskID + `"}`,
		TaskID: task.TaskID}); err != nil {
		t.Fatal(err)
	}
	if only, err := s.PendingLaunches("broker-1"); err != nil || len(only) != 1 || only[0].TaskID != task.TaskID {
		t.Fatalf("only the launch is pending: %+v (%v)", only, err)
	}
	// One launcher session is one queue, even if a stray row wears its id.
	if _, err := s.db.Exec(`INSERT INTO agents (scope, session_id, agent_id, name, status, registered_at, last_seen, source)
		VALUES ('/other','broker-1','aid-dup','dup-broker','active',?,?,'relay')`, now.Unix()+5, now.Unix()+5); err != nil {
		t.Fatal(err)
	}
	if only, err := s.PendingLaunches("broker-1"); err != nil || len(only) != 1 {
		t.Fatalf("a duplicate session row must not fan the queue out: %+v (%v)", only, err)
	}
	// Reading the inbox must not consume it: re-delivery is the point.
	if _, err := s.Read("host:BOX", broker.Name); err != nil {
		t.Fatal(err)
	}
	if again, err := s.PendingLaunches("broker-1"); err != nil || len(again) != 1 {
		t.Fatalf("a read launch is still pending: %+v (%v)", again, err)
	}
	// An older queued task sorts first, and another broker's work is not ours.
	seedTask(t, s, "task-old", "broker-1", "queued", now.Unix()-60)
	if err := s.SendToScope(Delivery{FromScope: "/r", FromName: requester.Name, ToScope: "host:BOX",
		ToName: broker.Name, Body: `{"type":"task.launch","task_id":"task-old"}`, TaskID: "task-old"}); err != nil {
		t.Fatal(err)
	}
	pending, err = s.PendingLaunches("broker-1")
	if err != nil || len(pending) != 2 || pending[0].TaskID != "task-old" || pending[1].TaskID != task.TaskID {
		t.Fatalf("oldest task first: %+v (%v)", pending, err)
	}
	if other, err := s.PendingLaunches("broker-2"); err != nil || len(other) != 0 {
		t.Fatalf("another broker's queue: %+v (%v)", other, err)
	}
	// Accepting the task clears it: the broker has it now.
	if _, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "accepted", "{}"); err != nil || !ok {
		t.Fatalf("accept: ok=%v (%v)", ok, err)
	}
	pending, err = s.PendingLaunches("broker-1")
	if err != nil || len(pending) != 1 || pending[0].TaskID != "task-old" {
		t.Fatalf("an accepted task is no longer pending: %+v (%v)", pending, err)
	}
}

// An unqualified brief still names a runtime: the first provider the chosen
// broker advertises, in a fixed order so the same host always answers the
// same way, recorded on the task and carried in the launch.
func TestAssignEyesTaskChoosesADefaultProvider(t *testing.T) {
	for _, c := range []struct {
		caps []string
		want string
	}{
		{[]string{"provider.grok"}, "grok"},
		{[]string{"provider.codex", "provider.claude"}, "claude"},
		{[]string{"provider.grok", "provider.codex"}, "codex"},
	} {
		s := open(t)
		name, _ := s.Register("/r", "s-a", "hook")
		ref := protocol.AgentRef{Name: name, AgentID: agentID("s-a"), Scope: "/r"}
		_, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
			Kind: protocol.KindLauncher, Origin: "relay", Secret: brokerSecret("broker-1"),
			Capabilities: append([]string{"browser.chrome"}, c.caps...)})
		if err != nil {
			t.Fatal(err)
		}
		task, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Brief: "b"})
		if err != nil || task.Runtime != c.want {
			t.Fatalf("caps %v must default to %q: %+v (%v)", c.caps, c.want, task, err)
		}
		if stored, err := s.EyesTask(task.TaskID); err != nil || stored.Runtime != c.want {
			t.Fatalf("the ledger must record the runtime: %+v (%v)", stored, err)
		}
		msgs, _ := s.PendingLaunches("broker-1")
		var body protocol.TaskLaunchMsg
		if len(msgs) != 1 || json.Unmarshal([]byte(msgs[0].Body), &body) != nil || body.Runtime != c.want {
			t.Fatalf("the launch must carry runtime %q: %+v", c.want, body)
		}
	}
}

// A broker advertising no provider at all cannot take a brief, even one that
// named no runtime: there is nothing for it to run.
func TestAssignEyesTaskWithoutAnyProvider(t *testing.T) {
	s := open(t)
	name, _ := s.Register("/r", "s-a", "hook")
	ref := protocol.AgentRef{Name: name, AgentID: agentID("s-a"), Scope: "/r"}
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-1",
		Kind: protocol.KindLauncher, Origin: "relay", Secret: brokerSecret("broker-1"),
		Capabilities: []string{"browser.chrome"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Brief: "b"}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("a broker with no provider: %v", err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM eyes_tasks`); n != 0 {
		t.Fatalf("nothing must be queued, got %d tasks", n)
	}
}

// A runtime is a provider name, not free text: it reaches a capability match
// and the task ledger, so its shape is checked before either.
func TestAssignEyesTaskRejectsABadRuntime(t *testing.T) {
	s := open(t)
	name, _ := s.Register("/r", "s-a", "hook")
	registerBroker(t, s, "host:BOX", "broker-1")
	ref := protocol.AgentRef{Name: name, AgentID: agentID("s-a"), Scope: "/r"}
	for _, bad := range []string{"Claude", "cl aude", `claude"`, "claude/../x", `"`, strings.Repeat("a", 33)} {
		if _, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Runtime: bad, Brief: "b"}); !errors.Is(err, ErrBadRuntime) {
			t.Fatalf("runtime %q must be ErrBadRuntime, got %v", bad, err)
		}
	}
	if n := count(t, s, `SELECT COUNT(*) FROM eyes_tasks`); n != 0 {
		t.Fatalf("a refused request must queue nothing, got %d tasks", n)
	}
	if _, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Runtime: "claude", Brief: "b"}); err != nil {
		t.Fatalf("a provider name is fine: %v", err)
	}
}

// An agent row is purged after two hours, a task lives for a day: cancelling
// after the launcher is gone still settles the task, with nobody to tell.
func TestCancelEyesTaskAfterTheLauncherIsPurged(t *testing.T) {
	s := open(t)
	requester, _, task, _ := liveTask(t, s)
	if _, err := s.db.Exec(`DELETE FROM agents WHERE session_id='broker-1'`); err != nil {
		t.Fatal(err)
	}
	before := count(t, s, `SELECT COUNT(*) FROM messages`)
	cancelled, err := s.CancelEyesTask(task.TaskID, localActor(requester))
	if err != nil || cancelled.State != "cancelled" {
		t.Fatalf("cancel with no launcher row: %+v (%v)", cancelled, err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM messages`); n != before {
		t.Fatalf("nobody is left to tell, got %d messages (was %d)", n, before)
	}
}

// A task past its deadline is failed by the ledger itself: the requester is
// told once, as the child, and the broker is free again - expiry is what
// hands a launcher back when nobody reports.
func TestExpireEyesTasks(t *testing.T) {
	s := open(t)
	now := time.Unix(7000000, 0)
	s.Now = func() time.Time { return now }
	requester, _, task, child := liveTask(t, s)

	if n, err := s.ExpireEyesTasks(now); err != nil || n != 0 {
		t.Fatalf("a task inside its deadline must not expire: %d (%v)", n, err)
	}
	if _, err := s.db.Exec(`UPDATE eyes_tasks SET created_at=? WHERE task_id=?`,
		now.Unix()-301, task.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PickLauncher(); !errors.Is(err, ErrEyesBusy) {
		t.Fatalf("an unexpired task still holds its launcher, got %v", err)
	}
	n, err := s.ExpireEyesTasks(now)
	if err != nil || n != 1 {
		t.Fatalf("the deadline must fail the task: %d (%v)", n, err)
	}
	got, err := s.EyesTask(task.TaskID)
	if err != nil || got.State != "failed" || got.UpdatedAt != now.Unix() {
		t.Fatalf("expired task: %+v (%v)", got, err)
	}
	msgs, err := s.Read("/r", requester.Name)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("requester mail: %d (%v)", len(msgs), err)
	}
	var body protocol.TaskFailedMsg
	if err := json.Unmarshal([]byte(msgs[0].Body), &body); err != nil {
		t.Fatalf("failure body %q: %v", msgs[0].Body, err)
	}
	if body.Type != protocol.TaskFailed || body.TaskID != task.TaskID || body.Error != "deadline exceeded" ||
		msgs[0].From != child.Name || msgs[0].Kind != protocol.KindEyes || msgs[0].TaskID != task.TaskID {
		t.Fatalf("failure mail: %+v body %+v", msgs[0], body)
	}
	if l, err := s.PickLauncher(); err != nil || l.SessionID != "broker-1" {
		t.Fatalf("an expired task frees its launcher: %+v (%v)", l, err)
	}
	if pending, err := s.PendingLaunches("broker-1"); err != nil || len(pending) != 0 {
		t.Fatalf("an expired task is no longer work to run: %+v (%v)", pending, err)
	}
	if n, err := s.ExpireEyesTasks(now); err != nil || n != 0 {
		t.Fatalf("a settled task expires once: %d (%v)", n, err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM messages WHERE task_id=?`, task.TaskID); n != 2 {
		t.Fatalf("a second pass must send nothing: %d task messages", n)
	}
}

// One task the sweep cannot settle - a report that landed first, a write that
// will not go through - must not cost the others their deadline, nor cost
// Housekeep its purges.
func TestExpireEyesTasksSkipsWhatItCannotSettle(t *testing.T) {
	s := open(t)
	now := time.Unix(7300000, 0)
	s.Now = func() time.Time { return now }
	stranded, _ := s.Register("/r", "s-a", "hook")
	heard, _ := s.Register("/r", "s-b", "hook")
	registerBroker(t, s, "host:BOX", "broker-1")
	registerBroker(t, s, "host:BOX", "broker-2")
	first, _, err := s.AssignEyesTask(EyesRequest{
		Requester: protocol.AgentRef{Name: stranded, AgentID: agentID("s-a"), Scope: "/r"}, Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.AssignEyesTask(EyesRequest{
		Requester: protocol.AgentRef{Name: heard, AgentID: agentID("s-b"), Scope: "/r"}, Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	// The first task's failure mail cannot be written, so its move fails.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_expiry BEFORE INSERT ON messages
		WHEN NEW.task_id = '` + first.TaskID + `' AND NEW.body LIKE '%deadline%'
		BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	// A row old enough for Housekeep to purge, to prove the sweep runs anyway.
	if _, err := s.db.Exec(`INSERT INTO agents (scope, session_id, agent_id, name, status, registered_at, last_seen, source)
		VALUES ('/r','ghost','aid-ghost','ghost-agent','active',?,?,'hook')`,
		now.Unix()-3*3600, now.Unix()-3*3600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(301 * time.Second)

	n, err := s.ExpireEyesTasks(now)
	if n != 1 || err == nil {
		t.Fatalf("the sweep must settle the rest and report the failure: %d (%v)", n, err)
	}
	if got, _ := s.EyesTask(second.TaskID); got.State != "failed" {
		t.Fatalf("the settleable task must expire: %+v", got)
	}
	if got, _ := s.EyesTask(first.TaskID); got.State != "queued" {
		t.Fatalf("the unsettleable task must be left alone: %+v", got)
	}
	if err := s.Housekeep(); err == nil {
		t.Fatal("Housekeep must report what the sweep hit")
	}
	if n := count(t, s, `SELECT COUNT(*) FROM agents WHERE session_id='ghost'`); n != 0 {
		t.Fatalf("Housekeep must purge whatever the sweep hit, %d stale rows left", n)
	}
}

// An expiry speaks for nobody, so a task whose child and broker rows are both
// gone still settles - there is simply no voice left to report it.
func TestExpireEyesTasksWithNoVoiceLeft(t *testing.T) {
	s := open(t)
	now := time.Unix(7400000, 0)
	s.Now = func() time.Time { return now }
	requester, _, task, _ := liveTask(t, s)
	if _, err := s.db.Exec(`DELETE FROM agents WHERE session_id IN ('broker-1',?)`, "eyes-"+task.TaskID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(301 * time.Second)
	if n, err := s.ExpireEyesTasks(now); err != nil || n != 1 {
		t.Fatalf("the task must still settle: %d (%v)", n, err)
	}
	if got, _ := s.EyesTask(task.TaskID); got.State != "failed" {
		t.Fatalf("expired task: %+v", got)
	}
	if n, _ := s.UnreadCount("/r", requester.Name); n != 0 {
		t.Fatalf("there is nobody left to speak, unread=%d", n)
	}
}

// A relay actor speaks for itself: with its row gone it has no standing left,
// so the move is refused and a state change never exists without the mail it
// implies.
func TestTransitionEyesTaskNeedsTheActorsRow(t *testing.T) {
	s := open(t)
	requester, _, task, _ := liveTask(t, s)
	if _, err := s.db.Exec(`DELETE FROM agents WHERE session_id='broker-1'`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "accepted", "{}"); ok || !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("a purged launcher must not settle a move: ok=%v (%v)", ok, err)
	}
	if got, _ := s.EyesTask(task.TaskID); got.State != "queued" {
		t.Fatalf("the task must be untouched: %+v", got)
	}
	if n, _ := s.UnreadCount("/r", requester.Name); n != 0 {
		t.Fatalf("a refused move must send nothing, unread=%d", n)
	}
}

// The bound the requester asked for is the one the sweep enforces, so the
// deadline is stored on the task, and Housekeep is what runs the sweep.
func TestExpireEyesTasksUsesTheStoredDeadline(t *testing.T) {
	s := open(t)
	now := time.Unix(7100000, 0)
	s.Now = func() time.Time { return now }
	name, _ := s.Register("/r", "s-a", "hook")
	registerBroker(t, s, "host:BOX", "broker-1")
	ref := protocol.AgentRef{Name: name, AgentID: agentID("s-a"), Scope: "/r"}
	task, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Brief: "b", DeadlineS: 9999})
	if err != nil {
		t.Fatal(err)
	}
	if d := count(t, s, `SELECT deadline_s FROM eyes_tasks WHERE task_id=?`, task.TaskID); d != 1800 {
		t.Fatalf("the ledger must record the capped deadline, got %d", d)
	}
	now = now.Add(1799 * time.Second)
	if n, err := s.ExpireEyesTasks(now); err != nil || n != 0 {
		t.Fatalf("inside the deadline: %d (%v)", n, err)
	}
	now = now.Add(2 * time.Second)
	if err := s.Housekeep(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.EyesTask(task.TaskID); err != nil || got.State != "failed" {
		t.Fatalf("Housekeep must fail a task past its deadline: %+v (%v)", got, err)
	}
	if n, _ := s.UnreadCount("/r", name); n != 1 {
		t.Fatalf("the requester must hear about the deadline, unread=%d", n)
	}
}

// A reported task is settled: the requester cannot cancel it afterwards.
func TestCancelEyesTaskAfterDoneIsRefused(t *testing.T) {
	s := open(t)
	requester, _, task, _ := liveTask(t, s)
	if _, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "accepted", "{}"); err != nil || !ok {
		t.Fatalf("accept: ok=%v (%v)", ok, err)
	}
	if _, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("/r", "eyes-"+task.TaskID), "done", "{}"); err != nil || !ok {
		t.Fatalf("done: ok=%v (%v)", ok, err)
	}
	if _, err := s.CancelEyesTask(task.TaskID, localActor(requester)); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("a done task must not be cancellable, got %v", err)
	}
}

// A requester is a workspace AND a session: an agent id is derived from the
// session id alone, so the same session in another workspace is a stranger to
// this task, not its owner.
func TestCancelEyesTaskIsBoundToTheRequestersScope(t *testing.T) {
	s := open(t)
	requester, _, task, _ := liveTask(t, s)
	elsewhere := protocol.AgentRef{Name: requester.Name, AgentID: requester.AgentID, Scope: "/other"}
	if _, err := s.CancelEyesTask(task.TaskID, localActor(elsewhere)); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("another workspace must not cancel, got %v", err)
	}
	if got, _ := s.EyesTask(task.TaskID); got.State != "queued" {
		t.Fatalf("a refused cancel must leave the task alone: %+v", got)
	}
}

// A task that settles - or disappears - between the sweep's snapshot and its
// move leaves nothing to expire, so the sweep skips it instead of counting it
// or reporting it as a failure. The clock hook is the race: it fires once,
// just before the sweep opens its transaction.
func TestExpireEyesTasksToleratesAConcurrentSettle(t *testing.T) {
	for _, c := range []struct {
		what, wantState string
		race            func(t *testing.T, s *Store, taskID string)
	}{
		{"a report that lands first", "done", func(t *testing.T, s *Store, taskID string) {
			if _, ok, err := s.TransitionEyesTask(taskID, relayActor("/r", "eyes-"+taskID), "done",
				`{"type":"task.result","task_id":"`+taskID+`"}`); err != nil || !ok {
				t.Errorf("the racing report: ok=%v (%v)", ok, err)
			}
		}},
		{"a purge that gets there first", "", func(t *testing.T, s *Store, taskID string) {
			if _, err := s.db.Exec(`DELETE FROM eyes_tasks WHERE task_id=?`, taskID); err != nil {
				t.Error(err)
			}
		}},
	} {
		s := open(t)
		now := time.Unix(7600000, 0)
		s.Now = func() time.Time { return now }
		_, _, task, _ := liveTask(t, s)
		if _, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "accepted", "{}"); err != nil || !ok {
			t.Fatalf("accept: ok=%v (%v)", ok, err)
		}
		if _, err := s.db.Exec(`UPDATE eyes_tasks SET created_at=? WHERE task_id=?`,
			now.Unix()-301, task.TaskID); err != nil {
			t.Fatal(err)
		}
		racing := true
		s.Now = func() time.Time {
			if racing {
				racing = false
				c.race(t, s, task.TaskID)
			}
			return now
		}
		if n, err := s.ExpireEyesTasks(now); err != nil || n != 0 {
			t.Fatalf("%s: the sweep must skip it, not report it: %d (%v)", c.what, n, err)
		}
		got, err := s.EyesTask(task.TaskID)
		if c.wantState == "" {
			if !errors.Is(err, ErrUnknownTask) {
				t.Fatalf("%s: the task is gone: %+v (%v)", c.what, got, err)
			}
			continue
		}
		if err != nil || got.State != c.wantState {
			t.Fatalf("%s: the race wins: %+v (%v)", c.what, got, err)
		}
	}
}

// A local row may share a broker's session id. Resolving the broker's queue
// to that row would silently empty it: the mail is addressed to the broker's
// workspace, not the squatter's.
func TestPendingTaskMailIgnoresALocalRowWithTheSameSessionID(t *testing.T) {
	s := open(t)
	now := time.Unix(9200000, 0)
	s.Now = func() time.Time { return now }
	name, _ := s.Register("/r", "s-a", "hook")
	registerBroker(t, s, "host:BOX", "broker-1")
	// Registered in the same second, in a scope that sorts ahead of host:BOX.
	if _, err := s.Register("/r", "broker-1", "hook"); err != nil {
		t.Fatal(err)
	}
	requester := protocol.AgentRef{Name: name, AgentID: agentID("s-a"), Scope: "/r"}
	task, _, err := s.AssignEyesTask(EyesRequest{Requester: requester, Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingLaunches("broker-1")
	if err != nil || len(pending) != 1 || pending[0].TaskID != task.TaskID {
		t.Fatalf("the broker's own launch queue must answer: %+v (%v)", pending, err)
	}
	if _, err := s.CancelEyesTask(task.TaskID, localActor(requester)); err != nil {
		t.Fatal(err)
	}
	cancels, err := s.PendingCancels("broker-1")
	if err != nil || len(cancels) != 1 || cancels[0].TaskID != task.TaskID {
		t.Fatalf("and so must its cancel queue: %+v (%v)", cancels, err)
	}
}

// A cancel is durable only once the broker answers it: until then the notice
// is redelivered, and the terminal report the broker sends after killing the
// provider is what ends that - not a refusal it would retry forever.
func TestCancelAckStopsRedelivery(t *testing.T) {
	s := open(t)
	requester, _, task, _ := liveTask(t, s)
	if _, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "accepted", "{}"); err != nil || !ok {
		t.Fatalf("accept: ok=%v (%v)", ok, err)
	}
	if _, err := s.CancelEyesTask(task.TaskID, localActor(requester)); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingCancels("broker-1")
	if err != nil || len(pending) != 1 || pending[0].TaskID != task.TaskID {
		t.Fatalf("an unanswered cancel is redelivered: %+v (%v)", pending, err)
	}
	var body protocol.TaskCancelMsg
	if err := json.Unmarshal([]byte(pending[0].Body), &body); err != nil || body.Type != protocol.TaskCancel {
		t.Fatalf("cancel body %q: %v", pending[0].Body, err)
	}
	if only, err := s.PendingLaunches("broker-1"); err != nil || len(only) != 0 {
		t.Fatalf("a cancelled task is no longer work to run: %+v (%v)", only, err)
	}
	before := count(t, s, `SELECT COUNT(*) FROM messages WHERE task_id=?`, task.TaskID)

	child := "eyes-" + task.TaskID
	got, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("/r", child), "failed",
		`{"type":"task.failed","task_id":"`+task.TaskID+`","error":"cancelled"}`)
	if err != nil || ok || got.State != "cancelled" {
		t.Fatalf("the child's report acks the cancel: %+v ok=%v (%v)", got, ok, err)
	}
	if n := count(t, s, `SELECT cancel_acked FROM eyes_tasks WHERE task_id=?`, task.TaskID); n != 1 {
		t.Fatalf("the ack must be recorded, cancel_acked=%d", n)
	}
	if left, err := s.PendingCancels("broker-1"); err != nil || len(left) != 0 {
		t.Fatalf("an acked cancel leaves the queue: %+v (%v)", left, err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM messages WHERE task_id=?`, task.TaskID); n != before {
		t.Fatalf("an ack is not news for the requester: %d task messages, was %d", n, before)
	}
	// A broker that lost the response says it again, on either terminal body.
	if again, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("/r", child), "done", "{}"); err != nil || ok || again.State != "cancelled" {
		t.Fatalf("a repeated ack stays ok: %+v ok=%v (%v)", again, ok, err)
	}
}

// Who answers a cancel is who could have been running it: the child when the
// task has one, the broker itself when it never got that far. Anyone else is
// refused exactly as before, so a stranger cannot retire somebody's task.
func TestCancelAckSpeaker(t *testing.T) {
	s := open(t)
	name, _ := s.Register("/r", "s-a", "hook")
	registerBroker(t, s, "host:BOX", "broker-1")
	requester := protocol.AgentRef{Name: name, AgentID: agentID("s-a"), Scope: "/r"}
	task, _, err := s.AssignEyesTask(EyesRequest{Requester: requester, Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelEyesTask(task.TaskID, localActor(requester)); err != nil {
		t.Fatal(err)
	}
	// No child was ever minted, so the broker's own failure is the answer.
	got, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "failed",
		`{"type":"task.failed","task_id":"`+task.TaskID+`","error":"cancelled"}`)
	if err != nil || ok || got.State != "cancelled" {
		t.Fatalf("the launcher acks a childless cancel: %+v ok=%v (%v)", got, ok, err)
	}
	if n := count(t, s, `SELECT cancel_acked FROM eyes_tasks WHERE task_id=?`, task.TaskID); n != 1 {
		t.Fatalf("the ack must be recorded, cancel_acked=%d", n)
	}

	s2 := open(t)
	requester2, _, task2, _ := liveTask(t, s2)
	registerBroker(t, s2, "host:BOX", "broker-2")
	if _, err := s2.CancelEyesTask(task2.TaskID, localActor(requester2)); err != nil {
		t.Fatal(err)
	}
	// This task has a child, so the child is the only thing that can answer.
	if _, _, err := s2.TransitionEyesTask(task2.TaskID, relayActor("host:BOX", "broker-1"), "failed", "{}"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("the launcher must not speak over its child, got %v", err)
	}
	if _, _, err := s2.TransitionEyesTask(task2.TaskID, relayActor("host:BOX", "broker-2"), "failed", "{}"); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("another broker has no standing, got %v", err)
	}
	if n := count(t, s2, `SELECT cancel_acked FROM eyes_tasks WHERE task_id=?`, task2.TaskID); n != 0 {
		t.Fatalf("a refused report acks nothing, cancel_acked=%d", n)
	}
	if left, err := s2.PendingCancels("broker-1"); err != nil || len(left) != 1 {
		t.Fatalf("the cancel is still owed an answer: %+v (%v)", left, err)
	}
}

// A launcher and its child exist only over the relay, so a local session that
// registered itself under one of their ids speaks for nothing.
func TestTransitionEyesTaskRefusesALocalActor(t *testing.T) {
	s := open(t)
	name, _ := s.Register("/r", "s-a", "hook")
	registerBroker(t, s, "host:BOX", "broker-1")
	requester := protocol.AgentRef{Name: name, AgentID: agentID("s-a"), Scope: "/r"}
	task, _, err := s.AssignEyesTask(EyesRequest{Requester: requester, Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "accepted", "{}"); err != nil || !ok {
		t.Fatalf("accept: ok=%v (%v)", ok, err)
	}
	// A hook session in the requester's own workspace, wearing the child's id.
	child := "eyes-" + task.TaskID
	if _, err := s.Register("/r", child, "hook"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.TransitionEyesTask(task.TaskID, relayActor("/r", child), "done",
		`{"type":"task.result","task_id":"`+task.TaskID+`","status":"ok"}`); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("a local session must not report for the task, got %v", err)
	}
	if got, _ := s.EyesTask(task.TaskID); got.State != "accepted" {
		t.Fatalf("a refused report must leave the task alone: %+v", got)
	}
	// Nor does a relay row of some other kind speak for the task.
	s2 := open(t)
	_, _, task2, _ := liveTask(t, s2)
	if _, err := s2.db.Exec(`UPDATE agents SET kind='' WHERE session_id=?`, "eyes-"+task2.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.TransitionEyesTask(task2.TaskID, relayActor("/r", "eyes-"+task2.TaskID), "failed", "{}"); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("a relay row that is neither launcher nor eyes must not report, got %v", err)
	}
}

// A cancelled task the broker never answered still reserves it, so the
// deadline has to be the release: the sweep acks the cancel, changes no state
// and tells nobody, because the requester is the one who asked for it.
func TestExpireEyesTasksReleasesAnUnackedCancel(t *testing.T) {
	s := open(t)
	now := time.Unix(7500000, 0)
	s.Now = func() time.Time { return now }
	requester, _, task, _ := liveTask(t, s)
	if _, err := s.CancelEyesTask(task.TaskID, localActor(requester)); err != nil {
		t.Fatal(err)
	}
	// The broker died holding the job: its child row is gone, so nothing will
	// ever report and the cancel would be redelivered forever.
	if _, err := s.db.Exec(`DELETE FROM agents WHERE session_id=?`, "eyes-"+task.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PickLauncher(); !errors.Is(err, ErrEyesBusy) {
		t.Fatalf("an unacked cancel still reserves its broker, got %v", err)
	}
	if _, err := s.db.Exec(`UPDATE eyes_tasks SET created_at=? WHERE task_id=?`,
		now.Unix()-301, task.TaskID); err != nil {
		t.Fatal(err)
	}
	before := count(t, s, `SELECT COUNT(*) FROM messages WHERE task_id=?`, task.TaskID)

	n, err := s.ExpireEyesTasks(now)
	if err != nil || n != 1 {
		t.Fatalf("the deadline must settle an unacked cancel: %d (%v)", n, err)
	}
	got, err := s.EyesTask(task.TaskID)
	if err != nil || got.State != "cancelled" || !got.CancelAcked {
		t.Fatalf("expiry acks the cancel and moves no state: %+v (%v)", got, err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM messages WHERE task_id=?`, task.TaskID); n != before {
		t.Fatalf("the requester asked for this, it is not news: %d task messages, was %d", n, before)
	}
	if left, err := s.PendingCancels("broker-1"); err != nil || len(left) != 0 {
		t.Fatalf("a settled cancel leaves the queue: %+v (%v)", left, err)
	}
	if l, err := s.PickLauncher(); err != nil || l.SessionID != "broker-1" {
		t.Fatalf("the broker must be free again: %+v (%v)", l, err)
	}
	if n, err := s.ExpireEyesTasks(now); err != nil || n != 0 {
		t.Fatalf("a settled cancel expires once: %d (%v)", n, err)
	}
}

// A broker that restarted lost the child secret it was holding, so a cancel
// it has not answered must still mint one: that child is what sends the
// terminal report acking the cancel. A settled task mints nothing.
func TestReissueEyesChildForAnUnackedCancel(t *testing.T) {
	s := open(t)
	requester, broker, task, _ := liveTask(t, s)
	if _, err := s.CancelEyesTask(task.TaskID, localActor(requester)); err != nil {
		t.Fatal(err)
	}
	child, err := s.ReissueEyesChild(task.TaskID, "broker-1", broker.Secret)
	if err != nil || child.Secret == "" {
		t.Fatalf("an unanswered cancel still gains a child: %+v (%v)", child, err)
	}
	if _, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("/r", "eyes-"+task.TaskID), "failed",
		`{"type":"task.failed","task_id":"`+task.TaskID+`","error":"cancelled"}`); err != nil || ok {
		t.Fatalf("that child's report acks the cancel: ok=%v (%v)", ok, err)
	}
	if _, err := s.ReissueEyesChild(task.TaskID, "broker-1", broker.Secret); !errors.Is(err, ErrTaskNotLive) {
		t.Fatalf("an acked cancel is settled, got %v", err)
	}

	s2 := open(t)
	_, broker2, task2, _ := liveTask(t, s2)
	if _, ok, err := s2.TransitionEyesTask(task2.TaskID, relayActor("/r", "eyes-"+task2.TaskID), "failed", "{}"); err != nil || !ok {
		t.Fatalf("fail: ok=%v (%v)", ok, err)
	}
	if _, err := s2.ReissueEyesChild(task2.TaskID, "broker-1", broker2.Secret); !errors.Is(err, ErrTaskNotLive) {
		t.Fatalf("a failed task mints nothing, got %v", err)
	}
}

// The requester's row is purged hours before its task, and an expiry speaks
// for nobody: a deadline with no one left to tell still settles the task and
// still hands the broker back.
func TestExpireEyesTasksWithNoRequesterLeft(t *testing.T) {
	s := open(t)
	now := time.Unix(7600000, 0)
	s.Now = func() time.Time { return now }
	requester, _, task, _ := liveTask(t, s)
	if _, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "accepted", "{}"); err != nil || !ok {
		t.Fatalf("accept: ok=%v (%v)", ok, err)
	}
	if _, err := s.db.Exec(`DELETE FROM agents WHERE scope='/r' AND agent_id=?`, requester.AgentID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE eyes_tasks SET created_at=? WHERE task_id=?`,
		now.Unix()-301, task.TaskID); err != nil {
		t.Fatal(err)
	}
	before := count(t, s, `SELECT COUNT(*) FROM messages WHERE task_id=?`, task.TaskID)

	n, err := s.ExpireEyesTasks(now)
	if err != nil || n != 1 {
		t.Fatalf("a purged requester must not block the deadline: %d (%v)", n, err)
	}
	if got, _ := s.EyesTask(task.TaskID); got.State != "failed" {
		t.Fatalf("expired task: %+v", got)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM messages WHERE task_id=?`, task.TaskID); n != before {
		t.Fatalf("there is nobody left to tell: %d task messages, was %d", n, before)
	}
	if l, err := s.PickLauncher(); err != nil || l.SessionID != "broker-1" {
		t.Fatalf("the broker must be free again: %+v (%v)", l, err)
	}
}

// The pick and the default provider must agree on what a provider is: a
// broker advertising a runtime this store cannot name loses an unqualified
// brief to one that runs claude, however recently it polled.
func TestPickLauncherMatchesOnlyKnownProviders(t *testing.T) {
	s := open(t)
	now := time.Unix(7700000, 0)
	s.Now = func() time.Time { return now }
	registerBroker(t, s, "host:BOX", "broker-1")
	now = now.Add(5 * time.Second) // the odd broker polled most recently
	if _, err := s.RegisterRelay(RelayRegistration{Scope: "host:BOX", SessionID: "broker-foo",
		Kind: protocol.KindLauncher, Origin: "relay", Platform: "windows",
		Secret:       brokerSecret("broker-foo"),
		Capabilities: []string{"browser.chrome", "provider.foo"}}); err != nil {
		t.Fatal(err)
	}
	l, err := s.PickLauncher()
	if err != nil || l.SessionID != "broker-1" {
		t.Fatalf("an unknown provider must not win the pick: %+v (%v)", l, err)
	}
	if p := defaultProvider(l.caps); p != "claude" {
		t.Fatalf("the pick must be a provider the store can name, got %q", p)
	}
}

// The redelivery queue matches the body's type wherever it sits, so
// reordering the launch struct's fields cannot silently empty it.
func TestPendingTaskMailMatchesAnyFieldOrder(t *testing.T) {
	s := open(t)
	name, _ := s.Register("/r", "s-a", "hook")
	registerBroker(t, s, "host:BOX", "broker-1")
	requester := protocol.AgentRef{Name: name, AgentID: agentID("s-a"), Scope: "/r"}
	task, _, err := s.AssignEyesTask(EyesRequest{Requester: requester, Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE messages SET body=? WHERE task_id=?`,
		`{"task_id":"`+task.TaskID+`","type":"task.launch"}`, task.TaskID); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingLaunches("broker-1")
	if err != nil || len(pending) != 1 || pending[0].TaskID != task.TaskID {
		t.Fatalf("the launch is still owed whatever order it was written in: %+v (%v)", pending, err)
	}
}

// relayActor is how the gate presents an authenticated relay caller: the row
// it proved, plus the provenance the daemon stamps on a TCP request.
func relayActor(scope, session string) EyesActor {
	return EyesActor{Scope: scope, SessionID: session, Origin: RelayOrigin}
}

// localActor is a caller off the unix socket: the ref its own workspace knows
// it by, and no provenance at all.
func localActor(ref protocol.AgentRef) EyesActor {
	return EyesActor{Scope: ref.Scope, Ref: ref}
}

// The launch and the cancel belong to the ledger, not to an inbox: a broker
// must never read a stale launch as ordinary mail, or it would start a
// provider the requester already called off. The rows are still consumed, so
// only what is still owed comes back - through the queue.
func TestLedgerBodiesAreNotABrokersOrdinaryMail(t *testing.T) {
	s := open(t)
	requester, broker, task, _ := liveTask(t, s)
	if _, err := s.CancelEyesTask(task.TaskID, localActor(requester)); err != nil {
		t.Fatal(err)
	}
	// The rows are real mail - they are what the ledger redelivers - but a
	// broker's own poll never sees them raw.
	if info, err := s.PeekMail("host:BOX", broker.Name, 0); err != nil || info.Unread != 2 {
		t.Fatalf("the launch and the cancel are still delivered rows: %+v (%v)", info, err)
	}
	info, err := s.PeekBrokerMail("host:BOX", broker.Name, 0)
	if err != nil || info.Unread != 0 {
		t.Fatalf("neither body is unread mail for a broker: %+v (%v)", info, err)
	}
	msgs, err := s.ReadBroker("host:BOX", broker.Name)
	if err != nil || len(msgs) != 0 {
		t.Fatalf("neither body is ordinary mail for a broker: %+v (%v)", msgs, err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM deliveries WHERE read_at IS NULL`); n != 0 {
		t.Fatalf("a read still consumes them: %d unread deliveries left", n)
	}
	cancels, err := s.PendingCancels("broker-1")
	if err != nil || len(cancels) != 1 {
		t.Fatalf("the ledger still owes the cancel: %+v (%v)", cancels, err)
	}
	if owed, err := s.PendingLaunches("broker-1"); err != nil || len(owed) != 0 {
		t.Fatalf("a cancelled task is no longer work to run: %+v (%v)", owed, err)
	}
}

// Only a relay row speaks for a task, and only for the side it registered as:
// a unix caller has no provenance at all, and a launcher wearing the child's
// session id is still a launcher.
func TestEyesRoleIsBoundToTheProvenRow(t *testing.T) {
	s := open(t)
	_, _, task, _ := liveTask(t, s)
	accepted := `{"type":"task.accepted","task_id":"` + task.TaskID + `"}`
	// The broker's own session id, presented by a caller off the unix socket.
	local := EyesActor{Scope: "host:BOX", SessionID: "broker-1"}
	if _, _, err := s.TransitionEyesTask(task.TaskID, local, "accepted", accepted); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("a caller with no relay provenance speaks for nothing, got %v", err)
	}
	// The right session id in the wrong workspace is somebody else's row.
	if _, _, err := s.TransitionEyesTask(task.TaskID, relayActor("/r", "broker-1"), "accepted",
		accepted); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("the row is looked up where the caller proved it, got %v", err)
	}
	if _, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "accepted",
		accepted); err != nil || !ok {
		t.Fatalf("the broker's own row still accepts: ok=%v (%v)", ok, err)
	}
	// A launcher row registered under the child's session id is not the child.
	s2 := open(t)
	_, _, task2, _ := liveTask(t, s2)
	child := "eyes-" + task2.TaskID
	if _, err := s2.db.Exec(`UPDATE agents SET kind=? WHERE session_id=?`, protocol.KindLauncher, child); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.TransitionEyesTask(task2.TaskID, relayActor("/r", child), "failed",
		`{"type":"task.failed","task_id":"`+task2.TaskID+`"}`); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("a launcher row must not report as the child, got %v", err)
	}
}

// A broker that crashed holding a live child restarts with no secret for it.
// The cancel that arrives meanwhile is still answerable: the same launcher
// re-mints the child, and that child's report is the ack.
func TestReissueEyesChildAfterACrashAndCancel(t *testing.T) {
	s := open(t)
	requester, broker, task, first := liveTask(t, s)
	if _, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "accepted", "{}"); err != nil || !ok {
		t.Fatalf("accept: ok=%v (%v)", ok, err)
	}
	if _, err := s.CancelEyesTask(task.TaskID, localActor(requester)); err != nil {
		t.Fatal(err)
	}
	registerBroker(t, s, "host:BOX", "broker-2")
	if _, err := s.ReissueEyesChild(task.TaskID, "broker-2", brokerSecret("broker-2")); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("only the assigned launcher may mint the child, got %v", err)
	}
	again, err := s.ReissueEyesChild(task.TaskID, "broker-1", broker.Secret)
	if err != nil || again.Secret == "" || again.Secret == first.Secret {
		t.Fatalf("the restarted broker gets a fresh child secret: %+v (%v)", again, err)
	}
	before := count(t, s, `SELECT COUNT(*) FROM messages WHERE task_id=?`, task.TaskID)
	if _, ok, err := s.TransitionEyesTask(task.TaskID, relayActor("/r", "eyes-"+task.TaskID), "failed",
		`{"type":"task.failed","task_id":"`+task.TaskID+`","error":"cancelled"}`); err != nil || ok {
		t.Fatalf("the new child acks the cancel: ok=%v (%v)", ok, err)
	}
	got, err := s.EyesTask(task.TaskID)
	if err != nil || got.State != "cancelled" || !got.CancelAcked {
		t.Fatalf("an acked cancel keeps its state: %+v (%v)", got, err)
	}
	if left, err := s.PendingCancels("broker-1"); err != nil || len(left) != 0 {
		t.Fatalf("the queue is empty: %+v (%v)", left, err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM messages WHERE task_id=?`, task.TaskID); n != before {
		t.Fatalf("the ack is not news for the requester: %d task messages, was %d", n, before)
	}
}

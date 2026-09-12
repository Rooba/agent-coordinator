package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

// liveProgressTask sets up one accepted task with its child minted, which is
// the only shape a running task ever has.
func liveProgressTask(t *testing.T, s *Store) (EyesTask, protocol.AgentRef) {
	t.Helper()
	requester, _ := s.Register("/r", "s-a", "hook")
	broker := registerBroker(t, s, "host:BOX", "broker-1")
	ref := protocol.AgentRef{Name: requester, AgentID: agentID("s-a"), Scope: "/r"}
	task, _, err := s.AssignEyesTask(EyesRequest{Requester: ref, Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReissueEyesChild(task.TaskID, "broker-1", broker.Secret); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.TransitionEyesTask(task.TaskID, relayActor("host:BOX", "broker-1"), "accepted",
		`{"type":"task.accepted","task_id":"`+task.TaskID+`"}`); err != nil {
		t.Fatal(err)
	}
	return task, ref
}

// progressMail counts the requester's task.progress copies and returns their
// decoded bodies.
func progressMail(t *testing.T, s *Store, taskID string) []protocol.TaskProgressMsg {
	t.Helper()
	rows, err := s.db.Query(`SELECT body FROM messages WHERE task_id=? AND scope='/r' ORDER BY id`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []protocol.TaskProgressMsg
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		var msg protocol.TaskProgressMsg
		if json.Unmarshal([]byte(body), &msg) == nil && msg.Type == protocol.TaskProgress {
			out = append(out, msg)
		}
	}
	return out
}

func taskProgressRow(t *testing.T, s *Store, taskID string) (EyesProgress, int64) {
	t.Helper()
	var raw string
	var heartbeat int64
	if err := s.db.QueryRow(`SELECT progress, heartbeat_at FROM eyes_tasks WHERE task_id=?`,
		taskID).Scan(&raw, &heartbeat); err != nil {
		t.Fatal(err)
	}
	return decodeEyesProgress(raw), heartbeat
}

// The row moves on every sample; the requester's inbox does not. Ten minutes
// of ten-second samples is one mail every two minutes, not sixty.
func TestRecordEyesProgressUpdatesRowAndThrottlesMail(t *testing.T) {
	s := open(t)
	now := time.Unix(1000000, 0)
	s.Now = func() time.Time { return now }
	task, _ := liveProgressTask(t, s)
	child := relayActor("/r", "eyes-"+task.TaskID)

	const samples = 60
	for turn := 1; turn <= samples; turn++ {
		if _, _, err := s.RecordEyesProgress(task.TaskID, child,
			EyesProgress{Turns: turn, Tool: "mcp__claude-in-chrome__navigate"}); err != nil {
			t.Fatalf("sample %d: %v", turn, err)
		}
		stored, heartbeat := taskProgressRow(t, s, task.TaskID)
		if stored.Turns != turn || heartbeat != now.Unix() {
			t.Fatalf("sample %d stored %+v heartbeat %d", turn, stored, heartbeat)
		}
		now = now.Add(10 * time.Second)
	}
	mail := progressMail(t, s, task.TaskID)
	if len(mail) != 4 {
		t.Fatalf("%d samples produced %d progress messages, want one per %ds window",
			samples, len(mail), eyesProgressMailInterval)
	}
	for _, msg := range mail {
		if msg.TaskID != task.TaskID || msg.Turns == 0 || msg.Tool != "mcp__claude-in-chrome__navigate" || msg.ElapsedS <= 0 {
			t.Fatalf("progress mail: %+v", msg)
		}
	}
	// The state never moved: progress is liveness, not a lifecycle transition.
	if after, err := s.EyesTask(task.TaskID); err != nil || after.State != "accepted" {
		t.Fatalf("state = %+v (%v), want it untouched", after, err)
	}
}

// A stalled task mails nothing new: the throttle only opens for a sample that
// actually advanced, so silence in the inbox means silence from the provider.
func TestRecordEyesProgressMailsOnlyOnAdvance(t *testing.T) {
	s := open(t)
	now := time.Unix(1000000, 0)
	s.Now = func() time.Time { return now }
	task, _ := liveProgressTask(t, s)
	child := relayActor("/r", "eyes-"+task.TaskID)
	for i := 0; i < 20; i++ {
		if _, _, err := s.RecordEyesProgress(task.TaskID, child, EyesProgress{Turns: 1, Tool: "navigate"}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	if mail := progressMail(t, s, task.TaskID); len(mail) != 0 {
		t.Fatalf("a task stuck on one turn mailed %d times", len(mail))
	}
}

// Nothing a provider wrote reaches the row or an inbox: a tool name that is
// not a name is dropped whole, and a name longer than the cap is truncated.
func TestRecordEyesProgressBoundsStoredContent(t *testing.T) {
	s := open(t)
	now := time.Unix(1000000, 0)
	s.Now = func() time.Time { return now }
	task, _ := liveProgressTask(t, s)
	child := relayActor("/r", "eyes-"+task.TaskID)

	long := strings.Repeat("n", maxEyesToolBytes+40)
	stored, _, err := s.RecordEyesProgress(task.TaskID, child, EyesProgress{Turns: 1, Tool: long})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Tool != strings.Repeat("n", maxEyesToolBytes) {
		t.Fatalf("oversized tool name was not truncated: %q", stored.Tool)
	}
	// Arguments, URLs and page text all carry characters a name does not.
	now = now.Add(10 * time.Second)
	stored, _, err = s.RecordEyesProgress(task.TaskID, child,
		EyesProgress{Turns: 2, Tool: `navigate {"url":"https://SECRET.example"}`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.Tool, "SECRET") {
		t.Fatalf("tool arguments reached the row: %q", stored.Tool)
	}
	if stored.Turns != 2 {
		t.Fatalf("turns = %d, want the sample counted", stored.Turns)
	}
	// A counter a broker inflates is clamped rather than believed.
	now = now.Add(10 * time.Second)
	if stored, _, err = s.RecordEyesProgress(task.TaskID, child, EyesProgress{Turns: maxEyesTurns * 10}); err != nil {
		t.Fatal(err)
	}
	if stored.Turns != maxEyesTurns {
		t.Fatalf("turns = %d, want the cap", stored.Turns)
	}
}

// The child agent's last_seen is the only "is this task alive?" signal there
// is, so every sample refreshes it.
func TestRecordEyesProgressRefreshesChildLastSeen(t *testing.T) {
	s := open(t)
	now := time.Unix(1000000, 0)
	s.Now = func() time.Time { return now }
	task, _ := liveProgressTask(t, s)
	child := relayActor("/r", "eyes-"+task.TaskID)
	lastSeen := func() int64 {
		var seen int64
		if err := s.db.QueryRow(`SELECT last_seen FROM agents WHERE scope='/r' AND session_id=?`,
			"eyes-"+task.TaskID).Scan(&seen); err != nil {
			t.Fatal(err)
		}
		return seen
	}
	registered := lastSeen()
	now = now.Add(5 * time.Minute)
	if _, _, err := s.RecordEyesProgress(task.TaskID, child, EyesProgress{Turns: 1, Tool: "navigate"}); err != nil {
		t.Fatal(err)
	}
	if seen := lastSeen(); seen != now.Unix() || seen == registered {
		t.Fatalf("child last_seen = %d, want the sample's clock %d (was %d)", seen, now.Unix(), registered)
	}
}

// Only the sides that run the job may report on it, and only while it runs.
func TestRecordEyesProgressAuthorizesAndRequiresALiveTask(t *testing.T) {
	s := open(t)
	now := time.Unix(1000000, 0)
	s.Now = func() time.Time { return now }
	task, ref := liveProgressTask(t, s)
	sample := EyesProgress{Turns: 1, Tool: "navigate"}

	if _, _, err := s.RecordEyesProgress("task-nope", relayActor("/r", "eyes-"+task.TaskID), sample); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("unknown task: %v", err)
	}
	// The requester owns the task but does not run it.
	if _, _, err := s.RecordEyesProgress(task.TaskID, EyesActor{Scope: "/r", Ref: ref}, sample); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("requester reporting progress: %v", err)
	}
	if _, _, err := s.RecordEyesProgress(task.TaskID, relayActor("/r", "eyes-task-000000000099"), sample); !errors.Is(err, ErrNotYourTask) {
		t.Fatalf("a stranger's child: %v", err)
	}
	report := `{"type":"task.result","task_id":"` + task.TaskID + `","status":"succeeded","summary":"s",` +
		`"observations":[],"actions":[],"evidence":[]}`
	if _, _, err := s.TransitionEyesTask(task.TaskID, relayActor("/r", "eyes-"+task.TaskID), "done", report); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordEyesProgress(task.TaskID, relayActor("/r", "eyes-"+task.TaskID), sample); !errors.Is(err, ErrTaskNotLive) {
		t.Fatalf("a settled task takes no samples: %v", err)
	}
}

// The listing is where a human looks, so the sample and its age land there.
func TestListEyesTasksShowsProgressAndHeartbeatAge(t *testing.T) {
	s := open(t)
	now := time.Unix(1000000, 0)
	s.Now = func() time.Time { return now }
	task, _ := liveProgressTask(t, s)
	if _, _, err := s.RecordEyesProgress(task.TaskID, relayActor("/r", "eyes-"+task.TaskID),
		EyesProgress{Turns: 7, Tool: "mcp__claude-in-chrome__click"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(90 * time.Second)
	views, err := s.ListEyesTasks(EyesTaskFilter{Scope: "/r"})
	if err != nil || len(views) != 1 {
		t.Fatalf("listing: %+v (%v)", views, err)
	}
	v := views[0]
	if v.Progress.Turns != 7 || v.Progress.Tool != "mcp__claude-in-chrome__click" || v.HeartbeatAgeS != 90 {
		t.Fatalf("view = %+v", v)
	}
}

// A finished task stops the clock. Elapsed measured to now would make a job
// that took two minutes yesterday read as a day old, and its remaining time
// as deeply negative.
func TestListEyesTasksFreezesElapsedOnceSettled(t *testing.T) {
	s := open(t)
	now := time.Unix(1000000, 0)
	s.Now = func() time.Time { return now }
	task, _ := liveProgressTask(t, s)
	now = now.Add(60 * time.Second)
	if _, _, err := s.RecordEyesProgress(task.TaskID, relayActor("/r", "eyes-"+task.TaskID),
		EyesProgress{Turns: 3, Tool: "mcp__claude-in-chrome__click"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(140 * time.Second)
	report := `{"type":"task.result","task_id":"` + task.TaskID + `","status":"succeeded","summary":"s",` +
		`"observations":[],"actions":[],"evidence":[]}`
	if _, _, err := s.TransitionEyesTask(task.TaskID, relayActor("/r", "eyes-"+task.TaskID), "done", report); err != nil {
		t.Fatal(err)
	}
	settled := onlyTaskView(t, s)
	if settled.State != "done" || settled.ElapsedS != 200 || settled.RemainingS != 100 ||
		settled.HeartbeatAgeS != 140 || settled.Overdue || settled.HeartbeatStale {
		t.Fatalf("at the moment it settled: %+v", settled)
	}
	now = now.Add(24 * time.Hour)
	later := onlyTaskView(t, s)
	if later.ElapsedS != settled.ElapsedS || later.RemainingS != settled.RemainingS ||
		later.HeartbeatAgeS != settled.HeartbeatAgeS {
		t.Fatalf("a day later the settled row moved: %+v", later)
	}
	if later.RemainingS < 0 || later.Overdue || later.HeartbeatStale {
		t.Fatalf("a settled row must not read as overdue or stale: %+v", later)
	}
}

// A stalled task deliberately sends no mail, so a quiet heartbeat is the only
// stall signal a reader gets - including for a task that never reported once.
func TestListEyesTasksMarksAQuietLiveTaskStale(t *testing.T) {
	s := open(t)
	now := time.Unix(1000000, 0)
	s.Now = func() time.Time { return now }
	task, _ := liveProgressTask(t, s)
	if _, _, err := s.RecordEyesProgress(task.TaskID, relayActor("/r", "eyes-"+task.TaskID),
		EyesProgress{Turns: 1, Tool: "Read"}); err != nil {
		t.Fatal(err)
	}
	if v := onlyTaskView(t, s); v.HeartbeatStale {
		t.Fatalf("a task that just reported is not stale: %+v", v)
	}
	now = now.Add(eyesHeartbeatStaleS * time.Second)
	v := onlyTaskView(t, s)
	if !v.HeartbeatStale || v.HeartbeatAgeS != eyesHeartbeatStaleS {
		t.Fatalf("a task quiet for the whole window is stale: %+v", v)
	}
	// A live task that never reported is measured from when it was created.
	seedTaskAged(t, s, "/r", "aid-b", "task-quiet", "queued", eyesHeartbeatStaleS, 1800)
	seedTaskAged(t, s, "/r", "aid-b", "task-fresh", "queued", 5, 1800)
	byID := map[string]EyesTaskView{}
	views, err := s.ListEyesTasks(EyesTaskFilter{Scope: "/r"})
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		byID[view.TaskID] = view
	}
	if quiet := byID["task-quiet"]; !quiet.HeartbeatStale || quiet.HeartbeatAt != 0 {
		t.Fatalf("a task that never reported is stale once the window passes: %+v", quiet)
	}
	if fresh := byID["task-fresh"]; fresh.HeartbeatStale {
		t.Fatalf("a task younger than the window is not stale: %+v", fresh)
	}
}

// onlyTaskView reads the one task the fixture seeded.
func onlyTaskView(t *testing.T, s *Store) EyesTaskView {
	t.Helper()
	views, err := s.ListEyesTasks(EyesTaskFilter{Scope: "/r"})
	if err != nil || len(views) != 1 {
		t.Fatalf("listing: %+v (%v)", views, err)
	}
	return views[0]
}

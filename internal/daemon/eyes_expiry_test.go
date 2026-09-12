package daemon

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/store"
)

func TestServeExpiresAbandonedEyesTaskBeforeHousekeeping(t *testing.T) {
	t.Setenv("AC_RELAY_LISTEN", "")
	st, err := store.Open(filepath.Join(t.TempDir(), "expiry.db"))
	if err != nil {
		t.Fatal(err)
	}
	var now atomic.Int64
	now.Store(time.Now().Unix())
	st.Now = func() time.Time { return time.Unix(now.Load(), 0) }
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Serve(l, st, 2*time.Second) }()
	t.Cleanup(func() {
		l.Close()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	// A response proves startup housekeeping has already finished. The task
	// is created afterward, so only the periodic sweep can expire it.
	if r := tcpRoundTrip(t, l.Addr().String(), protocol.Request{Op: protocol.OpAgents, Scope: "/r"}); !r.OK {
		t.Fatalf("start daemon: %+v", r)
	}
	name, err := st.Register("/r", "requester", "hook")
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Identity("/r", "requester")
	if err != nil {
		t.Fatal(err)
	}
	requester := protocol.AgentRef{Name: name, AgentID: id.AgentID, Scope: "/r"}
	secret := strings.Repeat("a", 64)
	if _, err := st.RegisterRelay(store.RelayRegistration{Scope: "host:BOX", SessionID: "broker",
		Kind: protocol.KindLauncher, Origin: "relay", Platform: "windows", Secret: secret,
		Capabilities: []string{"browser.chrome", "provider.claude"}}); err != nil {
		t.Fatal(err)
	}
	task, launcher, err := st.AssignEyesTask(store.EyesRequest{Requester: requester, Brief: "inspect the page", DeadlineS: 300})
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.ReissueEyesChild(task.TaskID, "broker", secret)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := json.Marshal(protocol.TaskAcceptedMsg{Type: protocol.TaskAccepted, TaskID: task.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.TransitionEyesTask(task.TaskID, store.EyesActor{
		Scope: launcher.Scope, SessionID: "broker", Origin: "relay", Ref: launcher,
	}, "accepted", string(accepted)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Read("/r", name); err != nil {
		t.Fatal(err)
	}
	// Advance only the store clock: the deadline has passed, and full
	// housekeeping would also purge these now-stale agent rows.
	now.Add(int64((3 * time.Hour) / time.Second))
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		got, err := st.EyesTask(task.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deadline sweep did not settle abandoned task: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := st.Identity("/r", "requester"); err != nil {
		t.Fatalf("deadline sweep must not run full housekeeping: %v", err)
	}
	msgs, err := st.Read("/r", name)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("requester failure mail: %+v (%v)", msgs, err)
	}
	var failure protocol.TaskFailedMsg
	if err := json.Unmarshal([]byte(msgs[0].Body), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Type != protocol.TaskFailed || failure.TaskID != task.TaskID || failure.Error != "deadline exceeded" ||
		msgs[0].From != child.Name || msgs[0].Kind != protocol.KindEyes || msgs[0].TaskID != task.TaskID {
		t.Fatalf("deadline failure mail changed: %+v body %+v", msgs[0], failure)
	}
	// Another watchdog tick must leave the settled task alone.
	time.Sleep(600 * time.Millisecond)
	if msgs, err := st.Read("/r", name); err != nil || len(msgs) != 0 {
		t.Fatalf("deadline failure delivered more than once: %+v (%v)", msgs, err)
	}
}

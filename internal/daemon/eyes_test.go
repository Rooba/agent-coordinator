package daemon

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/store"
)

// listTaskFixture is one requester, one broker and one queued task, with the
// clock under the test's control so a deadline can be walked past.
func listTaskFixture(t *testing.T) (*store.Store, func(time.Duration), protocol.AgentRef, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Unix(2000000, 0)
	st.Now = func() time.Time { return now }
	name, err := st.Register("/r", "requester", "hook")
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Identity("/r", "requester")
	if err != nil {
		t.Fatal(err)
	}
	requester := protocol.AgentRef{Name: name, AgentID: id.AgentID, Scope: "/r"}
	if _, err := st.RegisterRelay(store.RelayRegistration{Scope: "host:BOX", SessionID: "broker",
		Kind: protocol.KindLauncher, Origin: "relay", Platform: "windows",
		Secret:       strings.Repeat("a", 64),
		Capabilities: []string{"browser.chrome", "provider.claude"}}); err != nil {
		t.Fatal(err)
	}
	task, _, err := st.AssignEyesTask(store.EyesRequest{Requester: requester,
		Brief: "inspect the page", DeadlineS: 300})
	if err != nil {
		t.Fatal(err)
	}
	return st, func(d time.Duration) { now = now.Add(d) }, requester, task.TaskID
}

// The listing is what a waiting human reads, so the daemon hands over the
// elapsed and remaining numbers already measured - and marks the task whose
// deadline passed with nothing left running behind it.
func TestListEyesTasksReportsDeadlineProgress(t *testing.T) {
	st, advance, requester, taskID := listTaskFixture(t)
	advance(780 * time.Second)
	resp := listEyesTasks(st, protocol.Request{Op: protocol.OpListEyesTasks, Scope: "/r"},
		protocol.AgentRef{})
	if !resp.OK || len(resp.EyesTasks) != 1 {
		t.Fatalf("listing: %+v", resp)
	}
	got := resp.EyesTasks[0]
	if got.TaskID != taskID || got.State != "queued" || got.Runtime != "claude" {
		t.Fatalf("row identity: %+v", got)
	}
	if got.RequesterScope != "/r" || got.RequesterAgentID != requester.AgentID ||
		got.LauncherScope != "host:BOX" {
		t.Fatalf("row parties: %+v", got)
	}
	if got.DeadlineS != 300 || got.ElapsedS != 780 || got.RemainingS != -480 || !got.Overdue {
		t.Fatalf("deadline progress: %+v", got)
	}
}

// The workspace the daemon bound is the authorization boundary: any peer in
// it may read this bounded metadata, and "mine" is an opt-in narrowing to the
// tasks the caller could cancel.
func TestListEyesTasksVisibility(t *testing.T) {
	st, advance, requester, taskID := listTaskFixture(t)
	advance(30 * time.Second)
	stranger, err := st.Register("/r", "stranger", "hook")
	if err != nil {
		t.Fatal(err)
	}
	strangerID, err := st.Identity("/r", "stranger")
	if err != nil {
		t.Fatal(err)
	}
	strangerRef := protocol.AgentRef{Name: stranger, AgentID: strangerID.AgentID, Scope: "/r"}
	for _, c := range []struct {
		name, scope string
		actor       protocol.AgentRef
		mine        bool
		want        int
	}{
		{name: "own workspace, unidentified", scope: "/r", want: 1},
		{name: "other workspace", scope: "/elsewhere"},
		{name: "requester", scope: "/r", actor: requester, want: 1},
		{name: "another agent in the workspace", scope: "/r", actor: strangerRef, want: 1},
		{name: "requester asking for its own", scope: "/r", actor: requester, mine: true, want: 1},
		{name: "another agent asking for its own", scope: "/r", actor: strangerRef, mine: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp := listEyesTasks(st, protocol.Request{Op: protocol.OpListEyesTasks,
				Scope: c.scope, Mine: c.mine}, c.actor)
			if !resp.OK || len(resp.EyesTasks) != c.want {
				t.Fatalf("want %d rows, got %d (%+v)", c.want, len(resp.EyesTasks), resp)
			}
			if c.want == 1 && resp.EyesTasks[0].TaskID != taskID {
				t.Fatalf("listed the wrong task: %+v", resp.EyesTasks[0])
			}
		})
	}
}

// The listing has to survive the dispatch table, not just the handler: a
// direct call would still pass if the op stopped being routed at all. Over
// the wire it also has to hold the workspace boundary and honour "mine".
func TestListEyesTasksOverTheWire(t *testing.T) {
	sock, addr, tok := startRelayDaemon(t)
	owner := registerUnix(t, sock, "/r", "s-owner")
	registerUnix(t, sock, "/r", "s-peer")
	registerUnix(t, sock, "/elsewhere", "s-away")
	registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	req := roundTrip(t, sock, protocol.Request{Op: protocol.OpRequestEyes, Scope: "/r",
		SessionID: "s-owner", From: owner.Name, Brief: "does the login page render"})
	if !req.OK {
		t.Fatalf("request_eyes: %+v", req)
	}
	for _, c := range []struct {
		name, scope, session string
		mine                 bool
		want                 int
	}{
		{name: "requester", scope: "/r", session: "s-owner", want: 1},
		{name: "peer in the workspace", scope: "/r", session: "s-peer", want: 1},
		{name: "requester asking for its own", scope: "/r", session: "s-owner", mine: true, want: 1},
		{name: "peer asking for its own", scope: "/r", session: "s-peer", mine: true},
		{name: "another workspace", scope: "/elsewhere", session: "s-away"},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp := roundTrip(t, sock, protocol.Request{Op: protocol.OpListEyesTasks,
				Scope: c.scope, SessionID: c.session, Mine: c.mine})
			if !resp.OK {
				t.Fatalf("list_eyes_tasks: %+v", resp)
			}
			if len(resp.EyesTasks) != c.want {
				t.Fatalf("want %d rows, got %d (%+v)", c.want, len(resp.EyesTasks), resp.EyesTasks)
			}
			if c.want == 1 && resp.EyesTasks[0].TaskID != req.TaskID {
				t.Fatalf("listed the wrong task: %+v", resp.EyesTasks[0])
			}
		})
	}
}

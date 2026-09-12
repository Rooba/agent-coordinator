package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Rooba/agent-coordinator/internal/protocol"
)

// A running task reports progress the way it reports anything else, and the
// daemon writes it onto the row instead of delivering it as mail: the listing
// then shows motion while the task is still in flight, and the lifecycle state
// never moves.
func TestEyesProgressUpdatesTheTaskRowWithoutMovingIt(t *testing.T) {
	sock, addr, tok := startRelayDaemon(t)
	a := registerUnix(t, sock, "/r", "s-a")
	_, secret := registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	req := roundTrip(t, sock, protocol.Request{Op: protocol.OpRequestEyes, Scope: "/r",
		SessionID: "s-a", From: a.Name, Brief: "does the login page render"})
	if !req.OK {
		t.Fatalf("request_eyes: %+v", req)
	}
	child := "eyes-" + req.TaskID
	minted := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpRegister, Scope: "/r",
		SessionID: child, Kind: protocol.KindEyes, Token: tok, AuthSessionID: "broker-1", SessionSecret: secret})
	if !minted.OK {
		t.Fatalf("child register: %+v", minted)
	}
	accepted, err := json.Marshal(protocol.TaskAcceptedMsg{Type: protocol.TaskAccepted, TaskID: req.TaskID,
		Child: &protocol.AgentRef{Name: minted.Name, AgentID: minted.AgentID, Scope: "/r"}})
	if err != nil {
		t.Fatal(err)
	}
	if r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpSendWorkspace, SessionID: "broker-1",
		Token: tok, SessionSecret: secret, Body: string(accepted), Target: &a}); !r.OK {
		t.Fatalf("task.accepted: %+v", r)
	}

	sample := func(session, sessionSecret string, turns int, tool string) protocol.Response {
		body, err := json.Marshal(protocol.TaskProgressMsg{Type: protocol.TaskProgress, TaskID: req.TaskID,
			Turns: turns, Tool: tool, ElapsedS: turns * 7})
		if err != nil {
			t.Fatal(err)
		}
		return tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpSendWorkspace, SessionID: session,
			Token: tok, SessionSecret: sessionSecret, Body: string(body), Target: &a})
	}
	if r := sample(child, minted.SessionSecret, 4, "mcp__claude-in-chrome__navigate"); !r.OK || r.TaskID != req.TaskID {
		t.Fatalf("task.progress: %+v", r)
	}
	// Page text posing as a tool name is dropped rather than recorded.
	if r := sample(child, minted.SessionSecret, 5, `navigate url="https://SECRET.example"`); !r.OK {
		t.Fatalf("task.progress with a non-name tool: %+v", r)
	}

	tasks := roundTrip(t, sock, protocol.Request{Op: protocol.OpListEyesTasks, Scope: "/r", SessionID: "s-a"})
	if !tasks.OK || len(tasks.EyesTasks) != 1 {
		t.Fatalf("list_eyes_tasks: %+v", tasks)
	}
	view := tasks.EyesTasks[0]
	if view.State != "accepted" {
		t.Fatalf("progress moved the task to %q", view.State)
	}
	if view.Turns != 5 || view.Tool != "mcp__claude-in-chrome__navigate" || view.HeartbeatAt == 0 {
		t.Fatalf("task view = %+v", view)
	}
	if strings.Contains(view.Tool, "SECRET") {
		t.Fatalf("tool arguments reached the ledger: %q", view.Tool)
	}
	// The first samples only start the throttle's clock, so the requester's
	// inbox still holds just the accept.
	inbox := roundTrip(t, sock, protocol.Request{Op: protocol.OpRead, Scope: "/r", SessionID: "s-a", From: a.Name})
	for _, m := range inbox.Messages {
		if strings.Contains(m.Body, protocol.TaskProgress) {
			t.Fatalf("an early sample was mailed: %s", m.Body)
		}
	}
}

// A task.progress body from anyone but the task's own sides is refused, and a
// malformed one never lands as ordinary mail.
func TestEyesProgressIsRefusedFromStrangersAndWhenMalformed(t *testing.T) {
	sock, addr, tok := startRelayDaemon(t)
	a := registerUnix(t, sock, "/r", "s-a")
	_, secret := registerLauncher(t, addr, tok, "broker-1", "host:BOX")
	req := roundTrip(t, sock, protocol.Request{Op: protocol.OpRequestEyes, Scope: "/r",
		SessionID: "s-a", From: a.Name, Brief: "b"})
	if !req.OK {
		t.Fatalf("request_eyes: %+v", req)
	}
	body, err := json.Marshal(protocol.TaskProgressMsg{Type: protocol.TaskProgress, TaskID: req.TaskID, Turns: 1})
	if err != nil {
		t.Fatal(err)
	}
	// The requester owns the task but does not run it.
	if r := roundTrip(t, sock, protocol.Request{Op: protocol.OpSendWorkspace, Scope: "/r", SessionID: "s-a",
		From: a.Name, Body: string(body), Target: &a}); r.OK {
		t.Fatalf("the requester must not report its own progress: %+v", r)
	}
	// A task.* body that does not decode is reserved, never mail.
	if r := tcpRoundTrip(t, addr, protocol.Request{Op: protocol.OpSendWorkspace, SessionID: "broker-1",
		Token: tok, SessionSecret: secret, Target: &a,
		Body: `{"type":"task.progress","task_id":"` + req.TaskID + `","turns":-1}`}); r.OK ||
		r.Error != errReservedTask.Error() {
		t.Fatalf("malformed progress: %+v", r)
	}
}

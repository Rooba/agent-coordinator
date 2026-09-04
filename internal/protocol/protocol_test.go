package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	in := Request{Op: OpEvent, Scope: "/repo", SessionID: "s1", Tool: "Edit",
		Activity: "Editing main.go", Files: []string{"main.go"}, Writes: []string{"main.go"},
		TaskEv: &TaskEvent{Kind: "update", Key: "3", Status: "completed"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Request
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.TaskEv == nil || out.TaskEv.Key != "3" || out.Writes[0] != "main.go" {
		t.Fatalf("round trip lost data: %+v", out)
	}
}

func TestResponseOmitsEmpty(t *testing.T) {
	b, _ := json.Marshal(Response{OK: true})
	if string(b) != `{"ok":true}` {
		t.Fatalf("want minimal JSON, got %s", b)
	}
}

func TestRelayFieldsRoundTrip(t *testing.T) {
	in := Request{
		Op:            OpSendWorkspace,
		Scope:         "/home/ra/proj",
		From:          "eager-crane",
		Body:          "open https://localhost:3000 and report",
		Token:         "secret",
		SessionSecret: "deadbeef",
		Kind:          KindEyes,
		Runtime:       "grok",
		Brief:         "click Save",
		TaskID:        "t1",
		Target:        &AgentRef{Scope: "/home/ra/proj", AgentID: "98ffc675471a"},
		ReplyTo:       &AgentRef{Name: "eager-crane", AgentID: "98ffc675471a", Scope: "/home/ra/proj"},
		Platform:      "windows",
		Capabilities:  []string{"browser.chrome", "provider.claude"},
		DeadlineS:     600,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Request
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Op != OpSendWorkspace || out.Token != "secret" || out.SessionSecret != "deadbeef" ||
		out.Kind != KindEyes || out.Runtime != "grok" || out.Brief != "click Save" || out.TaskID != "t1" ||
		out.Platform != "windows" || out.DeadlineS != 600 || len(out.Capabilities) != 2 || out.Capabilities[0] != "browser.chrome" {
		t.Fatalf("scalar fields: %+v", out)
	}
	if out.Target == nil || out.Target.Scope != "/home/ra/proj" || out.Target.AgentID != "98ffc675471a" {
		t.Fatalf("target: %+v", out.Target)
	}
	if out.ReplyTo == nil || out.ReplyTo.Name != "eager-crane" {
		t.Fatalf("reply_to: %+v", out.ReplyTo)
	}
}

func TestTaskEnvelopeJSON(t *testing.T) {
	mustJSON := func(v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	launch := mustJSON(TaskLaunchMsg{Type: TaskLaunch, TaskID: "t1", Runtime: "claude", Scope: "/p", Brief: "b", ReplyTo: AgentRef{Name: "n", AgentID: "a", Scope: "/p"}, DeadlineS: 600})
	if !strings.Contains(launch, `"type":"task.launch"`) || !strings.Contains(launch, `"deadline_s":600`) || !strings.Contains(launch, `"reply_to"`) {
		t.Fatalf("launch: %s", launch)
	}
	cancel := mustJSON(TaskCancelMsg{Type: TaskCancel, TaskID: "t1"})
	if cancel != `{"type":"task.cancel","task_id":"t1"}` {
		t.Fatalf("cancel: %s", cancel)
	}
	accepted := mustJSON(TaskAcceptedMsg{Type: TaskAccepted, TaskID: "t1", Child: &AgentRef{Name: "eyes"}})
	if !strings.Contains(accepted, `"type":"task.accepted"`) || !strings.Contains(accepted, `"child"`) {
		t.Fatalf("accepted: %s", accepted)
	}
	failed := mustJSON(TaskFailedMsg{Type: TaskFailed, TaskID: "t1", Error: "boom"})
	if failed != `{"type":"task.failed","task_id":"t1","error":"boom"}` {
		t.Fatalf("failed: %s", failed)
	}
	result := mustJSON(TaskResultMsg{Type: TaskResult, TaskID: "t1", Status: "ok", Summary: "s", Observations: []string{}, Actions: []string{}, Evidence: []string{}})
	for _, k := range []string{`"summary"`, `"observations"`, `"actions"`, `"evidence"`, `"type":"task.result"`} {
		if !strings.Contains(result, k) {
			t.Fatalf("result missing %s: %s", k, result)
		}
	}
	if strings.Contains(result, `"summary,omitempty"`) {
		t.Fatal(result)
	}
}

func TestAuthSessionIDAndTaskEnvelopeRoundTrip(t *testing.T) {
	in := Request{Op: OpRegister, Kind: KindEyes, SessionID: "eyes-t1", AuthSessionID: "launcher-1", SessionSecret: "sec"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"auth_session_id":"launcher-1"`) {
		t.Fatalf("auth_session_id json: %s", b)
	}
	var out Request
	if err := json.Unmarshal(b, &out); err != nil || out.AuthSessionID != "launcher-1" || out.SessionID != "eyes-t1" {
		t.Fatalf("auth roundtrip: %+v %v", out, err)
	}
	launch := TaskLaunchMsg{Type: TaskLaunch, TaskID: "t1", Runtime: "claude", Scope: "/home/ra/p", Brief: "look", ReplyTo: AgentRef{Name: "a"}, DeadlineS: 600}
	b, err = json.Marshal(launch)
	if err != nil {
		t.Fatal(err)
	}
	var got TaskLaunchMsg
	if err := json.Unmarshal(b, &got); err != nil || got.Type != TaskLaunch || got.TaskID != "t1" || got.DeadlineS != 600 || got.ReplyTo.Name != "a" {
		t.Fatalf("launch envelope: %+v %v", got, err)
	}
}

func TestCapabilitiesJSONName(t *testing.T) {
	b, err := json.Marshal(Request{Capabilities: []string{"browser.chrome"}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"capabilities":["browser.chrome"]`) || strings.Contains(s, `"caps"`) {
		t.Fatalf("capabilities json: %s", s)
	}
	b, err = json.Marshal(AgentInfo{Capabilities: []string{"browser.chrome"}})
	if err != nil {
		t.Fatal(err)
	}
	s = string(b)
	if !strings.Contains(s, `"capabilities"`) || strings.Contains(s, `"caps"`) {
		t.Fatalf("AgentInfo capabilities json: %s", s)
	}
}

func TestAgentRefOmitsEmpty(t *testing.T) {
	b, err := json.Marshal(AgentRef{AgentID: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"agent_id":"abc"}` {
		t.Fatalf("got %s", b)
	}
}

func TestOldRequestStillUnmarshals(t *testing.T) {
	var out Request
	if err := json.Unmarshal([]byte(`{"op":"send","scope":"/repo","from":"a","to":"b","body":"hi"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Op != OpSend || out.ReplyTo != nil || out.Token != "" || out.Target != nil || out.Brief != "" ||
		out.Platform != "" || out.DeadlineS != 0 || out.Capabilities != nil {
		t.Fatalf("legacy request grew new fields: %+v", out)
	}
}

func TestMessageTaskIDRoundTrip(t *testing.T) {
	in := Message{ID: 7, From: "eyes", Body: "done", TaskID: "t1", Kind: KindEyes}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Message
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.TaskID != "t1" || out.Kind != KindEyes {
		t.Fatalf("got %+v", out)
	}
}

func TestWorkspacesResponseRoundTrip(t *testing.T) {
	in := Response{OK: true, TaskID: "t1", SessionSecret: "s", Launcher: &AgentRef{Name: "host"},
		Workspaces: []WorkspaceInfo{
			{Scope: "/home/ra/proj", LiveAgents: 2, EyesAgents: 1, LauncherAgents: 1},
		}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Response
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.TaskID != "t1" || out.SessionSecret != "s" || out.Launcher == nil || out.Launcher.Name != "host" {
		t.Fatalf("got %+v", out)
	}
	if len(out.Workspaces) != 1 || out.Workspaces[0].EyesAgents != 1 || out.Workspaces[0].LauncherAgents != 1 {
		t.Fatalf("workspaces: %+v", out.Workspaces)
	}
}

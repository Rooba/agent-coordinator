package protocol

import (
	"encoding/json"
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

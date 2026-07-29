package hookcli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/socktest"
)

func TestNormalizePaths(t *testing.T) {
	got := normalizePaths("/repo", []string{"internal/a.go", "/other/b.go"})
	if len(got) != 2 || got[0] != "/repo/internal/a.go" || got[1] != "/other/b.go" {
		t.Fatalf("got %v", got)
	}
}

// fakeDaemon answers every request with the canned response and records requests.
func fakeDaemon(t *testing.T, resp protocol.Response) (string, *[]protocol.Request) {
	t.Helper()
	sock := filepath.Join(socktest.Dir(t), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var got []protocol.Request
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var r protocol.Request
			json.Unmarshal(line, &r)
			got = append(got, r)
			b, _ := json.Marshal(resp)
			c.Write(append(b, '\n'))
			c.Close()
		}
	}()
	return sock, &got
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Skipf("fixture %s missing (run spike task 0): %v", name, err)
	}
	return b
}

func TestPostToolUseEmitsNotices(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Notices: []string{"[coordinator] 1 new message from amber-fox - call read_messages"}})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "post_read.json")), &out, sock)
	if len(*got) != 1 || (*got)[0].Op != protocol.OpEvent || (*got)[0].Tool == "" {
		t.Fatalf("daemon saw %+v", got)
	}
	if !strings.Contains(out.String(), "additionalContext") || !strings.Contains(out.String(), "amber-fox") {
		t.Fatalf("stdout: %s", out.String())
	}
}

func TestPostToolUseSilentWhenNoNotices(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "post_read.json")), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("must emit nothing without notices, got %s", out.String())
	}
}

func TestSessionStartIntroducesName(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true, Name: "amber-fox"})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "session_start.json")), &out, sock)
	if len(*got) != 1 || (*got)[0].Op != protocol.OpRegister {
		t.Fatalf("daemon saw %+v", got)
	}
	if !strings.Contains(out.String(), "amber-fox") {
		t.Fatalf("stdout: %s", out.String())
	}
	// The injection teaches the wake pattern with the agent's actual name.
	if !strings.Contains(out.String(), "agent-coordinator wait 'amber-fox'") {
		t.Fatalf("missing wake-pattern teaching: %s", out.String())
	}
}

func TestStopEmitsBlockOnNotices(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true,
		Notices: []string{"[coordinator] 1 new message from amber-fox - call read_messages"}})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "stop.json")), &out, sock)
	if len(*got) != 1 || (*got)[0].Op != protocol.OpIdle {
		t.Fatalf("daemon saw %+v", got)
	}
	for _, want := range []string{`"decision"`, `"block"`, "amber-fox", "read_messages"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout missing %q: %s", want, out.String())
		}
	}
}

func TestStopSilentWithoutNotices(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "stop.json")), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("stop without notices must emit nothing, got %s", out.String())
	}
}

func TestUserPromptSubmitDrainsNotices(t *testing.T) {
	// No captured fixture exists for UserPromptSubmit, so the input is built
	// from the known common hook fields (session_id, cwd, hook_event_name)
	// plus this event's "prompt" field.
	const input = `{"session_id":"3cdc4c5b-e78d-4fbb-9859-f18d8dc2b200","cwd":"/home/user/agent-coordinator-go","hook_event_name":"UserPromptSubmit","prompt":"please continue"}`
	sock, got := fakeDaemon(t, protocol.Response{OK: true,
		Notices: []string{"[coordinator] broadcast from brisk-owl - call read_messages"}})
	var out bytes.Buffer
	Run(strings.NewReader(input), &out, sock)
	if len(*got) != 1 {
		t.Fatalf("daemon saw %+v", got)
	}
	if r := (*got)[0]; r.Op != protocol.OpEvent || r.Tool != "UserPromptSubmit" ||
		r.Activity != "Handling user prompt" || len(r.Files) != 0 || len(r.Writes) != 0 || r.TaskEv != nil {
		t.Fatalf("daemon saw %+v", r)
	}
	for _, want := range []string{`"UserPromptSubmit"`, "additionalContext", "brisk-owl"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout missing %q: %s", want, out.String())
		}
	}
}

func TestFailOpenWithoutDaemon(t *testing.T) {
	// AC_NO_SPAWN keeps the test hermetic - otherwise the miss would spawn the
	// test binary itself as "daemon".
	t.Setenv("AC_NO_SPAWN", "1")
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "post_read.json")), &out, filepath.Join(socktest.Dir(t), "absent.sock"))
	if out.Len() != 0 {
		t.Fatalf("must be silent when daemon is unreachable, got %s", out.String())
	}
}

func TestSubagentEventTagged(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "post_bash_subagent.json")), &out, sock)
	if len(*got) != 1 || !strings.Contains((*got)[0].Activity, "(subagent:") {
		t.Fatalf("daemon saw %+v", got)
	}
}

func TestGarbageInputIsSilent(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{OK: true})
	var out bytes.Buffer
	Run(strings.NewReader("not json at all"), &out, sock)
	if out.Len() != 0 {
		t.Fatalf("garbage must be swallowed, got %s", out.String())
	}
}

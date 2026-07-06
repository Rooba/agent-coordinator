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

	"github.com/agent-coordinator/go/internal/protocol"
)

// fakeDaemon answers every request with the canned response and records requests.
func fakeDaemon(t *testing.T, resp protocol.Response) (string, *[]protocol.Request) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
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
}

func TestFailOpenWithoutDaemon(t *testing.T) {
	var out bytes.Buffer
	Run(bytes.NewReader(fixture(t, "post_read.json")), &out, filepath.Join(t.TempDir(), "absent.sock"))
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

package mcpserv

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-coordinator/go/internal/protocol"
)

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

func rpc(t *testing.T, sock string, lines ...string) []string {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out bytes.Buffer
	if err := Serve(in, &out, sock, "/some/repo"); err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(out.String()), "\n")
}

func TestInitializeAndList(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{OK: true})
	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if len(out) != 2 {
		t.Fatalf("want 2 responses (notification is silent), got %d: %v", len(out), out)
	}
	if !strings.Contains(out[0], `"protocolVersion"`) || !strings.Contains(out[0], "agent-coordinator") {
		t.Fatalf("initialize: %s", out[0])
	}
	for _, tool := range []string{"status_board", "list_agents", "send_message", "read_messages", "broadcast"} {
		if !strings.Contains(out[1], `"`+tool+`"`) {
			t.Fatalf("tools/list missing %s: %s", tool, out[1])
		}
	}
}

// Regression: a single line over the old 1MiB Scanner cap must be served,
// not kill Serve.
func TestOversizedLineIsServed(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{OK: true})
	pad := strings.Repeat("x", 1<<20+4096)
	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"`+pad+`"}}`)
	if len(out) != 1 || !strings.Contains(out[0], "status_board") {
		t.Fatalf("oversized request not served: %d responses", len(out))
	}
}

// Regression: a garbage line must yield a JSON-RPC -32700 parse error and
// Serve must keep serving subsequent requests.
func TestGarbageLineYieldsParseError(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{OK: true})
	out := rpc(t, sock,
		"this is not json",
		`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)
	if len(out) != 2 {
		t.Fatalf("want parse-error line + response, got %v", out)
	}
	if !strings.Contains(out[0], "-32700") || !strings.Contains(out[0], `"id":null`) || !strings.Contains(out[0], "parse error") {
		t.Fatalf("parse error line: %s", out[0])
	}
	if !strings.Contains(out[1], `"id":7`) || !strings.Contains(out[1], "status_board") {
		t.Fatalf("server must keep serving after garbage: %s", out[1])
	}
}

func TestSendMessageToolCall(t *testing.T) {
	sock, got := fakeDaemon(t, protocol.Response{OK: true})
	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_message","arguments":{"from":"amber-fox","to":"brisk-owl","body":"ping"}}}`)
	if len(*got) != 1 || (*got)[0].Op != protocol.OpSend || (*got)[0].To != "brisk-owl" || (*got)[0].Scope != "/some/repo" {
		t.Fatalf("daemon saw %+v", *got)
	}
	if !strings.Contains(out[0], `"content"`) {
		t.Fatalf("tool result: %s", out[0])
	}
}

func TestDaemonErrorSurfacesAsToolError(t *testing.T) {
	sock, _ := fakeDaemon(t, protocol.Response{Error: "no agent \"nobody\" in this workspace"})
	out := rpc(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_message","arguments":{"from":"a","to":"nobody","body":"x"}}}`)
	if !strings.Contains(out[0], `"isError":true`) || !strings.Contains(out[0], "nobody") {
		t.Fatalf("want isError with reason: %s", out[0])
	}
}

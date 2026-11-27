package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/socktest"
)

// boardDaemon answers every request with OK, one gone agent and one unread
// message, and records what it received.
func boardDaemon(t *testing.T) (string, chan protocol.Request) {
	t.Helper()
	sock := filepath.Join(socktest.Dir(t), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	reqs := make(chan protocol.Request, 16)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var req protocol.Request
			json.Unmarshal(line, &req)
			reqs <- req
			resp := protocol.Response{OK: true, Agents: []protocol.AgentInfo{
				{Name: "amber-fox", Status: "gone"},
			}, Unread: 1, HighWater: 1, PeekIDs: []int64{1}, PeekFroms: []string{"brisk-owl"}}
			b, _ := json.Marshal(resp)
			c.Write(append(b, '\n'))
			c.Close()
		}
	}()
	return sock, reqs
}

func TestFetchBoardAllSendsIncludeGone(t *testing.T) {
	sock, reqs := boardDaemon(t)
	agents, err := fetchBoard(sock, "/r", false, true)
	if err != nil {
		t.Fatal(err)
	}
	req := <-reqs
	if req.Op != protocol.OpBoard || !req.IncludeGone {
		t.Fatalf("--all must send OpBoard with IncludeGone=true, got %+v", req)
	}
	if len(agents) != 1 || agents[0].Status != "gone" {
		t.Fatalf("want the daemon's gone row passed through, got %+v", agents)
	}
}

func TestFetchBoardDefaultOmitsIncludeGone(t *testing.T) {
	sock, reqs := boardDaemon(t)
	if _, err := fetchBoard(sock, "/r", false, false); err != nil {
		t.Fatal(err)
	}
	req := <-reqs
	if req.Op != protocol.OpBoard || req.IncludeGone {
		t.Fatalf("default board must send OpBoard with IncludeGone=false, got %+v", req)
	}
}

func TestFetchBoardLiveSendsOpAgents(t *testing.T) {
	sock, reqs := boardDaemon(t)
	if _, err := fetchBoard(sock, "/r", true, false); err != nil {
		t.Fatal(err)
	}
	req := <-reqs
	if req.Op != protocol.OpAgents || req.IncludeGone {
		t.Fatalf("--live must send OpAgents without IncludeGone, got %+v", req)
	}
}

func TestScopedCommandsHonorACScope(t *testing.T) {
	commands := map[string]func(){
		"board":      func() { runBoard(nil) },
		"wait":       func() { runWait([]string{"amber-fox", "-timeout", "5"}) },
		"workspaces": func() { runWorkspaces(nil) },
		"eyes":       func() { runEyes(nil) },
	}
	for name, run := range commands {
		t.Run(name, func(t *testing.T) {
			sock, reqs := boardDaemon(t)
			t.Setenv("AC_SOCKET", sock)
			t.Setenv("AC_DB", filepath.Join(t.TempDir(), "coordinator.db"))
			t.Setenv("AC_SCOPE", "/elsewhere")
			run()
			if len(reqs) == 0 {
				t.Fatalf("%s sent no requests", name)
			}
			for len(reqs) > 0 {
				if req := <-reqs; req.Scope != "/elsewhere" {
					t.Fatalf("%s sent scope %q, want AC_SCOPE /elsewhere", name, req.Scope)
				}
			}
		})
	}
}

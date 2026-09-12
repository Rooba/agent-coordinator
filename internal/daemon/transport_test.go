package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/store"
)

const transportAgents = 30

// transportFixture isolates the socket transport: both listeners use the same
// handler and store without the relay's extra authentication work. This is a
// test-only TCP listener, not the production relay or a supported client mode.
func transportFixture(t testing.TB, network string) (string, []protocol.Request) {
	t.Helper()
	dir, err := os.MkdirTemp("", "act") // short enough for Unix socket paths on every platform
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	addr := "127.0.0.1:0"
	if network == "unix" {
		addr = filepath.Join(dir, "d.sock")
	}
	l, err := net.Listen(network, addr)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	var lastActivity atomic.Int64
	var handlers sync.WaitGroup
	exited := make(chan error, 1)
	go func() { exited <- accept(l, st, nil, &lastActivity, &handlers) }()
	t.Cleanup(func() {
		l.Close()
		if err := <-exited; err != nil {
			t.Error(err)
		}
		handlers.Wait()
		st.Close()
	})
	requests := make([]protocol.Request, transportAgents)
	for i := range requests {
		req := protocol.Request{Op: protocol.OpRegister, Scope: "/transport", SessionID: fmt.Sprintf("session-%d", i)}
		resp, err := transportRoundTrip(network, l.Addr().String(), req)
		if err != nil {
			t.Fatal(err)
		}
		req.Op, req.To, req.Body = protocol.OpSend, resp.Name, "transport load test"
		requests[i] = req
	}
	return l.Addr().String(), requests
}

func transportRoundTrip(network, addr string, req protocol.Request) (protocol.Response, error) {
	conn, err := net.DialTimeout(network, addr, time.Second)
	if err != nil {
		return protocol.Response{}, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return protocol.Response{}, err
	}
	var resp protocol.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return resp, err
	}
	if !resp.OK {
		return resp, fmt.Errorf("%s: %s", req.Op, resp.Error)
	}
	return resp, nil
}

// Concurrent board reads share the same single SQLite connection as messages.
// Changing the listener must not lose or duplicate mail under this workload.
func TestTransportConcurrentMailAndBoard(t *testing.T) {
	for _, network := range []string{"unix", "tcp"} {
		t.Run(network, func(t *testing.T) {
			addr, requests := transportFixture(t, network)
			var workers sync.WaitGroup
			for _, req := range requests {
				workers.Add(1)
				go func(req protocol.Request) {
					defer workers.Done()
					for i := 0; i < 10; i++ {
						req.Op, req.Body = protocol.OpSend, fmt.Sprintf("message-%d", i)
						if _, err := transportRoundTrip(network, addr, req); err != nil {
							t.Error(err)
							return
						}
						req.Op = protocol.OpBoard
						resp, err := transportRoundTrip(network, addr, req)
						if err != nil || len(resp.Agents) != transportAgents {
							t.Errorf("board agents=%d, err=%v", len(resp.Agents), err)
							return
						}
					}
				}(req)
			}
			workers.Wait()
			for _, req := range requests {
				req.Op = protocol.OpRead
				resp, err := transportRoundTrip(network, addr, req)
				if err != nil {
					t.Fatal(err)
				}
				seen := make(map[string]bool)
				for _, msg := range resp.Messages {
					seen[msg.Body] = true
				}
				if len(resp.Messages) != 10 || len(seen) != 10 {
					t.Fatalf("%s received %d messages with %d distinct bodies", req.To, len(resp.Messages), len(seen))
				}
			}
		})
	}
}

func BenchmarkTransportConcurrent(b *testing.B) {
	for _, op := range []string{protocol.OpSend, protocol.OpBoard} {
		for _, network := range []string{"unix", "tcp"} {
			b.Run(op+"/"+network, func(b *testing.B) {
				addr, requests := transportFixture(b, network)
				var workers sync.WaitGroup
				b.ResetTimer()
				for worker, req := range requests {
					req.Op = op
					workers.Add(1)
					go func(worker int, req protocol.Request) {
						defer workers.Done()
						for i := worker; i < b.N; i += transportAgents {
							if _, err := transportRoundTrip(network, addr, req); err != nil {
								b.Error(err)
								return
							}
						}
					}(worker, req)
				}
				workers.Wait()
				b.StopTimer()
			})
		}
	}
}

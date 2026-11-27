package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/socktest"
	"github.com/Rooba/agent-coordinator/internal/wake"
)

// recordingDaemon answers every peek with one unread message and keeps the
// requests it saw.
func recordingDaemon(t *testing.T) (string, func() []protocol.Request) {
	t.Helper()
	sock := filepath.Join(socktest.Dir(t), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var mu sync.Mutex
	var seen []protocol.Request
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var req protocol.Request
			json.Unmarshal(line, &req)
			mu.Lock()
			seen = append(seen, req)
			mu.Unlock()
			b, _ := json.Marshal(protocol.Response{OK: true, Unread: 1, HighWater: 5,
				PeekIDs: []int64{5}, PeekFroms: []string{"peer-fox"}})
			c.Write(append(b, '\n'))
			c.Close()
		}
	}()
	return sock, func() []protocol.Request {
		mu.Lock()
		defer mu.Unlock()
		return append([]protocol.Request(nil), seen...)
	}
}

// cancelOnWrite ends the watch as soon as its first line is out.
type cancelOnWrite struct {
	cancel context.CancelFunc
	line   string
}

func (w *cancelOnWrite) Write(p []byte) (int, error) {
	w.line += string(p)
	w.cancel()
	return len(p), nil
}

// An auto-bound watch pins the session id on every probe and sends no name,
// so the daemon resolves the inbox from the session even if the name is later
// minted for someone else.
func TestWatchBoundProbesCarrySessionIDNotName(t *testing.T) {
	sock, requests := recordingDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := &cancelOnWrite{cancel: cancel}
	err := watchMail(ctx, sock, waitStateFile(t), protocol.Request{Scope: "/r", SessionID: "sess-a", Watcher: true},
		out, wake.Options{Interval: 5 * time.Millisecond, Quiet: 5 * time.Millisecond})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("watchMail: %v", err)
	}
	if out.line != "mail from=peer-fox count=1 ids=5\n" {
		t.Fatalf("line = %q", out.line)
	}
	seen := requests()
	if len(seen) < 2 {
		t.Fatalf("expected repeated probes, saw %d", len(seen))
	}
	for _, req := range seen {
		if req.Op != protocol.OpPeek || req.Scope != "/r" || req.SessionID != "sess-a" || req.From != "" || !req.Watcher {
			t.Fatalf("probe must carry the bound session only: %+v", req)
		}
	}
}

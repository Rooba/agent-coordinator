package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-coordinator/go/internal/protocol"
)

// peekDaemon answers peek with 0 unread for the first `misses` polls, then 1.
func peekDaemon(t *testing.T, misses int64) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var polls atomic.Int64
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			bufio.NewReader(c).ReadBytes('\n')
			resp := protocol.Response{OK: true}
			if polls.Add(1) > misses {
				resp.Unread = 1
			}
			b, _ := json.Marshal(resp)
			c.Write(append(b, '\n'))
			c.Close()
		}
	}()
	return sock
}

func TestWaitForMailFindsMailAfterPolls(t *testing.T) {
	sock := peekDaemon(t, 2)
	n, found := waitForMail(sock, "/r", "amber-fox", 5*time.Second, 10*time.Millisecond)
	if !found || n != 1 {
		t.Fatalf("want found=true n=1 after two misses, got found=%v n=%d", found, n)
	}
}

func TestWaitForMailTimesOut(t *testing.T) {
	sock := peekDaemon(t, 1<<40) // never flips
	n, found := waitForMail(sock, "/r", "amber-fox", 50*time.Millisecond, 10*time.Millisecond)
	if found || n != 0 {
		t.Fatalf("want timeout, got found=%v n=%d", found, n)
	}
}

func TestWaitForMailSurvivesMissingDaemon(t *testing.T) {
	// A failed poll is a miss, not an error: the daemon may be idle-restarting.
	sock := filepath.Join(t.TempDir(), "absent.sock")
	if _, found := waitForMail(sock, "/r", "x", 30*time.Millisecond, 10*time.Millisecond); found {
		t.Fatal("must time out quietly without a daemon")
	}
}

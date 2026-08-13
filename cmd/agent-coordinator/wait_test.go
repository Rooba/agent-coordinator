package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/socktest"
)

// peekDaemon answers peek. The first `baselineMisses` successful connections
// fail (daemon down). Then the next `stalePolls` report HighWater=hw with 0
// unread after that baseline (stale backlog only). After that, each poll
// reports fresh unread above the baseline.
func peekDaemon(t *testing.T, baselineMisses, stalePolls int64, hw int64) string {
	t.Helper()
	sock := filepath.Join(socktest.Dir(t), "d.sock")
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
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var req protocol.Request
			json.Unmarshal(line, &req)
			n := polls.Add(1)
			resp := protocol.Response{OK: true, HighWater: hw}
			if n <= baselineMisses {
				// Simulate unreachable: close without a response so dial/read fails.
				c.Close()
				continue
			}
			// After baseline arm, polls with AfterID=hw see fresh mail only
			// once stalePolls of "still only old mail" have elapsed.
			if req.AfterID >= hw {
				armedPoll := n - baselineMisses - 1 // 1-based arm already used one poll
				if armedPoll > stalePolls {
					resp.Unread = 1
					resp.PeekIDs = []int64{hw + 1}
					resp.PeekFroms = []string{"sender-fox"}
				}
			} else {
				// Baseline arm (AfterID=0): report high water; may also report
				// stale unread count which wait must ignore.
				resp.Unread = 3
				resp.PeekIDs = []int64{hw - 2, hw - 1, hw}
				resp.PeekFroms = []string{"old-owl"}
			}
			b, _ := json.Marshal(resp)
			c.Write(append(b, '\n'))
			c.Close()
		}
	}()
	return sock
}

func TestWaitForMailFindsMailAfterPolls(t *testing.T) {
	// Arm sees hw=10 (and 3 stale unread). Two polls still only stale. Then fresh.
	sock := peekDaemon(t, 0, 2, 10)
	result, found, err := waitForMail(sock, "/r", "amber-fox", 5*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !found || result.Unread != 1 || len(result.IDs) != 1 || result.IDs[0] != 11 {
		t.Fatalf("want found with id 11, got found=%v result=%+v", found, result)
	}
	if len(result.Froms) != 1 || result.Froms[0] != "sender-fox" {
		t.Fatalf("want sender-fox, got %v", result.Froms)
	}
}

func TestWaitForMailTimesOutOnStaleBacklogOnly(t *testing.T) {
	// High water 10, never injects mail above it - only "stale" responses forever.
	sock := peekDaemon(t, 0, 1<<40, 10)
	result, found, err := waitForMail(sock, "/r", "amber-fox", 50*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if found || result.Unread != 0 {
		t.Fatalf("stale backlog must not wake: found=%v result=%+v", found, result)
	}
}

func TestWaitForMailTimesOut(t *testing.T) {
	sock := peekDaemon(t, 0, 1<<40, 10)
	result, found, err := waitForMail(sock, "/r", "amber-fox", 50*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if found || result.Unread != 0 {
		t.Fatalf("want timeout, got found=%v result=%+v", found, result)
	}
}

func TestWaitForMailSurvivesMissingDaemon(t *testing.T) {
	// A failed poll is a miss, not an error: the daemon may be idle-restarting.
	// AC_NO_SPAWN keeps the test hermetic - otherwise the miss would spawn the
	// test binary itself as "daemon".
	t.Setenv("AC_NO_SPAWN", "1")
	sock := filepath.Join(socktest.Dir(t), "absent.sock")
	if _, found, err := waitForMail(sock, "/r", "x", 30*time.Millisecond, 10*time.Millisecond); found || err != nil {
		t.Fatal("must time out quietly without a daemon")
	}
}

func TestWaitForMailImmediateWhenFreshAtArm(t *testing.T) {
	// Baseline high water is 0 (no prior deliveries). First post-arm peek sees mail id 1.
	sock := filepath.Join(socktest.Dir(t), "fresh.sock")
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
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var req protocol.Request
			json.Unmarshal(line, &req)
			n := polls.Add(1)
			resp := protocol.Response{OK: true}
			if n == 1 {
				// Arm: no prior deliveries.
				resp.HighWater = 0
			} else if req.AfterID == 0 {
				resp.HighWater = 1
				resp.Unread = 1
				resp.PeekIDs = []int64{1}
				resp.PeekFroms = []string{"brisk-owl"}
			} else {
				resp.HighWater = 1
				resp.Unread = 1
				resp.PeekIDs = []int64{1}
				resp.PeekFroms = []string{"brisk-owl"}
			}
			b, _ := json.Marshal(resp)
			c.Write(append(b, '\n'))
			c.Close()
		}
	}()
	result, found, err := waitForMail(sock, "/r", "amber-fox", 2*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !found || result.Unread != 1 {
		t.Fatalf("want immediate wake on fresh mail, got found=%v result=%+v", found, result)
	}
}

func TestWaitForMailUnknownNameFailsFast(t *testing.T) {
	// Daemon is live but refuses the name at arm time: wait must surface the
	// resolve error immediately instead of burning the full timeout.
	sock := filepath.Join(socktest.Dir(t), "refuse.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			bufio.NewReader(c).ReadBytes('\n')
			b, _ := json.Marshal(protocol.Response{OK: false, Error: `no agent "nosuch" in this workspace`})
			c.Write(append(b, '\n'))
			c.Close()
		}
	}()
	start := time.Now()
	_, found, err := waitForMail(sock, "/r", "nosuch", 5*time.Second, 10*time.Millisecond)
	if found || err == nil {
		t.Fatalf("unknown name must return an error, got found=%v err=%v", found, err)
	}
	if !strings.Contains(err.Error(), "no agent") {
		t.Fatalf("error must carry the daemon's resolve message, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("must fail fast, not wait out the timeout")
	}
}

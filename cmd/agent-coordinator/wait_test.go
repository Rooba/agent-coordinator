package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
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
			if req.AfterID != 0 {
				armedPoll := n - baselineMisses - 1 // 1-based arm already used one poll
				if armedPoll > stalePolls {
					resp.Unread = 1
					resp.HighWater = hw + 1
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

func waitStateFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "wait.json")
}

func TestWaitForMailFindsMailAfterPolls(t *testing.T) {
	// Arm sees hw=10 (and 3 stale unread). Two polls still only stale. Then fresh.
	sock := peekDaemon(t, 0, 2, 10)
	result, found, err := waitForMail(sock, waitStateFile(t), "/r", "amber-fox", 5*time.Second, 10*time.Millisecond, nil)
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
	result, found, err := waitForMail(sock, waitStateFile(t), "/r", "amber-fox", 50*time.Millisecond, 10*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if found || result.Unread != 0 {
		t.Fatalf("stale backlog must not wake: found=%v result=%+v", found, result)
	}
}

func TestWaitForMailTimesOut(t *testing.T) {
	sock := peekDaemon(t, 0, 1<<40, 10)
	result, found, err := waitForMail(sock, waitStateFile(t), "/r", "amber-fox", 50*time.Millisecond, 10*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if found || result.Unread != 0 {
		t.Fatalf("want timeout, got found=%v result=%+v", found, result)
	}
}

func TestWaitForMailRearmsAfterProcessDies(t *testing.T) {
	statePath := waitStateFile(t)
	sock := peekDaemon(t, 0, 0, 10)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		waitForMail(sock, statePath, "/r", "amber-fox", time.Second, 10*time.Millisecond, func(afterID int64) {
			persisted, ok := loadWaitState(statePath, time.Now())
			if !ok || afterID != 10 || persisted != 10 {
				t.Errorf("armed callback preceded durable state: after=%d persisted=%d ok=%v", afterID, persisted, ok)
			}
			runtime.Goexit() // model a host watchdog killing the waiter after it armed
		})
	}()
	<-stopped

	result, found, err := waitForMail(sock, statePath, "/r", "amber-fox", time.Second, 10*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(result.IDs) != 1 || result.IDs[0] != 11 {
		t.Fatalf("re-arm lost mail delivered after the killed process armed: found=%v result=%+v", found, result)
	}
}

func TestWaitForMailRearmsAcrossTimeoutGapAndAdvancesAfterRead(t *testing.T) {
	statePath := waitStateFile(t)
	quietSock := peekDaemon(t, 0, 1<<40, 10)
	if _, found, err := waitForMail(quietSock, statePath, "/r", "amber-fox", 30*time.Millisecond, 10*time.Millisecond, nil); err != nil || found {
		t.Fatalf("initial wait must time out quietly: found=%v err=%v", found, err)
	}
	afterID, ok := loadWaitState(statePath, time.Now())
	if !ok || afterID != 10 {
		t.Fatalf("timeout must preserve baseline 10 for re-arm, got after=%d ok=%v", afterID, ok)
	}

	freshSock := peekDaemon(t, 0, 0, 10)
	result, found, err := waitForMail(freshSock, statePath, "/r", "amber-fox", time.Second, 10*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(result.IDs) != 1 || result.IDs[0] != 11 {
		t.Fatalf("re-arm lost mail delivered after timeout: found=%v result=%+v", found, result)
	}
	afterID, ok = loadWaitState(statePath, time.Now())
	if !ok || afterID != 10 {
		t.Fatalf("wake must not checkpoint unread mail before the harness sees it: after=%d ok=%v", afterID, ok)
	}
	// Model the first process dying after it detected mail but before its output
	// reached the harness. The replacement must deliver the same wake again.
	result, found, err = waitForMail(freshSock, statePath, "/r", "amber-fox", time.Second, 10*time.Millisecond, nil)
	if err != nil || !found || len(result.IDs) != 1 || result.IDs[0] != 11 {
		t.Fatalf("re-arm after an unobserved wake lost unread mail: found=%v result=%+v err=%v", found, result, err)
	}

	// Model the harness reading the wake's mail before it re-arms. The first
	// quiet peek proves the intervening delivery was consumed and advances it.
	staleSock := peekDaemon(t, 0, 1<<40, 11)
	if _, found, err := waitForMail(staleSock, statePath, "/r", "amber-fox", 30*time.Millisecond, 10*time.Millisecond, nil); err != nil || found {
		t.Fatalf("already reported mail must not wake forever: found=%v err=%v", found, err)
	}
	afterID, ok = loadWaitState(statePath, time.Now())
	if !ok || afterID != 11 {
		t.Fatalf("quiet re-arm must advance past consumed mail: after=%d ok=%v", afterID, ok)
	}
}

func TestWaitForMailConcurrentWaitersDoNotCheckpointUnread(t *testing.T) {
	statePath := waitStateFile(t)
	if err := saveWaitState(statePath, 10); err != nil {
		t.Fatal(err)
	}
	sock := peekDaemon(t, 0, 0, 10)
	type outcome struct {
		result waitResult
		found  bool
		err    error
	}
	outcomes := make(chan outcome, 2)
	for range 2 {
		go func() {
			result, found, err := waitForMail(sock, statePath, "/r", "amber-fox", time.Second, 10*time.Millisecond, nil)
			outcomes <- outcome{result: result, found: found, err: err}
		}()
	}
	for range 2 {
		got := <-outcomes
		if got.err != nil || !got.found || len(got.result.IDs) != 1 || got.result.IDs[0] != 11 {
			t.Fatalf("concurrent waiter lost wake: %+v", got)
		}
	}
	afterID, ok := loadWaitState(statePath, time.Now())
	if !ok || afterID != 10 {
		t.Fatalf("concurrent wakes must leave unread mail re-armable: after=%d ok=%v", afterID, ok)
	}
}

func TestWaitForMailReplacesCorruptOrExpiredState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		expired bool
	}{
		{name: "corrupt", content: "not an id"},
		{name: "expired", content: "4", expired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statePath := waitStateFile(t)
			if err := os.WriteFile(statePath, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.expired {
				old := time.Now().Add(-waitStateTTL - time.Hour)
				if err := os.Chtimes(statePath, old, old); err != nil {
					t.Fatal(err)
				}
			}
			sock := peekDaemon(t, 0, 1<<40, 10)
			if _, found, err := waitForMail(sock, statePath, "/r", "amber-fox", 30*time.Millisecond, 10*time.Millisecond, nil); err != nil || found {
				t.Fatalf("replacement arm must time out quietly: found=%v err=%v", found, err)
			}
			afterID, ok := loadWaitState(statePath, time.Now())
			if !ok || afterID != 10 {
				t.Fatalf("invalid state was not safely replaced: after=%d ok=%v", afterID, ok)
			}
		})
	}
}

func TestWaitForMailFailsBeforeArmedWhenCheckpointCannotBeSaved(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	armed := false
	_, found, err := waitForMail(peekDaemon(t, 0, 1<<40, 10), filepath.Join(parent, "wait"), "/r", "amber-fox",
		time.Second, 10*time.Millisecond, func(int64) { armed = true })
	if found || err == nil || armed || !strings.Contains(err.Error(), "save arm state") {
		t.Fatalf("must fail durably before acknowledging arm: found=%v armed=%v err=%v", found, armed, err)
	}
}

func TestSaveWaitStateConcurrentWritersStayValid(t *testing.T) {
	statePath := waitStateFile(t)
	errs := make(chan error, 16)
	for id := int64(0); id < 16; id++ {
		go func() { errs <- saveWaitState(statePath, id) }()
	}
	for range 16 {
		if err := <-errs; err != nil {
			t.Error(err) // collect every writer before temporary-directory cleanup
		}
	}
	afterID, ok := loadWaitState(statePath, time.Now())
	if !ok || afterID < 0 || afterID >= 16 {
		t.Fatalf("concurrent replacement left an invalid cursor: after=%d ok=%v", afterID, ok)
	}
}

func TestWaitStateRenameRetriesTransientWindowsErrors(t *testing.T) {
	results := []error{
		&os.LinkError{Op: "rename", Old: "temporary", New: "cursor", Err: syscall.Errno(5)},
		&os.LinkError{Op: "rename", Old: "temporary", New: "cursor", Err: syscall.Errno(32)},
		nil,
	}
	calls := 0
	var delays []time.Duration
	err := retryWaitStateRename(func() error {
		if calls >= len(results) {
			t.Fatal("rename continued after succeeding")
		}
		result := results[calls]
		calls++
		return result
	}, true, func(d time.Duration) { delays = append(delays, d) })
	if err != nil || calls != 3 || len(delays) != 2 || delays[0] <= 0 || delays[1] <= delays[0] {
		t.Fatalf("transient retry: calls=%d delays=%v err=%v", calls, delays, err)
	}
}

func TestWaitStateRenameReturnsPermanentOrExhaustedError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		onWindows bool
		cause     error
		retry     bool
	}{
		{"nonretryable", true, os.ErrNotExist, false},
		{"unix", false, syscall.Errno(5), false},
		{"access denied exhausted", true, syscall.Errno(5), true},
		{"sharing violation exhausted", true, syscall.Errno(32), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := &os.LinkError{Op: "rename", Old: "temporary", New: "cursor", Err: tc.cause}
			calls, slept := 0, time.Duration(0)
			err := retryWaitStateRename(func() error {
				calls++
				if calls > 10 {
					t.Fatal("rename retry is not bounded")
				}
				return failure
			}, tc.onWindows, func(d time.Duration) { slept += d })
			if err != failure || !errors.Is(err, tc.cause) {
				t.Fatalf("must preserve final replacement error: %v", err)
			}
			if tc.retry {
				if calls < 2 || slept <= 0 || slept > 2*time.Second {
					t.Fatalf("retry budget: calls=%d sleep=%v", calls, slept)
				}
			} else if calls != 1 || slept != 0 {
				t.Fatalf("nonretryable replacement retried: calls=%d sleep=%v", calls, slept)
			}
		})
	}
}

func TestWaitForMailSurvivesMissingDaemon(t *testing.T) {
	// A failed poll is a miss, not an error: the daemon may be idle-restarting.
	// AC_NO_SPAWN keeps the test hermetic - otherwise the miss would spawn the
	// test binary itself as "daemon".
	t.Setenv("AC_NO_SPAWN", "1")
	sock := filepath.Join(socktest.Dir(t), "absent.sock")
	if _, found, err := waitForMail(sock, waitStateFile(t), "/r", "x", 30*time.Millisecond, 10*time.Millisecond, nil); found || err != nil {
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
	result, found, err := waitForMail(sock, waitStateFile(t), "/r", "amber-fox", 2*time.Second, 10*time.Millisecond, nil)
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
	_, found, err := waitForMail(sock, waitStateFile(t), "/r", "nosuch", 5*time.Second, 10*time.Millisecond, nil)
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

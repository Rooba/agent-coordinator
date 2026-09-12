package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Rooba/agent-coordinator/internal/dialer"
	"github.com/Rooba/agent-coordinator/internal/paths"
	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/scope"
)

// runWait blocks until the named agent receives mail newer than its durable
// cursor (exit 0) or the timeout passes (exit 1). The cursor lets a replacement
// process continue an interrupted arm without repeatedly waking on old mail.
func runWait(args []string) {
	usage := func() {
		fmt.Fprintln(os.Stderr, "usage: agent-coordinator wait <name> [-timeout <seconds>] [-interval <seconds>]")
		os.Exit(2)
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		usage()
	}
	name := args[0]
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	timeout := fs.Int("timeout", 570, "seconds before giving up (default stays under 600s background caps)")
	interval := fs.Int("interval", 2, "seconds between polls")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *timeout <= 0 || *interval <= 0 {
		usage()
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	sc := scope.Resolve(cwd)
	statePath, err := waitStatePath(sc, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wait: %v\n", err)
		os.Exit(2)
	}
	result, found, err := waitForMail(paths.Socket(), statePath, sc, name,
		time.Duration(*timeout)*time.Second, time.Duration(*interval)*time.Second,
		func(afterID int64) { fmt.Fprintf(os.Stderr, "armed after_id=%d\n", afterID) })
	if err != nil {
		fmt.Fprintf(os.Stderr, "wait: %v\n", err)
		os.Exit(2)
	}
	if !found {
		fmt.Println("timeout")
		os.Exit(1)
	}
	from := strings.Join(result.Froms, ",")
	if from == "" {
		from = "?"
	}
	ids := make([]string, len(result.IDs))
	for i, id := range result.IDs {
		ids[i] = fmt.Sprintf("%d", id)
	}
	fmt.Printf("mail from=%s count=%d ids=%s\n", from, result.Unread, strings.Join(ids, ","))
}

// waitResult is the machine-parseable summary printed on a successful wake.
type waitResult struct {
	Unread int
	IDs    []int64
	Froms  []string
}

// A checkpoint survives ordinary timeouts and killed waiter processes, but
// eventually ages out rather than changing first-arm semantics forever.
const waitStateTTL = 24 * time.Hour

// waitForMail resumes a live cursor or creates one from the current high-water
// id, then polls for newer unread mail. Timeouts preserve the cursor; a wake
// leaves it behind the unread delivery until a later quiet peek proves the
// mail was consumed. Definitive daemon refusals fail fast, while transport
// failures remain misses because the daemon may be idle-restarting.
func waitForMail(socketPath, statePath, sc, name string, timeout, interval time.Duration, armed func(int64)) (waitResult, bool, error) {
	now := time.Now()
	deadline := now.Add(timeout)
	afterID, ok := loadWaitState(statePath, now)
	if !ok {
		switch info, err := peekOnce(socketPath, sc, name, 0); {
		case err == nil:
			afterID = info.HighWater
		case errors.As(err, new(daemonErr)):
			return waitResult{}, false, err
		}
		if err := saveWaitState(statePath, afterID); err != nil {
			return waitResult{}, false, fmt.Errorf("save arm state: %w", err)
		}
	} else if err := os.Chtimes(statePath, now, now); err != nil {
		return waitResult{}, false, fmt.Errorf("refresh arm state: %w", err)
	}
	if armed != nil {
		armed(afterID)
	}
	for {
		info, err := peekOnce(socketPath, sc, name, afterID)
		if err == nil && info.Unread > 0 {
			// Do not checkpoint unread mail before the wake reaches the harness.
			// If this process dies now, its replacement must see the same mail.
			return waitResult{Unread: info.Unread, IDs: info.IDs, Froms: info.Froms}, true, nil
		}
		if err == nil && info.HighWater > afterID {
			// Deliveries above the cursor have already been consumed. Advancing
			// here prevents them from becoming stale wake candidates without
			// ever checkpointing past unread mail.
			afterID = info.HighWater
			if err := saveWaitState(statePath, afterID); err != nil {
				return waitResult{}, false, fmt.Errorf("advance consumed state: %w", err)
			}
		}
		if errors.As(err, new(daemonErr)) {
			return waitResult{}, false, err
		}
		if time.Now().After(deadline) {
			return waitResult{}, false, nil
		}
		time.Sleep(interval)
	}
}

func waitStatePath(sc, name string) (string, error) {
	db, err := paths.DB()
	if err != nil {
		return "", err
	}
	key := sha256.Sum256([]byte(sc + "\x00" + name))
	return filepath.Join(filepath.Dir(db), "wait", fmt.Sprintf("%x.cursor", key)), nil
}

func loadWaitState(path string, now time.Time) (int64, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().Add(waitStateTTL).After(now) {
		return 0, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	afterID, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || afterID < 0 {
		return 0, false
	}
	return afterID, true
}

func saveWaitState(path string, afterID int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".wait-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err = fmt.Fprintln(f, afterID); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return retryWaitStateRename(func() error { return os.Rename(tmp, path) }, runtime.GOOS == "windows", time.Sleep)
}

func retryWaitStateRename(rename func() error, onWindows bool, sleep func(time.Duration)) error {
	// Competing replacements can temporarily leave a Windows destination
	// pending deletion. Retry the atomic rename, never remove the old cursor.
	// Numeric Win32 codes keep this path testable from every platform.
	const accessDenied, sharingViolation = syscall.Errno(5), syscall.Errno(32)
	for attempt := 0; ; attempt++ {
		err := rename()
		if err == nil || !onWindows || attempt == 8 ||
			!errors.Is(err, accessDenied) && !errors.Is(err, sharingViolation) {
			return err
		}
		sleep(5 * time.Millisecond << attempt) // at most 1.275 seconds in total
	}
}

type peekInfo struct {
	Unread    int
	HighWater int64
	IDs       []int64
	Froms     []string
}

// daemonErr is a definitive refusal from a live daemon (e.g. "no agent X in
// this workspace"), as opposed to a transient transport failure.
type daemonErr string

func (e daemonErr) Error() string { return string(e) }

func peekOnce(socketPath, sc, name string, afterID int64) (peekInfo, error) {
	conn, err := dialer.Dial(socketPath, time.Second)
	if err != nil {
		return peekInfo{}, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	b, err := json.Marshal(protocol.Request{
		Op: protocol.OpPeek, Scope: sc, From: name, AfterID: afterID,
	})
	if err != nil {
		return peekInfo{}, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return peekInfo{}, err
	}
	var resp protocol.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return peekInfo{}, err
	}
	if !resp.OK {
		return peekInfo{}, daemonErr(resp.Error)
	}
	return peekInfo{
		Unread: resp.Unread, HighWater: resp.HighWater,
		IDs: resp.PeekIDs, Froms: resp.PeekFroms,
	}, nil
}

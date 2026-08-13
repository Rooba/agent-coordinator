package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Rooba/agent-coordinator/internal/dialer"
	"github.com/Rooba/agent-coordinator/internal/paths"
	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/scope"
)

// runWait blocks until the named agent receives mail newer than the arm-time
// baseline (exit 0) or the timeout passes (exit 1). Stale unread backlog does
// not wake: only messages with id strictly greater than the high-water mark
// at arm time count. Armed as a background task before delegating or idling,
// its exit is a harness touchpoint so a fresh DM re-invokes the agent.
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
	result, found, err := waitForMail(paths.Socket(), sc, name,
		time.Duration(*timeout)*time.Second, time.Duration(*interval)*time.Second)
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

// waitForMail arms against the agent's current high-water message id, then
// polls until a strictly newer unread delivery appears or the deadline passes.
// A definitive daemon refusal at arm time (e.g. an unknown agent name) is
// returned as an error so the caller can fail fast instead of burning the
// timeout. A transient failed poll is just a miss - the daemon may be
// idle-restarting - so polling continues.
func waitForMail(socketPath, sc, name string, timeout, interval time.Duration) (waitResult, bool, error) {
	deadline := time.Now().Add(timeout)
	// Baseline: max message id already delivered to this agent (AfterID=0
	// returns HighWater over all deliveries). Stale unread backlog sits at or
	// below this watermark and must not wake us. Daemon down at arm keeps the
	// baseline at zero so the first successful peek still works.
	baseline := int64(0)
	switch info, err := peekOnce(socketPath, sc, name, 0); {
	case err == nil:
		baseline = info.HighWater
	case errors.As(err, new(daemonErr)):
		return waitResult{}, false, err
	}
	for {
		if info, err := peekOnce(socketPath, sc, name, baseline); err == nil && info.Unread > 0 {
			return waitResult{Unread: info.Unread, IDs: info.IDs, Froms: info.Froms}, true, nil
		}
		if time.Now().After(deadline) {
			return waitResult{}, false, nil
		}
		time.Sleep(interval)
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

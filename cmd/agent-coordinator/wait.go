package main

import (
	"encoding/json"
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

// runWait blocks until the named agent has unread mail (exit 0) or the
// timeout passes (exit 1). Armed as a background task before delegating or
// idling, its exit is a harness touchpoint: the moment a DM arrives, the
// harness re-invokes the waiting agent.
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
	n, found := waitForMail(paths.Socket(), sc, name,
		time.Duration(*timeout)*time.Second, time.Duration(*interval)*time.Second)
	if !found {
		fmt.Println("timeout, no mail")
		os.Exit(1)
	}
	fmt.Printf("mail: %d unread - call read_messages\n", n)
}

// waitForMail polls the daemon's peek op until the named agent has unread
// mail or the deadline passes. A failed poll is just a miss - the daemon may
// be idle-restarting - so it keeps polling.
func waitForMail(socketPath, sc, name string, timeout, interval time.Duration) (int, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if n, ok := peekOnce(socketPath, sc, name); ok && n > 0 {
			return n, true
		}
		if time.Now().After(deadline) {
			return 0, false
		}
		time.Sleep(interval)
	}
}

func peekOnce(socketPath, sc, name string) (int, bool) {
	conn, err := dialer.Dial(socketPath, time.Second)
	if err != nil {
		return 0, false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	b, err := json.Marshal(protocol.Request{Op: protocol.OpPeek, Scope: sc, From: name})
	if err != nil {
		return 0, false
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return 0, false
	}
	var resp protocol.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return 0, false
	}
	return resp.Unread, resp.OK
}

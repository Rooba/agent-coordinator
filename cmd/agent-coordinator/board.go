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

// runBoard prints the workspace status board. Default hides gone agents;
// --all includes them. --json emits the agent list as JSON.
func runBoard(args []string) {
	fs := flag.NewFlagSet("board", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	live := fs.Bool("live", false, "only active and idle peers (same as list_agents)")
	all := fs.Bool("all", false, "include gone agents (default board hides them)")
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 || (*live && *all) {
		fmt.Fprintln(os.Stderr, "usage: agent-coordinator board [--live|--all] [--json]")
		os.Exit(2)
	}

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	sc := scope.Resolve(cwd)
	agents, err := fetchBoard(paths.Socket(), sc, *live, *all)
	if err != nil {
		fmt.Fprintf(os.Stderr, "board: %v\n", err)
		os.Exit(1)
	}
	if !*live && !*all {
		// Default: hide gone (and stale) so first contact is usable.
		filtered := agents[:0]
		for _, a := range agents {
			if a.Status == "active" || a.Status == "idle" {
				filtered = append(filtered, a)
			}
		}
		agents = filtered
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(agents); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if len(agents) == 0 {
		fmt.Println("no agents in this workspace")
		return
	}
	for _, a := range agents {
		line := fmt.Sprintf("%-16s %-8s id=%s", a.Name, a.Status, a.AgentID)
		if a.CurrentTask != "" {
			line += " task=" + a.CurrentTask
		} else if a.Activity != "" {
			line += " " + a.Activity
		}
		if a.TasksPending > 0 || a.TasksDone > 0 {
			line += fmt.Sprintf(" pending=%d done=%d", a.TasksPending, a.TasksDone)
		}
		if len(a.Files) > 0 {
			line += " files=" + strings.Join(a.Files, ",")
		}
		fmt.Println(line)
	}
}

// fetchBoard queries the daemon for the agent list. includeGone asks the
// daemon to keep gone rows (board --all); live asks for presence only.
func fetchBoard(socketPath, sc string, live, includeGone bool) ([]protocol.AgentInfo, error) {
	conn, err := dialer.Dial(socketPath, time.Second)
	if err != nil {
		return nil, fmt.Errorf("daemon unreachable: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	op := protocol.OpBoard
	if live {
		op = protocol.OpAgents
	}
	b, err := json.Marshal(protocol.Request{Op: op, Scope: sc, IncludeGone: includeGone})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	var resp protocol.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return resp.Agents, nil
}

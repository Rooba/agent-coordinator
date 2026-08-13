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

// runJoin registers this process in the workspace and prints the same name
// injection line SessionStart hooks emit. Use when a harness has no
// coordinator SessionStart hook (or as a one-shot bootstrap in a shell).
//
// Session id resolution order: -session-id flag, then common harness env
// vars, then a fresh ephemeral id (name will not stick across restarts).
func runJoin(args []string) {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	sessionFlag := fs.String("session-id", "", "stable session id (prefer harness env when available)")
	source := fs.String("source", "join", "registration source label shown in diagnostics")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: agent-coordinator join [-session-id <id>] [-source <label>]")
		os.Exit(2)
	}

	sessionID := strings.TrimSpace(*sessionFlag)
	if sessionID == "" {
		sessionID = sessionIDFromEnv()
	}
	if sessionID == "" {
		sessionID = fmt.Sprintf("join-%d-%d", os.Getpid(), time.Now().UnixNano())
	}

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	sc := scope.Resolve(cwd)
	conn, err := dialer.Dial(paths.Socket(), time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "join: daemon unreachable: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	req := protocol.Request{
		Op:        protocol.OpRegister,
		Scope:     sc,
		SessionID: sessionID,
		Source:    *source,
	}
	b, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "join: write: %v\n", err)
		os.Exit(1)
	}
	var resp protocol.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		fmt.Fprintf(os.Stderr, "join: decode: %v\n", err)
		os.Exit(1)
	}
	if !resp.OK || resp.Name == "" {
		errMsg := resp.Error
		if errMsg == "" {
			errMsg = "register failed"
		}
		fmt.Fprintf(os.Stderr, "join: %s\n", errMsg)
		os.Exit(1)
	}
	// Same injection line the SessionStart hook emits so harness context and
	// manual bootstrap stay interchangeable for agents.
	fmt.Printf("[coordinator] you are '%s' in this workspace. Peer tools (MCP agent-coordinator): status_board, list_agents, send_message, read_messages, broadcast. "+
		"To be wakeable while waiting or delegating, arm a background task first: agent-coordinator wait '%s' - it exits the moment new mail arrives and the harness re-invokes you.\n",
		resp.Name, resp.Name)
}

func sessionIDFromEnv() string {
	for _, k := range []string{
		"CLAUDE_CODE_SESSION_ID",
		"GROK_SESSION_ID",
		"CODEX_SESSION_ID",
		"AC_SESSION_ID",
	} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

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
	if err := doJoin(args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "join:", err)
		os.Exit(1)
	}
}

func doJoin(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sessionFlag := fs.String("session-id", "", "stable session id (prefer harness env when available)")
	source := fs.String("source", "join", "registration source label shown in diagnostics")
	kindFlag := fs.String("kind", "", "agent kind: eyes or launcher")
	scopeFlag := fs.String("scope", "", "workspace scope (default: resolve from cwd)")
	addrFlag := fs.String("addr", "", "unix://path or tcp://host:port (default AC_ADDR or unix socket)")
	credFile := fs.String("cred-file", "", "write/read session secret here (TCP; path only, never the secret)")
	platformFlag := fs.String("platform", "", "host platform advertised at register")
	capsFlag := fs.String("caps", "", "comma-separated capabilities (maps to JSON capabilities)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: agent-coordinator join [-session-id <id>] [-source <label>] [-kind eyes|launcher] [-scope <path>] [-addr tcp://host:port] [-cred-file PATH] [-platform windows] [-caps cap,cap]")
		return fmt.Errorf("usage")
	}

	sc := strings.TrimSpace(*scopeFlag)
	if sc == "" {
		sc = strings.TrimSpace(os.Getenv("AC_SCOPE"))
	}
	if sc == "" {
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}
		sc = scope.Resolve(cwd)
	}
	kind := strings.TrimSpace(*kindFlag)
	if kind == "" {
		kind = strings.TrimSpace(os.Getenv("AC_KIND"))
	}
	addr := strings.TrimSpace(*addrFlag)
	if addr == "" {
		addr = coordinatorAddr()
	}
	credPath := strings.TrimSpace(*credFile)
	if isTCPAddr(addr) && credPath == "" {
		return fmt.Errorf("tcp join requires -cred-file PATH (session secret is written there, never printed)")
	}
	var loaded sessionCred
	if credPath != "" {
		if c, err := loadCred(credPath); err == nil {
			loaded = c
		}
	}
	sessionID := strings.TrimSpace(*sessionFlag)
	if sessionID == "" {
		sessionID = sessionIDFromEnv()
	}
	if sessionID != "" && loaded.SessionID != "" && sessionID != loaded.SessionID {
		return fmt.Errorf("session id %q does not match cred-file id %q", sessionID, loaded.SessionID)
	}
	if sessionID == "" {
		sessionID = loaded.SessionID
	}
	if sessionID == "" {
		sessionID = fmt.Sprintf("join-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	secret := strings.TrimSpace(os.Getenv("AC_SESSION_SECRET"))
	if secret == "" {
		secret = loaded.SessionSecret
	}
	if isTCPAddr(addr) {
		if secret == "" {
			s, err := generateSessionSecret()
			if err != nil {
				return err
			}
			secret = s
		}
		if err := saveCred(credPath, sessionCred{SessionID: sessionID, SessionSecret: secret}); err != nil {
			return fmt.Errorf("write cred file: %w", err)
		}
	}
	req := protocol.Request{
		Op:        protocol.OpRegister,
		Scope:     sc,
		SessionID: sessionID,
		Source:    *source,
		Kind:      kind,
		Platform:  strings.TrimSpace(*platformFlag),
	}
	if caps := strings.TrimSpace(*capsFlag); caps != "" {
		req.Capabilities = splitCaps(caps)
	}
	if err := applyRelayAuth(&req, addr, credPath); err != nil {
		return err
	}
	if secret != "" {
		req.SessionSecret = secret
	}
	resp, err := once(addr, req)
	if err != nil || resp.Name == "" {
		errMsg := resp.Error
		if err != nil {
			errMsg = err.Error()
		}
		if errMsg == "" {
			errMsg = "register failed"
		}
		return fmt.Errorf("%s", errMsg)
	}
	fmt.Fprintf(stdout, "[coordinator] you are '%s' in this workspace. Peer tools (MCP agent-coordinator): status_board, list_agents, send_message, read_messages, broadcast. "+
		"To be wakeable while waiting or delegating, arm a background task first: agent-coordinator wait '%s' - it exits the moment new mail arrives and the harness re-invokes you.\n",
		resp.Name, resp.Name)
	return nil
}

func splitCaps(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
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

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/scope"
)

func cwdScope() string {
	if v := strings.TrimSpace(os.Getenv("AC_SCOPE")); v != "" {
		return v
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	return scope.Resolve(cwd)
}

func mustOnce(cmd string, req protocol.Request) protocol.Response {
	resp, err := once(unixSockAddr(), req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		os.Exit(1)
	}
	return resp
}

// readFileOrStdin returns FILE contents, or stdin when path is "-".
func readBody(literal, file string) (string, error) {
	if file != "" && literal != "" {
		return "", fmt.Errorf("use only one of -body and -body-file")
	}
	if file != "" {
		b, err := os.ReadFile(file)
		return string(b), err
	}
	if literal == "-" {
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	return literal, nil
}

func cliTarget(workspace, to, agentID string) *protocol.AgentRef {
	if workspace == "" && to == "" && agentID == "" {
		return nil
	}
	ref := &protocol.AgentRef{Scope: workspace, AgentID: agentID}
	if to != "" && ref.AgentID == "" {
		if len(to) == 12 {
			hex := true
			for i := 0; i < len(to); i++ {
				c := to[i]
				if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
					hex = false
					break
				}
			}
			if hex {
				ref.AgentID = to
			} else {
				ref.Name = to
			}
		} else {
			ref.Name = to
		}
	} else if to != "" {
		ref.Name = to
	}
	return ref
}

func emitJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runWorkspaces(args []string) {
	fs := flag.NewFlagSet("workspaces", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: agent-coordinator workspaces [--json]")
		os.Exit(2)
	}
	resp := mustOnce("workspaces", protocol.Request{Op: protocol.OpListWorkspaces, Scope: cwdScope()})
	if *asJSON {
		ws := resp.Workspaces
		if ws == nil {
			ws = []protocol.WorkspaceInfo{}
		}
		emitJSON(ws)
		return
	}
	if len(resp.Workspaces) == 0 {
		fmt.Println("no workspaces")
		return
	}
	for _, w := range resp.Workspaces {
		fmt.Printf("%s live=%d eyes=%d launchers=%d\n", w.Scope, w.LiveAgents, w.EyesAgents, w.LauncherAgents)
	}
}

func runEyes(args []string) {
	fs := flag.NewFlagSet("eyes", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: agent-coordinator eyes [--json]")
		os.Exit(2)
	}
	resp := mustOnce("eyes", protocol.Request{Op: protocol.OpListEyes, Scope: cwdScope()})
	if *asJSON {
		agents := resp.Agents
		if agents == nil {
			agents = []protocol.AgentInfo{}
		}
		emitJSON(agents)
		return
	}
	if len(resp.Agents) == 0 {
		fmt.Println("no eyes agents")
		return
	}
	for _, a := range resp.Agents {
		fmt.Printf("%-16s %-8s id=%s kind=%s scope=%s\n", a.Name, a.Status, a.AgentID, a.Kind, a.Scope)
	}
}

func runRelay(args []string) {
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	workspace := fs.String("workspace", "", "destination scope id")
	to := fs.String("to", "", "destination agent name or 12-hex agent_id")
	agentID := fs.String("agent-id", "", "destination agent_id")
	bodyFlag := fs.String("body", "", "literal body, or - for stdin")
	bodyFile := fs.String("body-file", "", "read body from file")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 || (*bodyFlag == "" && *bodyFile == "") {
		fmt.Fprintln(os.Stderr, "usage: agent-coordinator relay [-workspace SCOPE] [-to NAME] [-agent-id ID] (-body TEXT|- | -body-file FILE)")
		os.Exit(2)
	}
	sid, err := callerSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		os.Exit(1)
	}
	body, err := readBody(*bodyFlag, *bodyFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		os.Exit(1)
	}
	req := protocol.Request{
		Op:        protocol.OpSendWorkspace,
		Scope:     cwdScope(),
		SessionID: sid,
		Body:      body,
		Target:    cliTarget(*workspace, *to, *agentID),
	}
	mustOnce("relay", req)
	fmt.Println("ok")
}

func runSummon(args []string) {
	fs := flag.NewFlagSet("request-eyes", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	workspace := fs.String("workspace", "", "destination scope id")
	runtime := fs.String("runtime", "", "host provider: claude, codex, or grok")
	briefFlag := fs.String("brief", "", "literal brief, or - for stdin")
	briefFile := fs.String("brief-file", "", "read brief from file")
	timeout := fs.Int("timeout", 0, "deadline seconds, 300..1800")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 || (*briefFlag == "" && *briefFile == "") {
		fmt.Fprintln(os.Stderr, "usage: agent-coordinator request-eyes|summon [-workspace SCOPE] [-runtime claude|codex|grok] [-timeout N] (-brief TEXT|- | -brief-file FILE)")
		os.Exit(2)
	}
	if *timeout != 0 && (*timeout < 300 || *timeout > 1800) {
		fmt.Fprintln(os.Stderr, "request-eyes: -timeout must be between 300 and 1800")
		os.Exit(2)
	}
	sid, err := callerSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "request-eyes: %v\n", err)
		os.Exit(1)
	}
	brief, err := readBody(*briefFlag, *briefFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "request-eyes: %v\n", err)
		os.Exit(1)
	}
	req := protocol.Request{
		Op:        protocol.OpRequestEyes,
		Scope:     cwdScope(),
		SessionID: sid,
		Runtime:   *runtime,
		Brief:     brief,
		DeadlineS: *timeout,
		Target:    cliTarget(*workspace, "", ""),
	}
	resp := mustOnce("request-eyes", req)
	if resp.TaskID == "" {
		fmt.Fprintln(os.Stderr, "request-eyes: daemon returned no task_id")
		os.Exit(1)
	}
	line := "task_id=" + resp.TaskID
	if resp.Launcher != nil && resp.Launcher.Name != "" {
		line += " launcher=" + resp.Launcher.Name
	}
	if w, err := once(unixSockAddr(), protocol.Request{Op: protocol.OpWhoami, Scope: cwdScope(), SessionID: sid}); err == nil && w.Name != "" {
		line += "\narm: agent-coordinator wait '" + w.Name + "'"
	}
	fmt.Println(line)
}

func runCancelEyes(args []string) {
	fs := flag.NewFlagSet("cancel-eyes", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: agent-coordinator cancel-eyes <task_id>")
		os.Exit(2)
	}
	sid, err := callerSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cancel-eyes: %v\n", err)
		os.Exit(1)
	}
	taskID := fs.Arg(0)
	mustOnce("cancel-eyes", protocol.Request{
		Op:        protocol.OpCancelEyes,
		Scope:     cwdScope(),
		SessionID: sid,
		TaskID:    taskID,
	})
	fmt.Println("cancelled " + taskID)
}

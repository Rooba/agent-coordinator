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
	tasks := fs.Bool("tasks", false, "list the eyes task ledger instead of the eyes agents")
	mine := fs.Bool("mine", false, "with --tasks, list only the tasks this workspace session requested")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: agent-coordinator eyes [--tasks] [--mine] [--json]")
		os.Exit(2)
	}
	if *tasks {
		runEyesTasks(*asJSON, *mine)
		return
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

// runEyesTasks lists the eyes task ledger for this workspace, still-running
// tasks first, so somebody watching a brief that has gone quiet can see
// whether it is alive and how long it has left. --mine narrows it to the
// tasks this session asked for.
func runEyesTasks(asJSON, mine bool) {
	resp := mustOnce("eyes", protocol.Request{Op: protocol.OpListEyesTasks, Scope: cwdScope(), Mine: mine})
	if asJSON {
		tasks := resp.EyesTasks
		if tasks == nil {
			tasks = []protocol.EyesTaskInfo{}
		}
		emitJSON(tasks)
		return
	}
	if len(resp.EyesTasks) == 0 {
		fmt.Println("no eyes tasks")
		return
	}
	for _, t := range resp.EyesTasks {
		fmt.Println(eyesTaskLine(t))
	}
}

// eyesTaskLine is one ledger row as a human reads it: what it is, how long it
// has been going, how long it has left, and whether it is still moving.
func eyesTaskLine(t protocol.EyesTaskInfo) string {
	return fmt.Sprintf("%-20s %-7s %-9s elapsed=%-8s %-20s %-28s %s@%s",
		t.TaskID, t.Runtime, t.State, humanDur(t.ElapsedS), eyesRemaining(t), eyesProgress(t),
		t.RequesterAgentID, t.RequesterScope)
}

// eyesLiveStates are the states a task is still supposed to be running in, so
// a deadline still means something to it.
var eyesLiveStates = map[string]bool{"queued": true, "accepted": true}

// eyesRemaining is the deadline cell. An overdue task says so in words rather
// than as a negative number: its deadline passed with no completion or
// cancellation confirmed, so nobody can say whether it is still running.
func eyesRemaining(t protocol.EyesTaskInfo) string {
	switch {
	case t.Overdue:
		return "OVERDUE by " + humanDur(t.RemainingS)
	case !eyesLiveStates[t.State]:
		return "remaining=-"
	}
	return "remaining=" + humanDur(t.RemainingS)
}

// eyesProgress is the liveness cell: turns taken, the NAME of the tool last
// used, and how long ago the task last showed a sign of life. A task that has
// gone quiet says STALE, because a stalled task sends no mail and this is the
// only warning a reader gets.
func eyesProgress(t protocol.EyesTaskInfo) string {
	var cells []string
	if t.Turns > 0 {
		cells = append(cells, fmt.Sprintf("turns=%d", t.Turns))
	}
	if t.Tool != "" {
		cells = append(cells, "tool="+t.Tool)
	}
	switch {
	case t.HeartbeatAt == 0 && t.HeartbeatStale:
		cells = append(cells, "hb=STALE:never")
	case t.HeartbeatAt == 0:
		cells = append(cells, "hb=-")
	case t.HeartbeatStale:
		cells = append(cells, "hb=STALE:"+humanDur(t.HeartbeatAgeS))
	default:
		cells = append(cells, "hb="+humanDur(t.HeartbeatAgeS))
	}
	return strings.Join(cells, " ")
}

// humanDur renders a second count the way a waiting reader scans it.
func humanDur(s int) string {
	if s < 0 {
		s = -s
	}
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
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
	runtime := fs.String("runtime", "", "host provider: claude, codex, or grok")
	briefFlag := fs.String("brief", "", "literal brief, or - for stdin")
	briefFile := fs.String("brief-file", "", "read brief from file")
	timeout := fs.Int("timeout", 0, "deadline seconds, 300..1800")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 || (*briefFlag == "" && *briefFile == "") {
		fmt.Fprintln(os.Stderr, "usage: agent-coordinator request-eyes|summon [-runtime claude|codex|grok] [-timeout N] (-brief TEXT|- | -brief-file FILE)")
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

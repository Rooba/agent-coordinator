package hookcli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Rooba/agent-coordinator/internal/activity"
	"github.com/Rooba/agent-coordinator/internal/dialer"
	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/scope"
)

type hookInput struct {
	HookEventName string
	SessionID     string
	CWD           string
	ToolName      string
	ToolInput     map[string]any
	ToolResponse  json.RawMessage
	// Subagent tool calls arrive under the PARENT session_id with these two
	// extra fields set; parent-session events lack them.
	AgentID   string
	AgentType string
}

// parseHookInput accepts both envelope dialects: Claude snake_case and Grok
// camelCase (Grok says toolResult where Claude says tool_response).
func parseHookInput(raw []byte) (hookInput, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return hookInput{}, err
	}
	pick := func(dst any, keys ...string) {
		for _, k := range keys {
			if v, ok := m[k]; ok && json.Unmarshal(v, dst) == nil {
				return
			}
		}
	}
	var in hookInput
	pick(&in.HookEventName, "hook_event_name", "hookEventName")
	pick(&in.SessionID, "session_id", "sessionId")
	pick(&in.CWD, "cwd")
	pick(&in.ToolName, "tool_name", "toolName")
	pick(&in.ToolInput, "tool_input", "toolInput")
	pick(&in.ToolResponse, "tool_response", "toolResult")
	pick(&in.AgentID, "agent_id", "agentId")
	pick(&in.AgentType, "agent_type", "agentType")
	return in, nil
}

// canonicalEvent folds event-name variants (SessionStart, session_start,
// sessionStart, pre_tool_use, ...) onto the canonical Claude spelling.
var canonicalEvents = map[string]string{
	"sessionstart":     "SessionStart",
	"sessionend":       "SessionEnd",
	"stop":             "Stop",
	"userpromptsubmit": "UserPromptSubmit",
	"pretooluse":       "PreToolUse",
	"posttooluse":      "PostToolUse",
	"subagentstart":    "SubagentStart",
	"subagentstop":     "SubagentStop",
}

func canonicalEvent(name string) string {
	return canonicalEvents[strings.ToLower(strings.ReplaceAll(name, "_", ""))]
}

// Run processes one hook invocation. It never fails: every error path is a
// silent no-op so a broken coordinator cannot break the host agent session.
func Run(stdin io.Reader, stdout io.Writer, socketPath string) {
	defer func() { recover() }() // absolute fail-open backstop
	raw, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
	if err != nil {
		return
	}
	in, err := parseHookInput(raw)
	if err != nil || in.SessionID == "" {
		debugf("bad input: %v", err)
		return
	}
	sc := scope.Resolve(in.CWD)
	event := canonicalEvent(in.HookEventName)
	req := protocol.Request{Scope: sc, SessionID: in.SessionID}
	switch event {
	case "SessionStart":
		req.Op = protocol.OpRegister
		req.Source = "hook" // provenance for identity binding, not the harness's start reason
	case "SessionEnd":
		req.Op = protocol.OpDeregister
		removeBindFile()
	case "Stop":
		req.Op = protocol.OpIdle
		refreshBindFile(sc, in.SessionID)
	case "UserPromptSubmit":
		req.Op = protocol.OpEvent
		req.Tool = "UserPromptSubmit"
		req.Activity = "Handling user prompt"
		refreshBindFile(sc, in.SessionID)
	case "PreToolUse":
		preToolUse(in, sc, socketPath, stdout)
		return
	case "SubagentStart":
		if in.AgentID == "" {
			return
		}
		req.Op = protocol.OpRegister
		req.Source = "hook-subagent"
		req.AgentID, req.AgentType = in.AgentID, in.AgentType
	case "SubagentStop":
		if in.AgentID == "" { // never retire the parent by mistake
			return
		}
		req.Op = protocol.OpDeregister
		req.AgentID = in.AgentID
	case "PostToolUse":
		req.Op = protocol.OpEvent
		req.Tool = in.ToolName
		refreshBindFile(sc, in.SessionID)
		req.Activity, req.Files, req.Writes = activity.Infer(in.ToolName, in.ToolInput)
		req.Files = normalizePaths(in.CWD, req.Files)
		req.Writes = normalizePaths(in.CWD, req.Writes)
		// Subagent work is recorded under the CHILD row the daemon derives
		// from these fields, not tagged onto the parent.
		req.AgentID, req.AgentType = in.AgentID, in.AgentType
		req.TaskEv = activity.TaskSignal(in.ToolName, in.ToolInput, in.ToolResponse)
		if in.ToolName == "update_plan" {
			req.Tasks = activity.PlanSnapshot(in.ToolInput)
			req.ReplaceTasks = true
		}
	default:
		return
	}
	resp, ok := roundTrip(socketPath, req)
	if !ok || !resp.OK {
		debugf("daemon: ok=%v err=%s", ok, resp.Error)
		return
	}
	switch event {
	case "PostToolUse", "UserPromptSubmit":
		if len(resp.Notices) > 0 {
			emit(stdout, in.HookEventName, strings.Join(resp.Notices, "\n"))
		}
	case "Stop":
		// Blocking Stop-hook output: the reason is fed back to the model, so
		// pending mail notices wake the agent at turn end. The daemon marks
		// them noticed, so a repeat Stop returns none - no Stop loop.
		if len(resp.Notices) > 0 {
			if b, err := json.Marshal(map[string]string{
				"decision": "block", "reason": strings.Join(resp.Notices, "\n")}); err == nil {
				stdout.Write(b)
			}
		}
	case "SessionStart":
		writeBindFile(sc, in.SessionID, resp.Name)
		if resp.Name != "" {
			emit(stdout, "SessionStart", fmt.Sprintf(
				"[coordinator] you are '%s' in this workspace. Peer tools (MCP agent-coordinator): status_board, list_agents, send_message, read_messages, broadcast. "+
					"To be wakeable while waiting or delegating, arm a background task first: agent-coordinator wait '%s' - it exits the moment a DM arrives and the harness re-invokes you.",
				resp.Name, resp.Name))
		}
	}
}

// preToolUse guards coordinator identity tools for subagents, which share the
// parent's MCP connection: bare read_messages (or one aimed at the parent's
// name) would drain the PARENT inbox, and whoami reports the parent identity.
// Deny those with the child's own name; everything else - parent events, other
// tools, reads of any non-parent inbox, any resolution error - is allowed by
// emitting nothing.
func preToolUse(in hookInput, sc, socketPath string, stdout io.Writer) {
	if in.AgentID == "" {
		return // parent events are never denied
	}
	whoami, _ := coordToolCall(in.ToolName, in.ToolInput, "whoami")
	read, from := coordToolCall(in.ToolName, in.ToolInput, "read_messages")
	if !whoami && !read {
		return
	}
	if read && from != "" && from != parentName(sc, in.SessionID, socketPath) {
		return
	}
	resp, ok := roundTrip(socketPath, protocol.Request{
		Op: protocol.OpRegister, Scope: sc, SessionID: in.SessionID,
		Source: "hook-subagent", AgentID: in.AgentID, AgentType: in.AgentType,
	})
	if !ok || !resp.OK || resp.Name == "" {
		return // fail-open: coordinator trouble must never block the host agent
	}
	reason := fmt.Sprintf(
		"subagents share the parent MCP connection: retry with from='%s' (your own inbox). Parent mail stays with the parent.", resp.Name)
	if whoami || from != "" {
		reason = fmt.Sprintf("you are '%s' in this workspace; use from='%s' on coordinator tools.", resp.Name, resp.Name)
		if whoami {
			reason += " whoami on this shared connection reports the parent identity."
		} else {
			reason += fmt.Sprintf(" from='%s' is the parent's inbox, not yours.", from)
		}
	}
	out := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": reason,
	}}
	if b, err := json.Marshal(out); err == nil {
		stdout.Write(b)
	}
}

// parentName resolves the parent session's registered name; empty on any
// error so callers stay fail-open.
func parentName(sc, sessionID, socketPath string) string {
	resp, ok := roundTrip(socketPath, protocol.Request{
		Op: protocol.OpWhoami, Scope: sc, SessionID: sessionID})
	if !ok || !resp.OK {
		return ""
	}
	return resp.Name
}

// coordToolCall reports whether a tool call targets the named coordinator tool
// - the MCP name, the hookless alias, or a generic use_tool wrapper naming it -
// and the 'from' argument it carries (looking inside the wrapper's nested args).
func coordToolCall(tool string, input map[string]any, want string) (match bool, from string) {
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	switch tool {
	case "mcp__agent-coordinator__" + want, "agent-coordinator__" + want:
		return true, str(input, "from")
	case "use_tool":
		if n := str(input, "name"); n != want && !strings.HasSuffix(n, "__"+want) {
			return false, ""
		}
		if f := str(input, "from"); f != "" {
			return true, f
		}
		for _, k := range []string{"args", "arguments"} {
			if nested, ok := input[k].(map[string]any); ok {
				if f := str(nested, "from"); f != "" {
					return true, f
				}
			}
		}
		return true, ""
	}
	return false, ""
}

func normalizePaths(cwd string, paths []string) []string {
	for i, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		paths[i] = filepath.Clean(path)
	}
	return paths
}

func roundTrip(socketPath string, req protocol.Request) (protocol.Response, bool) {
	conn, err := dialer.Dial(socketPath, 150*time.Millisecond)
	if err != nil {
		debugf("dial: %v", err)
		return protocol.Response{}, false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	b, err := json.Marshal(req)
	if err != nil {
		return protocol.Response{}, false
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return protocol.Response{}, false
	}
	dec := json.NewDecoder(conn)
	var resp protocol.Response
	if err := dec.Decode(&resp); err != nil {
		debugf("decode: %v", err)
		return protocol.Response{}, false
	}
	return resp, true
}

func emit(w io.Writer, event, context string) {
	out := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": event, "additionalContext": context}}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	w.Write(b)
}

func debugf(format string, args ...any) {
	if os.Getenv("AC_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[ac-hook] "+format+"\n", args...)
	}
}

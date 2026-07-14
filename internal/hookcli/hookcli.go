package hookcli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Rooba/agent-coordinator/internal/activity"
	"github.com/Rooba/agent-coordinator/internal/dialer"
	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/scope"
)

type hookInput struct {
	HookEventName string          `json:"hook_event_name"`
	SessionID     string          `json:"session_id"`
	CWD           string          `json:"cwd"`
	Source        string          `json:"source"`
	ToolName      string          `json:"tool_name"`
	ToolInput     map[string]any  `json:"tool_input"`
	ToolResponse  json.RawMessage `json:"tool_response"`
	// Spike finding: subagent tool calls arrive under the PARENT session_id
	// with these two extra fields set; parent-session events lack them.
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
}

// Run processes one hook invocation. It never fails: every error path is a
// silent no-op so a broken coordinator cannot break a Claude session.
func Run(stdin io.Reader, stdout io.Writer, socketPath string) {
	defer func() { recover() }() // absolute fail-open backstop
	raw, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
	if err != nil {
		return
	}
	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil || in.SessionID == "" {
		debugf("bad input: %v", err)
		return
	}
	sc := scope.Resolve(in.CWD)
	req := protocol.Request{Scope: sc, SessionID: in.SessionID}
	switch in.HookEventName {
	case "SessionStart":
		req.Op = protocol.OpRegister
		req.Source = in.Source
	case "SessionEnd":
		req.Op = protocol.OpDeregister
	case "Stop":
		req.Op = protocol.OpIdle
	case "UserPromptSubmit":
		req.Op = protocol.OpEvent
		req.Tool = "UserPromptSubmit"
		req.Activity = "Handling user prompt"
	case "PostToolUse":
		req.Op = protocol.OpEvent
		req.Tool = in.ToolName
		req.Activity, req.Files, req.Writes = activity.Infer(in.ToolName, in.ToolInput)
		if in.AgentType != "" { // subagent work, tagged for the board
			req.Activity += " (subagent: " + in.AgentType + ")"
		}
		req.TaskEv = activity.TaskSignal(in.ToolName, in.ToolInput, in.ToolResponse)
	default:
		return
	}
	resp, ok := roundTrip(socketPath, req)
	if !ok || !resp.OK {
		debugf("daemon: ok=%v err=%s", ok, resp.Error)
		return
	}
	switch in.HookEventName {
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
		if resp.Name != "" {
			emit(stdout, "SessionStart", fmt.Sprintf(
				"[coordinator] you are '%s' in this workspace. Peer tools (MCP agent-coordinator): status_board, list_agents, send_message, read_messages, broadcast. "+
					"To be wakeable while waiting or delegating, arm a background task first: agent-coordinator wait '%s' - it exits the moment a DM arrives and the harness re-invokes you.",
				resp.Name, resp.Name))
		}
	}
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

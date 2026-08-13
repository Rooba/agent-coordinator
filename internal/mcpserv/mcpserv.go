package mcpserv

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Rooba/agent-coordinator/internal/dialer"
	"github.com/Rooba/agent-coordinator/internal/hookcli"
	"github.com/Rooba/agent-coordinator/internal/paths"
	"github.com/Rooba/agent-coordinator/internal/protocol"
	"github.com/Rooba/agent-coordinator/internal/scope"
)

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type callParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

const latestProtocolVersion = "2025-11-25"

var supportedProtocolVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
	"2025-11-25": true,
}

type server struct {
	scope      string
	socketPath string
	sessionID  string
	source     string
	name       string
	bound      bool // adopted a hook-registered identity; never deregister it
}

// Seams for tests: where bind files live and how our ancestry is read.
var (
	bindDirFn  = paths.BindDir
	ancestryFn = hookcli.Ancestry
)

func Serve(stdin io.Reader, stdout io.Writer, socketPath, cwd string) error {
	s := &server{
		scope:      scope.Resolve(cwd),
		socketPath: socketPath,
		sessionID:  newSessionID(),
		source:     "mcp",
	}
	defer s.deregister()

	// MCP stdio messages are newline-delimited and have no protocol size cap.
	r := bufio.NewReader(stdin)
	enc := json.NewEncoder(stdout)
	for {
		line, err := r.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var resp map[string]any
			var req rpcReq
			if json.Unmarshal(trimmed, &req) != nil {
				resp = map[string]any{"jsonrpc": "2.0", "id": nil,
					"error": map[string]any{"code": -32700, "message": "parse error"}}
			} else if req.ID != nil { // notifications get no response
				resp = map[string]any{"jsonrpc": "2.0", "id": req.ID}
				if result, rpcErr := s.handle(req); rpcErr != nil {
					resp["error"] = rpcErr
				} else {
					resp["result"] = result
				}
			}
			if resp != nil {
				if werr := enc.Encode(resp); werr != nil {
					return werr
				}
			}
		}
		if err == io.EOF {
			return nil // stdin closed: clean shutdown
		}
		if err != nil {
			return err
		}
	}
}

func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("mcp-%x", b[:])
	}
	return fmt.Sprintf("mcp-%d-%d", os.Getpid(), time.Now().UnixNano())
}

func negotiateProtocolVersion(requested string) string {
	if supportedProtocolVersions[requested] {
		return requested
	}
	return latestProtocolVersion
}

func (s *server) handle(req rpcReq) (any, map[string]any) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
			ClientInfo      struct {
				Name string `json:"name"`
			} `json:"clientInfo"`
		}
		json.Unmarshal(req.Params, &p)
		if p.ClientInfo.Name != "" {
			s.source = "mcp:" + p.ClientInfo.Name
		}
		return map[string]any{
			"protocolVersion": negotiateProtocolVersion(p.ProtocolVersion),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agent-coordinator", "version": "2.0.0"},
			"instructions": "Presence + messaging mesh for agents sharing this workspace. " +
				"Identity: if session-start context assigned a coordinator name ('you are <name>'), use it and do not call register_agent; otherwise call register_agent once. " +
				"Subagents share the parent connection and MUST pass from='<their child name>' on read_messages - a bare call drains the PARENT inbox. " +
				"Wake pattern while waiting on a peer: arm a BACKGROUND task `agent-coordinator wait '<your name>' -timeout 570`; it exits when a DM arrives, then call read_messages and re-arm if still waiting - never busy-poll. " +
				"Keep DMs short: write long content (plans, surveys, inventories) to a file under <repo>/.ignore/coordination/ and DM a one-line pointer to the path. " +
				"Collab recipe: agree ONE writer per file up front; before editing a shared hub file, check status_board and take it in the claims ledger (claim/release/list_claims - advisory, not a lock). " +
				"message_history audits your sent/received mail without clearing anything.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolDefs}, nil
	case "tools/call":
		var p callParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, map[string]any{"code": -32602, "message": "bad params"}
		}
		return s.callTool(p), nil
	default:
		return nil, map[string]any{"code": -32601, "message": "method not found: " + req.Method}
	}
}

func arg(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func (s *server) callTool(p callParams) map[string]any {
	req := protocol.Request{Scope: s.scope}
	switch p.Name {
	case "register_agent":
		name, err := s.ensureRegistered(true)
		if err != nil {
			return errResult(err.Error())
		}
		return textResult("registered as " + name)
	case "whoami":
		return s.whoami()
	case "status_board":
		req.Op = protocol.OpBoard
		req.IncludeGone, _ = p.Arguments["include_gone"].(bool)
	case "list_agents":
		req.Op = protocol.OpAgents
	case "send_message":
		req.Op = protocol.OpSend
		req.From, req.To, req.Body = arg(p.Arguments, "from"), arg(p.Arguments, "to"), arg(p.Arguments, "body")
	case "read_messages":
		req.Op = protocol.OpRead
		req.From = arg(p.Arguments, "from")
	case "peek_messages":
		req.Op = protocol.OpPeek
		req.From = arg(p.Arguments, "from")
		if after, ok := p.Arguments["after_id"].(float64); ok {
			req.AfterID = int64(after)
		}
	case "broadcast":
		req.Op = protocol.OpBroadcast
		req.From, req.Body = arg(p.Arguments, "from"), arg(p.Arguments, "body")
	case "claim":
		req.Op = protocol.OpClaim
		req.From, req.Path, req.Note = arg(p.Arguments, "from"), arg(p.Arguments, "path"), arg(p.Arguments, "note")
	case "release":
		req.Op = protocol.OpRelease
		req.From, req.Path = arg(p.Arguments, "from"), arg(p.Arguments, "path")
	case "list_claims":
		req.Op = protocol.OpClaims
	case "message_history":
		req.Op = protocol.OpHistory
		req.From, req.Peer = arg(p.Arguments, "from"), arg(p.Arguments, "peer")
		if l, ok := p.Arguments["limit"].(float64); ok {
			req.Limit = int(l)
		}
	default:
		return errResult("unknown tool " + p.Name)
	}
	// Caller-identity ops resolve a missing from exactly like messaging:
	// bind-or-register, fail closed when identity cannot be determined.
	if req.From == "" {
		switch req.Op {
		case protocol.OpSend, protocol.OpRead, protocol.OpPeek, protocol.OpBroadcast,
			protocol.OpClaim, protocol.OpRelease, protocol.OpHistory:
			name, err := s.ensureRegistered(false)
			if err != nil {
				return errResult(err.Error())
			}
			req.From = name
		}
	}
	if s.name != "" {
		req.SessionID = s.sessionID // heartbeat: every call keeps the bound row fresh
	}
	resp, err := roundTrip(s.socketPath, req)
	if err != nil {
		return errResult("coordinator daemon unreachable: " + err.Error())
	}
	if !resp.OK {
		return errResult(resp.Error)
	}
	var text string
	switch p.Name {
	case "status_board", "list_agents":
		b, _ := json.MarshalIndent(resp.Agents, "", "  ")
		text = string(b)
		if len(resp.Agents) == 0 {
			text = "no agents in this workspace"
		}
	case "read_messages":
		b, _ := json.MarshalIndent(resp.Messages, "", "  ")
		text = string(b)
		if len(resp.Messages) == 0 {
			text = "no unread messages"
		}
	case "peek_messages":
		ids, senders := resp.PeekIDs, resp.PeekFroms
		if ids == nil {
			ids = []int64{}
		}
		if senders == nil {
			senders = []string{}
		}
		b, _ := json.MarshalIndent(map[string]any{
			"unread": resp.Unread, "senders": senders, "ids": ids, "high_water": resp.HighWater}, "", "  ")
		text = string(b)
	case "claim":
		text = "claimed " + strings.TrimSpace(arg(p.Arguments, "path"))
		if len(resp.Notices) > 0 {
			text += " (" + strings.Join(resp.Notices, "; ") + ")"
		}
	case "release":
		text = "released " + strings.TrimSpace(arg(p.Arguments, "path"))
	case "list_claims":
		b, _ := json.MarshalIndent(resp.Claims, "", "  ")
		text = string(b)
		if len(resp.Claims) == 0 {
			text = "no claims in this workspace"
		}
	case "message_history":
		b, _ := json.MarshalIndent(resp.History, "", "  ")
		text = string(b)
		if len(resp.History) == 0 {
			text = "no message history"
		}
	default:
		text = "ok"
	}
	return textResult(text)
}

// ensureRegistered resolves this connection's identity. A bind file written by
// the session hook (matched by scope + pid ancestry) makes it adopt the hook
// row, so one session stays one inbox. While unbound, every call re-checks for
// a bind: the hook may write one after an early call minted a fallback name,
// and late adoption then retires the minted row and switches identity. Without
// a bind, an implicit call minting a fresh mcp- identity is allowed only when
// the scope has no live hook agents (the daemon enforces this); register_agent
// (explicit) always registers.
func (s *server) ensureRegistered(explicit bool) (string, error) {
	if !s.bound {
		if dir, err := bindDirFn(); err == nil {
			if b, ok := hookcli.MatchBind(dir, s.scope, ancestryFn()); ok {
				if s.name != "" {
					_, _ = roundTrip(s.socketPath, protocol.Request{
						Op: protocol.OpDeregister, Scope: s.scope, SessionID: s.sessionID})
				}
				s.sessionID, s.name, s.bound = b.SessionID, "", true
			}
		}
	}
	if s.name != "" {
		return s.name, nil
	}
	resp, err := roundTrip(s.socketPath, protocol.Request{
		Op:           protocol.OpRegister,
		Scope:        s.scope,
		SessionID:    s.sessionID,
		Source:       s.source,
		OnlyIfNoHook: !s.bound && !explicit,
	})
	if err != nil {
		return "", fmt.Errorf("coordinator daemon unreachable: %w", err)
	}
	if !resp.OK {
		return "", errors.New(resp.Error)
	}
	s.name = resp.Name
	return s.name, nil
}

func (s *server) whoami() map[string]any {
	if _, err := s.ensureRegistered(false); err != nil {
		return errResult(err.Error())
	}
	resp, err := roundTrip(s.socketPath, protocol.Request{
		Op: protocol.OpWhoami, Scope: s.scope, SessionID: s.sessionID})
	if err != nil {
		return errResult("coordinator daemon unreachable: " + err.Error())
	}
	if !resp.OK {
		return errResult(resp.Error)
	}
	out := map[string]any{"name": resp.Name, "agent_id": resp.AgentID, "scope": s.scope, "source": resp.Source}
	if resp.Parent != "" {
		out["parent"] = resp.Parent
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return textResult(string(b))
}

// deregister marks only a self-minted mcp- identity gone on stdin EOF; a
// hook-bound row belongs to the session, which outlives this process.
func (s *server) deregister() {
	if s.name == "" || s.bound {
		return
	}
	_, _ = roundTrip(s.socketPath, protocol.Request{
		Op:        protocol.OpDeregister,
		Scope:     s.scope,
		SessionID: s.sessionID,
	})
}

func textResult(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

func errResult(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}

func roundTrip(socketPath string, req protocol.Request) (protocol.Response, error) {
	conn, err := dialer.Dial(socketPath, time.Second)
	if err != nil {
		return protocol.Response{}, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	b, err := json.Marshal(req)
	if err != nil {
		return protocol.Response{}, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return protocol.Response{}, err
	}
	var resp protocol.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return protocol.Response{}, fmt.Errorf("decode: %w", err)
	}
	return resp, nil
}

var toolDefs = []map[string]any{
	{
		"name":        "register_agent",
		"description": "ONLY if no SessionStart-assigned name; call once. Registers this MCP session and returns its assigned agent name.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name":        "whoami",
		"description": "This connection's identity: name, agent_id, scope, source, and parent when running as a subagent.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name":        "status_board",
		"description": "Full detail on every agent in THIS workspace: agent_id, name, presence, current task, task counts, latest activity and files. Hides gone agents by default (include_gone=true for all).",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"include_gone": map[string]any{"type": "boolean", "description": "Also list agents whose presence has decayed to gone."}}},
	},
	{
		"name":        "list_agents",
		"description": "Live peers (active/idle) in this workspace, for picking who to contact. Presence only; use status_board for detail.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name":        "send_message",
		"description": "Send a direct message to one agent. Keep bodies short; for large content write a file and DM the path. 'to' accepts a name or agent_id; 'from' is optional for a bound identity.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"to", "body"}, "properties": map[string]any{
			"from": map[string]any{"type": "string"}, "to": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}}},
	},
	{
		"name":        "read_messages",
		"description": "DESTRUCTIVE: returns and CLEARS your unread messages. Subagents: pass from='<your child name>' - never bare (a bare call drains the parent inbox). Use peek_messages to look without clearing.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"from": map[string]any{"type": "string"}}},
	},
	{
		"name":        "peek_messages",
		"description": "Non-destructive preview of unread mail: count, senders, message ids, high-water id. Clears nothing. Optional after_id limits to messages with id greater than it.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"from":     map[string]any{"type": "string"},
			"after_id": map[string]any{"type": "number", "description": "Only count messages with id above this."}}},
	},
	{
		"name":        "broadcast",
		"description": "One-shot message to agents registered NOW; late joiners miss it; not a chatroom. DM critical directives instead. 'from' is optional for a bound identity.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"body"}, "properties": map[string]any{
			"from": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}}},
	},
	{
		"name":        "claim",
		"description": "Advisory coordination ledger, NOT a lock: claim a path or label before editing shared/hub files so peers see ownership. Held by a live agent fails with holder and note; a gone holder's stale claim is taken over. Re-claim your own path to refresh the note.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"path"}, "properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "File path or free-form label to claim."},
			"note": map[string]any{"type": "string", "description": "Why you hold it (shown to anyone who collides)."},
			"from": map[string]any{"type": "string"}}},
	},
	{
		"name":        "release",
		"description": "Release a claim you hold in the coordination ledger. Only the holder can release; releasing an unheld path is a no-op.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"path"}, "properties": map[string]any{
			"path": map[string]any{"type": "string"}, "from": map[string]any{"type": "string"}}},
	},
	{
		"name":        "list_claims",
		"description": "Every claim in this workspace's coordination ledger: path, holder name and agent_id, note, since.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name":        "message_history",
		"description": "Non-destructive audit of your sent and received mail incl. who read what and when; answers 'who ate my mail'. Newest first. Optional peer filters to exchanges with that agent; limit defaults to 20 (max 100).",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"peer":  map[string]any{"type": "string", "description": "Only exchanges with this agent (name or agent_id)."},
			"limit": map[string]any{"type": "number", "description": "Max rows, default 20, capped at 100."},
			"from":  map[string]any{"type": "string"}}},
	},
}

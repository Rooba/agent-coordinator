package mcpserv

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Rooba/agent-coordinator/internal/dialer"
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
}

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
			"instructions": "If session-start context assigned a coordinator name, use that name as from and do not call register_agent. " +
				"Otherwise call register_agent once; after that, from is optional for messaging tools.",
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
		name, err := s.ensureRegistered()
		if err != nil {
			return errResult("coordinator daemon unreachable: " + err.Error())
		}
		return textResult("registered as " + name)
	case "status_board":
		req.Op = protocol.OpBoard
	case "list_agents":
		req.Op = protocol.OpAgents
	case "send_message":
		req.Op = protocol.OpSend
		req.From, req.To, req.Body = arg(p.Arguments, "from"), arg(p.Arguments, "to"), arg(p.Arguments, "body")
	case "read_messages":
		req.Op = protocol.OpRead
		req.From = arg(p.Arguments, "from")
	case "broadcast":
		req.Op = protocol.OpBroadcast
		req.From, req.Body = arg(p.Arguments, "from"), arg(p.Arguments, "body")
	default:
		return errResult("unknown tool " + p.Name)
	}
	if req.From == "" && (req.Op == protocol.OpSend || req.Op == protocol.OpRead || req.Op == protocol.OpBroadcast) {
		name, err := s.ensureRegistered()
		if err != nil {
			return errResult("coordinator daemon unreachable: " + err.Error())
		}
		req.From = name
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
	default:
		text = "ok"
	}
	return textResult(text)
}

func (s *server) ensureRegistered() (string, error) {
	if s.name != "" {
		return s.name, nil
	}
	resp, err := roundTrip(s.socketPath, protocol.Request{
		Op:        protocol.OpRegister,
		Scope:     s.scope,
		SessionID: s.sessionID,
		Source:    s.source,
	})
	if err != nil {
		return "", err
	}
	if !resp.OK {
		return "", fmt.Errorf("register: %s", resp.Error)
	}
	s.name = resp.Name
	return s.name, nil
}

func (s *server) deregister() {
	if s.name == "" {
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
		"description": "Fallback for clients without a coordinator SessionStart hook: register this MCP session and return its assigned agent name. Do not call when session-start context already assigned a name.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name":        "status_board",
		"description": "Status board for THIS workspace: every coordinated agent with name, presence, current task, task counts, latest activity and files.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name":        "list_agents",
		"description": "List currently active/idle agents in this workspace (presence only).",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name":        "send_message",
		"description": "Send a direct message to another agent in this workspace. 'from' is optional after register_agent; 'to' is the recipient's name or agent_id.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"to", "body"}, "properties": map[string]any{
			"from": map[string]any{"type": "string"}, "to": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}}},
	},
	{
		"name":        "read_messages",
		"description": "Read and clear your unread messages. 'from' is optional after register_agent.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"from": map[string]any{"type": "string"}}},
	},
	{
		"name":        "broadcast",
		"description": "Global need-to-know channel for this workspace. 'from' is optional after register_agent. Every active agent gets notified. Use sparingly - not a chatroom.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"body"}, "properties": map[string]any{
			"from": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}}},
	},
}

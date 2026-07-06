package mcpserv

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/agent-coordinator/go/internal/protocol"
	"github.com/agent-coordinator/go/internal/scope"
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

func Serve(stdin io.Reader, stdout io.Writer, socketPath, cwd string) error {
	sc := scope.Resolve(cwd)
	// bufio.Reader instead of Scanner: the peer is the local Claude Code
	// process, so lines have no fixed cap and an oversized one must not
	// kill the session.
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
				if result, rpcErr := handle(req, sc, socketPath); rpcErr != nil {
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

func handle(req rpcReq, sc, socketPath string) (any, map[string]any) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agent-coordinator", "version": "2.0.0"},
		}, nil
	case "tools/list":
		return map[string]any{"tools": toolDefs}, nil
	case "tools/call":
		var p callParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, map[string]any{"code": -32602, "message": "bad params"}
		}
		return callTool(p, sc, socketPath), nil
	default:
		return nil, map[string]any{"code": -32601, "message": "method not found: " + req.Method}
	}
}

func arg(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func callTool(p callParams, sc, socketPath string) map[string]any {
	req := protocol.Request{Scope: sc}
	switch p.Name {
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
	resp, err := roundTrip(socketPath, req)
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
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

func errResult(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}

func roundTrip(socketPath string, req protocol.Request) (protocol.Response, error) {
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
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
		"description": "Send a direct message to another agent in this workspace. 'from' is YOUR agent name (given at session start); 'to' is the recipient's name or agent_id.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"from", "to", "body"}, "properties": map[string]any{
			"from": map[string]any{"type": "string"}, "to": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}}},
	},
	{
		"name":        "read_messages",
		"description": "Read and clear your unread messages. 'from' is YOUR agent name (given at session start).",
		"inputSchema": map[string]any{"type": "object", "required": []string{"from"}, "properties": map[string]any{
			"from": map[string]any{"type": "string"}}},
	},
	{
		"name":        "broadcast",
		"description": "Global need-to-know channel for this workspace. Every active agent gets notified. Use sparingly - not a chatroom.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"from", "body"}, "properties": map[string]any{
			"from": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}}},
	},
}

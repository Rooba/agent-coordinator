package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/agent-coordinator/go/internal/inbox"
	"github.com/agent-coordinator/go/internal/registry"
	"github.com/agent-coordinator/go/pkg/types"
	log "github.com/sirupsen/logrus"
)

// MCPServer implements the Model Context Protocol server
type MCPServer struct {
	agentRegistry    *registry.AgentRegistry
	taskRegistry     *registry.TaskRegistry
	codebaseRegistry *registry.CodebaseRegistry
	inboxManager     *inbox.InboxManager
	serverManager    *ServerManager
	logger           *log.Logger
}

// MCPRequest represents an MCP protocol request
type MCPRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *string     `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// MCPResponse represents an MCP protocol response
type MCPResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *string     `json:"id,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   *MCPError   `json:"error,omitempty"`
}

// MCPError represents an MCP protocol error
type MCPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Tool represents an MCP tool definition
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

// NewMCPServer creates a new MCP server
func NewMCPServer(
	agentRegistry *registry.AgentRegistry,
	taskRegistry *registry.TaskRegistry,
	codebaseRegistry *registry.CodebaseRegistry,
	inboxManager *inbox.InboxManager,
	logger *log.Logger,
) *MCPServer {
	if logger == nil {
		logger = log.New()
	}

	return &MCPServer{
		agentRegistry:    agentRegistry,
		taskRegistry:     taskRegistry,
		codebaseRegistry: codebaseRegistry,
		inboxManager:     inboxManager,
		serverManager:    nil, // Will be initialized later
		logger:           logger,
	}
}

// SetServerManager sets the server manager for handling external MCP servers
func (s *MCPServer) SetServerManager(sm *ServerManager) {
	s.serverManager = sm
}

// HandleRequest processes an MCP request and returns a response
func (s *MCPServer) HandleRequest(requestData []byte) ([]byte, error) {
	var request MCPRequest
	if err := json.Unmarshal(requestData, &request); err != nil {
		response := &MCPResponse{
			JSONRPC: "2.0",
			Error: &MCPError{
				Code:    -32700,
				Message: "Parse error",
			},
		}
		return json.Marshal(response)
	}

	response := s.processRequest(&request)
	return json.Marshal(response)
}

// processRequest handles the actual request processing
func (s *MCPServer) processRequest(request *MCPRequest) *MCPResponse {
	switch request.Method {
	case "initialize":
		return s.handleInitialize(request)
	case "tools/list":
		return s.handleToolsList(request)
	case "tools/call":
		return s.handleToolsCall(request)
	default:
		return &MCPResponse{
			JSONRPC: "2.0",
			ID:      request.ID,
			Error: &MCPError{
				Code:    -32601,
				Message: "Method not found",
			},
		}
	}
}

// handleInitialize handles the initialize method
func (s *MCPServer) handleInitialize(request *MCPRequest) *MCPResponse {
	return &MCPResponse{
		JSONRPC: "2.0",
		ID:      request.ID,
		Result: map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
			"serverInfo": map[string]interface{}{
				"name":    "agent-coordinator",
				"version": "0.1.0",
			},
		},
	}
}

// handleToolsList handles the tools/list method
func (s *MCPServer) handleToolsList(request *MCPRequest) *MCPResponse {
	var tools []Tool
	
	if s.serverManager != nil {
		// Get unified tools from server manager (includes external + coordinator tools)
		tools = s.serverManager.GetUnifiedTools()
	} else {
		// Fallback to just coordinator tools
		tools = s.getTools()
	}
	
	return &MCPResponse{
		JSONRPC: "2.0",
		ID:      request.ID,
		Result: map[string]interface{}{
			"tools": tools,
		},
	}
}

// handleToolsCall handles the tools/call method
func (s *MCPServer) handleToolsCall(request *MCPRequest) *MCPResponse {
	params, ok := request.Params.(map[string]interface{})
	if !ok {
		return &MCPResponse{
			JSONRPC: "2.0",
			ID:      request.ID,
			Error: &MCPError{
				Code:    -32602,
				Message: "Invalid params",
			},
		}
	}

	toolName, ok := params["name"].(string)
	if !ok {
		return &MCPResponse{
			JSONRPC: "2.0",
			ID:      request.ID,
			Error: &MCPError{
				Code:    -32602,
				Message: "Missing tool name",
			},
		}
	}

	arguments, ok := params["arguments"].(map[string]interface{})
	if !ok {
		arguments = make(map[string]interface{})
	}

	// Route tool call through server manager if available
	if s.serverManager != nil {
		result, err := s.serverManager.RouteToolCall(toolName, arguments)
		if err != nil {
			return &MCPResponse{
				JSONRPC: "2.0",
				ID:      request.ID,
				Error: &MCPError{
					Code:    -1,
					Message: err.Error(),
				},
			}
		}

		return &MCPResponse{
			JSONRPC: "2.0",
			ID:      request.ID,
			Result:  result,
		}
	}

	// Fallback to local tool handling
	result, err := s.callTool(toolName, arguments)
	if err != nil {
		return &MCPResponse{
			JSONRPC: "2.0",
			ID:      request.ID,
			Error: &MCPError{
				Code:    -1,
				Message: err.Error(),
			},
		}
	}

	return &MCPResponse{
		JSONRPC: "2.0",
		ID:      request.ID,
		Result: map[string]interface{}{
			"content": []map[string]interface{}{
				{
					"type": "text",
					"text": result,
				},
			},
		},
	}
}

// getTools returns the list of available tools
func (s *MCPServer) getTools() []Tool {
	return []Tool{
		{
			Name:        "register_agent",
			Description: "Register a new agent with the coordination system",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type": "string",
					},
					"capabilities": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "string",
							"enum": []string{"coding", "testing", "documentation", "analysis", "review"},
						},
					},
					"codebase_id": map[string]interface{}{
						"type": "string",
					},
					"workspace_path": map[string]interface{}{
						"type": "string",
					},
					"cross_codebase_capable": map[string]interface{}{
						"type": "boolean",
					},
				},
				"required": []string{"name", "capabilities"},
			},
		},
		{
			Name:        "create_task",
			Description: "Create a new task in the coordination system",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"title": map[string]interface{}{
						"type": "string",
					},
					"description": map[string]interface{}{
						"type": "string",
					},
					"priority": map[string]interface{}{
						"type": "string",
						"enum": []string{"low", "normal", "high", "urgent"},
					},
					"codebase_id": map[string]interface{}{
						"type": "string",
					},
					"file_paths": map[string]interface{}{
						"type":  "array",
						"items": map[string]interface{}{"type": "string"},
					},
					"required_capabilities": map[string]interface{}{
						"type":  "array",
						"items": map[string]interface{}{"type": "string"},
					},
				},
				"required": []string{"title", "description"},
			},
		},
		{
			Name:        "get_next_task",
			Description: "Get the next task for an agent",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"agent_id": map[string]interface{}{
						"type": "string",
					},
				},
				"required": []string{"agent_id"},
			},
		},
		{
			Name:        "complete_task",
			Description: "Mark current task as completed",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"agent_id": map[string]interface{}{
						"type": "string",
					},
				},
				"required": []string{"agent_id"},
			},
		},
		{
			Name:        "get_task_board",
			Description: "Get overview of all agents and their current tasks",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"codebase_id": map[string]interface{}{
						"type": "string",
					},
				},
			},
		},
		{
			Name:        "heartbeat",
			Description: "Send heartbeat to maintain agent status",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"agent_id": map[string]interface{}{
						"type": "string",
					},
				},
				"required": []string{"agent_id"},
			},
		},
		{
			Name:        "register_codebase",
			Description: "Register a new codebase in the coordination system",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type": "string",
					},
					"workspace_path": map[string]interface{}{
						"type": "string",
					},
					"description": map[string]interface{}{
						"type": "string",
					},
				},
				"required": []string{"name", "workspace_path"},
			},
		},
		{
			Name:        "unregister_agent",
			Description: "Unregister an agent from the coordination system",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"agent_id": map[string]interface{}{
						"type": "string",
					},
					"reason": map[string]interface{}{
						"type": "string",
					},
				},
				"required": []string{"agent_id"},
			},
		},
	}
}

// callTool executes a specific tool
func (s *MCPServer) callTool(toolName string, arguments map[string]interface{}) (string, error) {
	switch toolName {
	case "register_agent":
		return s.registerAgent(arguments)
	case "create_task":
		return s.createTask(arguments)
	case "get_next_task":
		return s.getNextTask(arguments)
	case "complete_task":
		return s.completeTask(arguments)
	case "get_task_board":
		return s.getTaskBoard(arguments)
	case "heartbeat":
		return s.heartbeat(arguments)
	case "register_codebase":
		return s.registerCodebase(arguments)
	case "unregister_agent":
		return s.unregisterAgent(arguments)
	default:
		return "", fmt.Errorf("unknown tool: %s", toolName)
	}
}

// Tool implementations

func (s *MCPServer) registerAgent(args map[string]interface{}) (string, error) {
	name, ok := args["name"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid 'name' parameter")
	}

	capabilitiesRaw, ok := args["capabilities"].([]interface{})
	if !ok {
		return "", fmt.Errorf("missing or invalid 'capabilities' parameter")
	}

	capabilities := make([]types.Capability, len(capabilitiesRaw))
	for i, cap := range capabilitiesRaw {
		capStr, ok := cap.(string)
		if !ok {
			return "", fmt.Errorf("invalid capability type")
		}
		capabilities[i] = types.Capability(capStr)
	}

	// Optional parameters
	var opts []types.AgentOption
	if codebaseID, ok := args["codebase_id"].(string); ok {
		opts = append(opts, types.WithCodebaseID(codebaseID))
	}
	if workspacePath, ok := args["workspace_path"].(string); ok {
		opts = append(opts, types.WithWorkspacePath(workspacePath))
	}
	if crossCodebaseCapable, ok := args["cross_codebase_capable"].(bool); ok {
		opts = append(opts, types.WithCrossCodebaseCapability(crossCodebaseCapable))
	}

	agent := types.NewAgent(name, capabilities, opts...)

	if err := s.agentRegistry.RegisterAgent(agent); err != nil {
		return "", fmt.Errorf("failed to register agent: %w", err)
	}

	// Create inbox for the agent
	s.inboxManager.CreateInbox(agent.ID)

	result := map[string]interface{}{
		"agent_id":    agent.ID,
		"codebase_id": agent.CodebaseID,
		"status":      "registered",
	}

	resultBytes, _ := json.Marshal(result)
	return string(resultBytes), nil
}

func (s *MCPServer) createTask(args map[string]interface{}) (string, error) {
	title, ok := args["title"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid 'title' parameter")
	}

	description, ok := args["description"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid 'description' parameter")
	}

	// Optional parameters
	var opts []types.TaskOption
	if priority, ok := args["priority"].(string); ok {
		opts = append(opts, types.WithPriority(types.Priority(priority)))
	}
	if codebaseID, ok := args["codebase_id"].(string); ok {
		opts = append(opts, types.WithTaskCodebaseID(codebaseID))
	}
	if filePathsRaw, ok := args["file_paths"].([]interface{}); ok {
		filePaths := make([]string, len(filePathsRaw))
		for i, path := range filePathsRaw {
			if pathStr, ok := path.(string); ok {
				filePaths[i] = pathStr
			}
		}
		opts = append(opts, types.WithFilePaths(filePaths))
	}
	if reqCapsRaw, ok := args["required_capabilities"].([]interface{}); ok {
		reqCaps := make([]string, len(reqCapsRaw))
		for i, cap := range reqCapsRaw {
			if capStr, ok := cap.(string); ok {
				reqCaps[i] = capStr
			}
		}
		opts = append(opts, types.WithRequiredCapabilities(reqCaps))
	}

	task := types.NewTask(title, description, opts...)

	if err := s.taskRegistry.CreateTask(task); err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	result := map[string]interface{}{
		"task_id":     task.ID,
		"codebase_id": task.CodebaseID,
		"status":      string(task.Status),
	}

	resultBytes, _ := json.Marshal(result)
	return string(resultBytes), nil
}

func (s *MCPServer) getNextTask(args map[string]interface{}) (string, error) {
	agentID, ok := args["agent_id"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid 'agent_id' parameter")
	}

	inbox, err := s.inboxManager.GetInbox(agentID)
	if err != nil {
		return "", fmt.Errorf("inbox not found for agent: %w", err)
	}

	task, err := inbox.GetNextTask()
	if err != nil {
		result := map[string]interface{}{
			"message": "No tasks available",
		}
		resultBytes, _ := json.Marshal(result)
		return string(resultBytes), nil
	}

	result := map[string]interface{}{
		"task_id":     task.ID,
		"title":       task.Title,
		"description": task.Description,
		"file_paths":  task.FilePaths,
		"priority":    string(task.Priority),
	}

	resultBytes, _ := json.Marshal(result)
	return string(resultBytes), nil
}

func (s *MCPServer) completeTask(args map[string]interface{}) (string, error) {
	agentID, ok := args["agent_id"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid 'agent_id' parameter")
	}

	inbox, err := s.inboxManager.GetInbox(agentID)
	if err != nil {
		return "", fmt.Errorf("inbox not found for agent: %w", err)
	}

	task, err := inbox.CompleteCurrentTask()
	if err != nil {
		return "", fmt.Errorf("failed to complete task: %w", err)
	}

	// Update task registry
	if err := s.taskRegistry.CompleteTask(task.ID, agentID); err != nil {
		s.logger.WithError(err).Error("Failed to update task registry")
	}

	result := map[string]interface{}{
		"task_id":      task.ID,
		"status":       "completed",
		"completed_at": task.UpdatedAt,
	}

	resultBytes, _ := json.Marshal(result)
	return string(resultBytes), nil
}

func (s *MCPServer) getTaskBoard(args map[string]interface{}) (string, error) {
	var codebaseFilter *string
	if codebaseID, ok := args["codebase_id"].(string); ok {
		codebaseFilter = &codebaseID
	}

	agents := s.agentRegistry.ListAgents()
	var filteredAgents []*types.Agent

	// Filter by codebase if specified
	if codebaseFilter != nil {
		for _, agent := range agents {
			if agent.CodebaseID == *codebaseFilter {
				filteredAgents = append(filteredAgents, agent)
			}
		}
	} else {
		filteredAgents = agents
	}

	var agentInfos []types.AgentInfo
	for _, agent := range filteredAgents {
		agentInfo := types.AgentInfo{
			AgentID:              agent.ID,
			Name:                 agent.Name,
			Capabilities:         agent.Capabilities,
			Status:               agent.Status,
			CodebaseID:           agent.CodebaseID,
			WorkspacePath:        agent.WorkspacePath,
			Online:               agent.IsOnline(),
			CrossCodebaseCapable: agent.CrossCodebaseCapable,
		}

		// Get inbox status
		if inbox, err := s.inboxManager.GetInbox(agent.ID); err == nil {
			status := inbox.GetStatus()
			agentInfo.PendingTasks = status.PendingCount
			agentInfo.CompletedTasks = status.CompletedCount

			if status.CurrentTask != nil {
				agentInfo.CurrentTask = &types.TaskSummary{
					ID:         status.CurrentTask.ID,
					Title:      status.CurrentTask.Title,
					CodebaseID: status.CurrentTask.CodebaseID,
				}
			}
		}

		agentInfos = append(agentInfos, agentInfo)
	}

	result := types.TaskBoard{
		Agents:         agentInfos,
		CodebaseFilter: codebaseFilter,
	}

	resultBytes, _ := json.Marshal(result)
	return string(resultBytes), nil
}

func (s *MCPServer) heartbeat(args map[string]interface{}) (string, error) {
	agentID, ok := args["agent_id"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid 'agent_id' parameter")
	}

	if err := s.agentRegistry.UpdateAgentHeartbeat(agentID); err != nil {
		return "", fmt.Errorf("heartbeat failed: %w", err)
	}

	result := map[string]interface{}{
		"status": "heartbeat_received",
	}

	resultBytes, _ := json.Marshal(result)
	return string(resultBytes), nil
}

func (s *MCPServer) registerCodebase(args map[string]interface{}) (string, error) {
	name, ok := args["name"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid 'name' parameter")
	}

	workspacePath, ok := args["workspace_path"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid 'workspace_path' parameter")
	}

	var opts []types.CodebaseOption
	if description, ok := args["description"].(string); ok {
		opts = append(opts, types.WithDescription(description))
	}

	codebase := types.NewCodebase(name, workspacePath, opts...)

	if err := s.codebaseRegistry.RegisterCodebase(codebase); err != nil {
		return "", fmt.Errorf("failed to register codebase: %w", err)
	}

	result := map[string]interface{}{
		"codebase_id": codebase.ID,
		"status":      "registered",
	}

	resultBytes, _ := json.Marshal(result)
	return string(resultBytes), nil
}

func (s *MCPServer) unregisterAgent(args map[string]interface{}) (string, error) {
	agentID, ok := args["agent_id"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid 'agent_id' parameter")
	}

	reason := "Agent unregistered"
	if reasonArg, ok := args["reason"].(string); ok {
		reason = reasonArg
	}

	if err := s.agentRegistry.UnregisterAgent(agentID, reason); err != nil {
		return "", fmt.Errorf("unregister failed: %w", err)
	}

	// Clean up inbox
	if err := s.inboxManager.DeleteInbox(agentID); err != nil {
		s.logger.WithError(err).Warn("Failed to clean up inbox after agent unregistration")
	}

	result := map[string]interface{}{
		"status":   "agent_unregistered",
		"agent_id": agentID,
		"reason":   reason,
	}

	resultBytes, _ := json.Marshal(result)
	return string(resultBytes), nil
}
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// ServerConfig represents configuration for an external MCP server
type ServerConfig struct {
	Type        string   `json:"type"`
	Command     string   `json:"command,omitempty"`
	Args        []string `json:"args,omitempty"`
	URL         string   `json:"url,omitempty"`
	AutoRestart bool     `json:"auto_restart"`
	Description string   `json:"description"`
}

// MCPServersConfig represents the full configuration file structure
type MCPServersConfig struct {
	Servers map[string]ServerConfig `json:"servers"`
	Config  struct {
		StartupTimeout      int `json:"startup_timeout"`
		HeartbeatInterval   int `json:"heartbeat_interval"`
		AutoRestartDelay    int `json:"auto_restart_delay"`
		MaxRestartAttempts  int `json:"max_restart_attempts"`
	} `json:"config"`
}

// ExternalServer represents a running external MCP server
type ExternalServer struct {
	Name        string
	Config      ServerConfig
	Process     *exec.Cmd
	Stdin       *bufio.Writer
	Stdout      *bufio.Scanner
	Tools       []Tool
	Running     bool
	StartedAt   time.Time
	mu          sync.RWMutex
}

// ServerManager manages external MCP servers
type ServerManager struct {
	servers map[string]*ExternalServer
	config  MCPServersConfig
	mu      sync.RWMutex
	logger  *log.Logger
}

// NewServerManager creates a new server manager
func NewServerManager(configFile string, logger *log.Logger) (*ServerManager, error) {
	if logger == nil {
		logger = log.New()
	}

	config, err := loadServerConfig(configFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load server config: %w", err)
	}

	return &ServerManager{
		servers: make(map[string]*ExternalServer),
		config:  config,
		logger:  logger,
	}, nil
}

// Start initializes and starts all configured MCP servers
func (sm *ServerManager) Start(ctx context.Context) error {
	sm.logger.Info("Starting external MCP servers...")

	var wg sync.WaitGroup
	for name, config := range sm.config.Servers {
		wg.Add(1)
		go func(serverName string, serverConfig ServerConfig) {
			defer wg.Done()
			
			if err := sm.startServer(ctx, serverName, serverConfig); err != nil {
				sm.logger.WithError(err).Errorf("Failed to start server %s", serverName)
			} else {
				sm.logger.Infof("Successfully started server %s", serverName)
			}
		}(name, config)
	}

	wg.Wait()
	sm.logger.Infof("Started %d external MCP servers", len(sm.servers))
	return nil
}

// Stop stops all running servers
func (sm *ServerManager) Stop() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for name, server := range sm.servers {
		sm.logger.Infof("Stopping server %s", name)
		sm.stopServer(server)
	}
	
	sm.servers = make(map[string]*ExternalServer)
}

// GetUnifiedTools returns all tools from all servers plus coordinator tools
func (sm *ServerManager) GetUnifiedTools() []Tool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var allTools []Tool
	
	// Add coordinator tools
	allTools = append(allTools, getCoordinatorTools()...)
	
	// Add tools from external servers
	for _, server := range sm.servers {
		server.mu.RLock()
		allTools = append(allTools, server.Tools...)
		server.mu.RUnlock()
	}

	return allTools
}

// RouteToolCall routes a tool call to the appropriate server
func (sm *ServerManager) RouteToolCall(toolName string, arguments map[string]interface{}) (map[string]interface{}, error) {
	// Check if it's a coordinator tool first
	if sm.isCoordinatorTool(toolName) {
		return sm.handleCoordinatorTool(toolName, arguments)
	}

	// Find the server that has this tool
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	
	for _, server := range sm.servers {
		server.mu.RLock()
		hasTool := false
		for _, tool := range server.Tools {
			if tool.Name == toolName {
				hasTool = true
				break
			}
		}
		server.mu.RUnlock()
		
		if hasTool {
			return sm.callExternalTool(server, toolName, arguments)
		}
	}

	return nil, fmt.Errorf("tool not found: %s", toolName)
}

// Private methods

func loadServerConfig(configFile string) (MCPServersConfig, error) {
	var config MCPServersConfig
	
	if configFile == "" {
		configFile = "mcp_servers.json"
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		return config, fmt.Errorf("failed to read config file %s: %w", configFile, err)
	}

	if err := json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("failed to parse config file: %w", err)
	}

	return config, nil
}

func (sm *ServerManager) startServer(ctx context.Context, name string, config ServerConfig) error {
	if config.Type != "stdio" {
		sm.logger.Warnf("Server %s has unsupported type %s, skipping", name, config.Type)
		return nil
	}

	server := &ExternalServer{
		Name:      name,
		Config:    config,
		StartedAt: time.Now(),
	}

	// Start the process
	cmd := exec.CommandContext(ctx, config.Command, config.Args...)
	cmd.Stderr = os.Stderr // Forward stderr for debugging

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start process: %w", err)
	}

	server.Process = cmd
	server.Stdin = bufio.NewWriter(stdin)
	server.Stdout = bufio.NewScanner(stdout)
	server.Running = true

	// Initialize the server
	if err := sm.initializeServer(server); err != nil {
		sm.stopServer(server)
		return fmt.Errorf("failed to initialize server: %w", err)
	}

	// Store the server
	sm.mu.Lock()
	sm.servers[name] = server
	sm.mu.Unlock()

	// Start a goroutine to monitor the process
	go sm.monitorServer(server)

	return nil
}

func (sm *ServerManager) initializeServer(server *ExternalServer) error {
	// Send initialize request
	initRequest := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{},
			"clientInfo": map[string]interface{}{
				"name":    "agent-coordinator",
				"version": "0.1.0",
			},
		},
	}

	if _, err := sm.sendRequest(server, initRequest); err != nil {
		return fmt.Errorf("initialize request failed: %w", err)
	}

	// Get tools list
	toolsRequest := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	}

	response, err := sm.sendRequest(server, toolsRequest)
	if err != nil {
		return fmt.Errorf("tools/list request failed: %w", err)
	}

	// Parse tools from response
	if result, ok := response["result"].(map[string]interface{}); ok {
		if toolsData, ok := result["tools"].([]interface{}); ok {
			var tools []Tool
			for _, toolData := range toolsData {
				if toolMap, ok := toolData.(map[string]interface{}); ok {
					tool := Tool{
						Name:        getString(toolMap, "name"),
						Description: getString(toolMap, "description"),
						InputSchema: toolMap["inputSchema"],
					}
					tools = append(tools, tool)
				}
			}
			server.mu.Lock()
			server.Tools = tools
			server.mu.Unlock()
		}
	}

	return nil
}

func (sm *ServerManager) sendRequest(server *ExternalServer, request map[string]interface{}) (map[string]interface{}, error) {
	reqJSON, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}

	// Send request
	if _, err := server.Stdin.WriteString(string(reqJSON) + "\n"); err != nil {
		return nil, err
	}
	if err := server.Stdin.Flush(); err != nil {
		return nil, err
	}

	// Read response with timeout
	responseCh := make(chan map[string]interface{}, 1)
	errorCh := make(chan error, 1)

	go func() {
		for server.Stdout.Scan() {
			line := strings.TrimSpace(server.Stdout.Text())
			if line == "" {
				continue
			}

			// Skip log lines, look for JSON
			if !strings.HasPrefix(line, "{") {
				continue
			}

			var response map[string]interface{}
			if err := json.Unmarshal([]byte(line), &response); err != nil {
				continue
			}

			responseCh <- response
			return
		}

		if err := server.Stdout.Err(); err != nil {
			errorCh <- err
		} else {
			errorCh <- fmt.Errorf("stdout closed")
		}
	}()

	select {
	case response := <-responseCh:
		return response, nil
	case err := <-errorCh:
		return nil, err
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("request timeout")
	}
}

func (sm *ServerManager) stopServer(server *ExternalServer) {
	server.mu.Lock()
	defer server.mu.Unlock()
	
	server.Running = false
	if server.Process != nil {
		server.Process.Process.Kill()
		server.Process.Wait()
	}
}

func (sm *ServerManager) monitorServer(server *ExternalServer) {
	server.Process.Wait()
	server.mu.Lock()
	server.Running = false
	server.mu.Unlock()
	
	sm.logger.Warnf("Server %s has stopped", server.Name)
}

func (sm *ServerManager) isCoordinatorTool(toolName string) bool {
	coordinatorTools := []string{
		"register_agent", "create_task", "get_next_task", "complete_task",
		"get_task_board", "heartbeat", "register_codebase", "unregister_agent",
	}
	
	for _, tool := range coordinatorTools {
		if tool == toolName {
			return true
		}
	}
	return false
}

func (sm *ServerManager) handleCoordinatorTool(toolName string, arguments map[string]interface{}) (map[string]interface{}, error) {
	// This would integrate with the existing MCPServer tool handling
	// For now, return a placeholder
	return map[string]interface{}{
		"content": []map[string]interface{}{
			{
				"type": "text",
				"text": fmt.Sprintf("Coordinator tool %s called with args %v", toolName, arguments),
			},
		},
	}, nil
}

func (sm *ServerManager) callExternalTool(server *ExternalServer, toolName string, arguments map[string]interface{}) (map[string]interface{}, error) {
	request := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      time.Now().UnixNano(),
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      toolName,
			"arguments": arguments,
		},
	}

	return sm.sendRequest(server, request)
}

func getCoordinatorTools() []Tool {
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
	}
}

func getString(m map[string]interface{}, key string) string {
	if val, ok := m[key].(string); ok {
		return val
	}
	return ""
}
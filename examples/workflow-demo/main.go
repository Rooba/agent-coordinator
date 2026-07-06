package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/agent-coordinator/go/internal/inbox"
	"github.com/agent-coordinator/go/internal/mcp"
	"github.com/agent-coordinator/go/internal/registry"
	"github.com/agent-coordinator/go/pkg/types"
	log "github.com/sirupsen/logrus"
)

func main() {
	fmt.Println("🚀 Agent Coordinator Workflow Demo")
	fmt.Println("===================================")

	// Create components
	logger := log.New()
	agentRegistry := registry.NewAgentRegistry(logger)
	codebaseRegistry := registry.NewCodebaseRegistry(logger)
	taskRegistry := registry.NewTaskRegistry(logger, agentRegistry)
	inboxManager := inbox.NewInboxManager(logger)

	// Set up callbacks
	agentRegistry.SetCallbacks(registry.AgentCallbacks{
		OnAgentRegistered: func(agent *types.Agent) error {
			fmt.Printf("✅ Agent registered: %s (%s)\n", agent.Name, agent.ID)
			// Create inbox for the agent
			inboxManager.CreateInbox(agent.ID)
			// Register a default codebase if it doesn't exist
			defaultCodebase := types.NewCodebase("default", "/tmp/default")
			defaultCodebase.ID = "default"
			codebaseRegistry.RegisterCodebase(defaultCodebase)
			return codebaseRegistry.AddAgentToCodebase(agent.CodebaseID, agent.ID)
		},
		OnAgentUnregistered: func(agentID, reason string) error {
			fmt.Printf("❌ Agent unregistered: %s (reason: %s)\n", agentID, reason)
			return nil
		},
	})

	taskRegistry.SetCallbacks(registry.TaskCallbacks{
		OnTaskAssigned: func(task *types.Task, agentID string) error {
			fmt.Printf("🔄 Task assigned: %s -> %s\n", task.Title, agentID)
			// Add task to agent's inbox
			if inbox, err := inboxManager.GetInbox(agentID); err == nil {
				inbox.AddTask(task)
			} else {
				// Create inbox if it doesn't exist
				inbox := inboxManager.CreateInbox(agentID)
				inbox.AddTask(task)
			}
			return nil
		},
		OnTaskCompleted: func(task *types.Task) error {
			fmt.Printf("✅ Task completed: %s\n", task.Title)
			return nil
		},
		OnTaskQueued: func(task *types.Task) error {
			fmt.Printf("📋 Task queued: %s (priority: %s)\n", task.Title, task.Priority)
			return nil
		},
	})

	// Create MCP server
	mcpServer := mcp.NewMCPServer(
		agentRegistry,
		taskRegistry,
		codebaseRegistry,
		inboxManager,
		logger,
	)

	// Demo workflow
	fmt.Println("\n1. Registering a codebase...")
	demoRegisterCodebase(mcpServer)

	fmt.Println("\n2. Registering agents...")
	agent1ID := demoRegisterAgent(mcpServer, "GitHub Copilot", []string{"coding", "testing"})
	agent2ID := demoRegisterAgent(mcpServer, "Claude Code", []string{"coding", "documentation", "analysis"})

	fmt.Println("\n3. Creating tasks...")
	_ = demoCreateTask(mcpServer, "Fix authentication bug", "Login fails on mobile devices", "high", []string{"auth.go", "login.go"})
	_ = demoCreateTask(mcpServer, "Add API documentation", "Document REST API endpoints", "normal", []string{"docs/api.md"})
	_ = demoCreateTask(mcpServer, "Write unit tests", "Add tests for user service", "normal", []string{"user_test.go"})

	fmt.Println("\n4. Checking task board...")
	demoGetTaskBoard(mcpServer, "")

	fmt.Println("\n5. Agents working on tasks...")
	demoAgentWorkflow(mcpServer, agent1ID, "GitHub Copilot")
	demoAgentWorkflow(mcpServer, agent2ID, "Claude Code")

	fmt.Println("\n6. Final task board...")
	demoGetTaskBoard(mcpServer, "")

	fmt.Println("\n✨ Demo completed successfully!")
}

func demoRegisterCodebase(server *mcp.MCPServer) {
	request := createMCPRequest("tools/call", map[string]interface{}{
		"name": "register_codebase",
		"arguments": map[string]interface{}{
			"name":           "My Web App",
			"workspace_path": "/home/user/my-web-app",
			"description":    "A modern web application",
		},
	})

	response := callMCPServer(server, request)
	fmt.Printf("   Codebase registered: %s\n", extractResultText(response))
}

func demoRegisterAgent(server *mcp.MCPServer, name string, capabilities []string) string {
	request := createMCPRequest("tools/call", map[string]interface{}{
		"name": "register_agent",
		"arguments": map[string]interface{}{
			"name":         name,
			"capabilities": capabilities,
			"codebase_id":  "default",
		},
	})

	response := callMCPServer(server, request)
	resultText := extractResultText(response)
	
	// Extract agent ID from response
	var result map[string]interface{}
	json.Unmarshal([]byte(resultText), &result)
	agentID := result["agent_id"].(string)
	
	fmt.Printf("   Agent registered: %s (ID: %s)\n", name, agentID)
	return agentID
}

func demoCreateTask(server *mcp.MCPServer, title, description, priority string, filePaths []string) string {
	request := createMCPRequest("tools/call", map[string]interface{}{
		"name": "create_task",
		"arguments": map[string]interface{}{
			"title":       title,
			"description": description,
			"priority":    priority,
			"file_paths":  filePaths,
			"codebase_id": "default",
		},
	})

	response := callMCPServer(server, request)
	resultText := extractResultText(response)
	
	// Extract task ID from response
	var result map[string]interface{}
	json.Unmarshal([]byte(resultText), &result)
	taskID := result["task_id"].(string)
	
	fmt.Printf("   Task created: %s (ID: %s)\n", title, taskID)
	return taskID
}

func demoGetTaskBoard(server *mcp.MCPServer, codebaseID string) {
	args := make(map[string]interface{})
	if codebaseID != "" {
		args["codebase_id"] = codebaseID
	}

	request := createMCPRequest("tools/call", map[string]interface{}{
		"name":      "get_task_board",
		"arguments": args,
	})

	response := callMCPServer(server, request)
	fmt.Printf("   Task board: %s\n", extractResultText(response))
}

func demoAgentWorkflow(server *mcp.MCPServer, agentID, agentName string) {
	fmt.Printf("   %s getting next task...\n", agentName)
	
	// Get next task
	request := createMCPRequest("tools/call", map[string]interface{}{
		"name": "get_next_task",
		"arguments": map[string]interface{}{
			"agent_id": agentID,
		},
	})

	response := callMCPServer(server, request)
	resultText := extractResultText(response)
	
	// Check if there's a task
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(resultText), &result); err != nil {
		fmt.Printf("   No tasks for %s\n", agentName)
		return
	}

	if message, ok := result["message"]; ok {
		fmt.Printf("   %s: %s\n", agentName, message)
		return
	}

	taskTitle := result["title"].(string)
	fmt.Printf("   %s working on: %s\n", agentName, taskTitle)

	// Simulate work
	time.Sleep(100 * time.Millisecond)

	// Send heartbeat
	heartbeatRequest := createMCPRequest("tools/call", map[string]interface{}{
		"name": "heartbeat",
		"arguments": map[string]interface{}{
			"agent_id": agentID,
		},
	})
	callMCPServer(server, heartbeatRequest)

	// Complete task
	completeRequest := createMCPRequest("tools/call", map[string]interface{}{
		"name": "complete_task",
		"arguments": map[string]interface{}{
			"agent_id": agentID,
		},
	})
	
	completeResponse := callMCPServer(server, completeRequest)
	fmt.Printf("   %s completed task: %s\n", agentName, extractResultText(completeResponse))
}

// Helper functions

func createMCPRequest(method string, params interface{}) map[string]interface{} {
	id := fmt.Sprintf("demo-%d", time.Now().UnixNano())
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
}

func callMCPServer(server *mcp.MCPServer, request map[string]interface{}) map[string]interface{} {
	requestBytes, _ := json.Marshal(request)
	responseBytes, err := server.HandleRequest(requestBytes)
	if err != nil {
		fmt.Printf("MCP request failed: %v\n", err)
		return make(map[string]interface{})
	}

	var response map[string]interface{}
	if err := json.Unmarshal(responseBytes, &response); err != nil {
		fmt.Printf("Failed to unmarshal response: %v\n", err)
		return make(map[string]interface{})
	}

	if errObj, ok := response["error"]; ok {
		fmt.Printf("MCP error: %v\n", errObj)
		return make(map[string]interface{})
	}

	return response
}

func extractResultText(response map[string]interface{}) string {
	result, ok := response["result"].(map[string]interface{})
	if !ok {
		return "No result"
	}

	content, ok := result["content"].([]interface{})
	if !ok || len(content) == 0 {
		return "No content"
	}

	firstContent, ok := content[0].(map[string]interface{})
	if !ok {
		return "Invalid content"
	}

	text, ok := firstContent["text"].(string)
	if !ok {
		return "No text"
	}

	return text
}
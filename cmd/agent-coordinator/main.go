package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agent-coordinator/go/internal/config"
	"github.com/agent-coordinator/go/internal/inbox"
	"github.com/agent-coordinator/go/internal/mcp"
	"github.com/agent-coordinator/go/internal/registry"
	"github.com/agent-coordinator/go/pkg/types"
	"github.com/spf13/cobra"
	log "github.com/sirupsen/logrus"
)

var (
	configFile  string
	logLevel    string
	port        string
	showVersion bool
	version     = "0.1.0"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.WithError(err).Fatal("Failed to execute command")
	}
}

var rootCmd = &cobra.Command{
	Use:   "agent-coordinator",
	Short: "Agent Coordinator - MCP server for coordinating AI agents",
	Long: `Agent Coordinator is a Model Context Protocol (MCP) server that enables 
multiple AI agents to coordinate their work seamlessly across codebases without conflicts.

Built with Go for easy deployment and broad accessibility.`,
	RunE: runServer,
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&configFile, "config", "c", "", "Path to configuration file")
	rootCmd.PersistentFlags().StringVarP(&logLevel, "log-level", "l", "", "Log level (debug, info, warn, error)")
	rootCmd.PersistentFlags().StringVarP(&port, "port", "p", "", "Server port")
	rootCmd.PersistentFlags().BoolVarP(&showVersion, "version", "v", false, "Show version information")

	rootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("Agent Coordinator %s\n", version)
		fmt.Printf("Built with Go for easy deployment and broad accessibility\n")
	},
}

func runServer(cmd *cobra.Command, args []string) error {
	// Show version if requested
	if showVersion {
		versionCmd.Run(cmd, args)
		return nil
	}

	// Load configuration
	cfg, err := config.LoadConfig(configFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Override with command line flags
	if logLevel != "" {
		cfg.Logging.Level = logLevel
	}
	if port != "" {
		cfg.Server.Port = port
	}

	// Configure logging
	if err := cfg.ConfigureLogging(); err != nil {
		return fmt.Errorf("failed to configure logging: %w", err)
	}

	log.WithFields(log.Fields{
		"version": version,
		"config":  cfg.ToMap(),
	}).Info("Starting Agent Coordinator")

	// Create registries and services
	agentRegistry := registry.NewAgentRegistry(log.StandardLogger())
	codebaseRegistry := registry.NewCodebaseRegistry(log.StandardLogger())
	taskRegistry := registry.NewTaskRegistry(log.StandardLogger(), agentRegistry)
	inboxManager := inbox.NewInboxManager(log.StandardLogger())

	// Set up callbacks
	agentRegistry.SetCallbacks(registry.AgentCallbacks{
		OnAgentRegistered: func(agent *types.Agent) error {
			return codebaseRegistry.AddAgentToCodebase(agent.CodebaseID, agent.ID)
		},
		OnAgentUnregistered: func(agentID, reason string) error {
			// Get agent before it's removed to get codebase ID
			if agent, err := agentRegistry.GetAgent(agentID); err == nil {
				codebaseRegistry.RemoveAgentFromCodebase(agent.CodebaseID, agentID)
			}
			return nil
		},
	})

	taskRegistry.SetCallbacks(registry.TaskCallbacks{
		OnTaskAssigned: func(task *types.Task, agentID string) error {
			// Add task to inbox
			if inbox, err := inboxManager.GetInbox(agentID); err == nil {
				inbox.AddTask(task)
			}
			return codebaseRegistry.AddTaskToCodebase(task.CodebaseID, task.ID)
		},
		OnTaskCompleted: func(task *types.Task) error {
			return codebaseRegistry.RemoveTaskFromCodebase(task.CodebaseID, task.ID)
		},
	})

	// Create server manager for external MCP servers
	serverManager, err := mcp.NewServerManager("mcp_servers.json", log.StandardLogger())
	if err != nil {
		return fmt.Errorf("failed to create server manager: %w", err)
	}

	// Start external MCP servers
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	
	if err := serverManager.Start(ctx); err != nil {
		return fmt.Errorf("failed to start external servers: %w", err)
	}
	defer serverManager.Stop()

	// Create MCP server
	mcpServer := mcp.NewMCPServer(
		agentRegistry,
		taskRegistry,
		codebaseRegistry,
		inboxManager,
		log.StandardLogger(),
	)
	
	// Connect server manager to MCP server
	mcpServer.SetServerManager(serverManager)

	// Start cleanup routine if enabled
	if cfg.Cleanup.Enabled {
		go startCleanupRoutine(cfg, agentRegistry, taskRegistry)
	}

	// Start MCP server (simplified stdio-based server for now)
	return startMCPServer(mcpServer, cfg)
}

func startCleanupRoutine(cfg *config.Config, agentRegistry *registry.AgentRegistry, taskRegistry *registry.TaskRegistry) {
	ticker := time.NewTicker(cfg.Cleanup.Interval)
	defer ticker.Stop()

	for range ticker.C {
		log.Debug("Running cleanup routine")

		// Clean up offline agents
		removedAgents := agentRegistry.Cleanup(cfg.Cleanup.AgentOfflineTimeout)
		if removedAgents > 0 {
			log.WithField("removed_count", removedAgents).Info("Cleaned up offline agents")
		}

		// Clean up old completed tasks
		removedTasks := taskRegistry.CleanupCompletedTasks(cfg.Cleanup.CompletedTaskMaxAge)
		if removedTasks > 0 {
			log.WithField("removed_count", removedTasks).Info("Cleaned up old completed tasks")
		}
	}
}

func startMCPServer(mcpServer *mcp.MCPServer, cfg *config.Config) error {
	log.WithField("address", cfg.GetListenAddress()).Info("Starting MCP server")

	// Set up signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start stdin/stdout based MCP server
	go func() {
		defer cancel()
		if err := runStdioMCPServer(mcpServer); err != nil {
			log.WithError(err).Error("MCP server error")
		}
	}()

	// Wait for shutdown signal
	select {
	case sig := <-sigChan:
		log.WithField("signal", sig).Info("Received shutdown signal")
	case <-ctx.Done():
		log.Info("Context cancelled, shutting down")
	}

	log.Info("Agent Coordinator shutting down")
	return nil
}

func runStdioMCPServer(mcpServer *mcp.MCPServer) error {
	// Scanner to read line-delimited JSON-RPC messages from stdin
	scanner := bufio.NewScanner(os.Stdin)
	
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		log.WithField("request", line).Debug("Received MCP request")

		// Process the JSON-RPC request
		response, err := mcpServer.HandleRequest([]byte(line))
		if err != nil {
			log.WithError(err).Error("Failed to handle MCP request")
			continue
		}

		// Write response to stdout with newline
		if _, err := os.Stdout.Write(response); err != nil {
			log.WithError(err).Error("Failed to write response to stdout")
			continue
		}
		os.Stdout.Write([]byte("\n"))
		
		// Flush stdout to ensure immediate delivery
		if f, ok := os.Stdout.(*os.File); ok {
			f.Sync()
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanner error: %w", err)
	}

	return nil
}
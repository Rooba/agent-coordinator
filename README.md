# Agent Coordinator (Go)

A **Model Context Protocol (MCP) server** that enables multiple AI agents to coordinate their work seamlessly across codebases without conflicts. Built with Go for easy deployment and broad accessibility.

## 🎯 What is Agent Coordinator?

Agent Coordinator is an MCP server that solves the problem of multiple AI agents stepping on each other's toes when working on the same codebase. Instead of agents conflicting over files or duplicating work, they can register with the coordinator, receive tasks, and collaborate intelligently.

**Key Features:**

- **🤖 Multi-Agent Coordination**: Register multiple AI agents (GitHub Copilot, Claude, etc.) with different capabilities
- **📝 Intelligent Task Distribution**: Automatically assigns tasks to agents based on their capabilities and availability
- **🔄 Cross-Codebase Support**: Coordinate work across multiple repositories and projects
- **⚡ Real-Time Communication**: Agents can communicate and share progress via heartbeat system
- **🎯 Smart Task Management**: Queue, prioritize, and track tasks with metadata and dependencies
- **🔌 MCP Standard Compliance**: Works with any MCP-compatible AI agent or tool
- **📦 Single Binary Deployment**: Easy deployment with no external dependencies

## 🚀 How It Works

```ascii
 Agent 1          Agent 2         Agent N
(Copilot)         (Claude)        (Custom)
     │               │               │
     └──────── MCP Protocol ─────────┘
                     │
      ┌─────────────────────────────┐
      │       Agent Coordinator     │
      ├─────────────────────────────┤
      │ ┌─────────────────────────┐ │
      │ │     Task Registry       │ │
      │ ├─────────────────────────┤ │
      │ │   • Task Queuing        │ │
      │ │   • Agent Matching      │ │
      │ │   • Priority Handling   │ │
      │ └─────────────────────────┘ │
      │ ┌─────────────────────────┐ │
      │ │     Agent Manager       │ │
      │ ├─────────────────────────┤ │
      │ │   • Registration        │ │
      │ │   • Heartbeat           │ │
      │ │   • Capability Match    │ │
      │ └─────────────────────────┘ │
      │ ┌─────────────────────────┐ │
      │ │     Codebase Registry   │ │
      │ ├─────────────────────────┤ │
      │ │   • Cross-Repo Tasks    │ │
      │ │   • Dependencies        │ │
      │ │   • Workspace Mgmt      │ │
      │ └─────────────────────────┘ │
      └─────────────────────────────┘
```
<!-- ᕦ(ò_óˇ)ᕤ -->

## 🛠️ Prerequisites

You need these installed to run Agent Coordinator:

- **Go**: 1.21+
- **Git**: For version control

## ⚡ Quick Start

### 1. Get the Code

```bash
git clone https://github.com/your-username/agent-coordinator-go.git
cd agent-coordinator-go
```

### 2. Build and Start the MCP Server

```bash
# Build the binary
go build -o agent-coordinator ./cmd/agent-coordinator

# Start the MCP server directly
./agent-coordinator

# Or with custom configuration
./agent-coordinator -config config.json
```

### 3. Configure Your AI Tools

The agent coordinator is designed to work with VS Code and AI tools that support MCP. Add this to your VS Code `settings.json`:

```json
{
  "github.copilot.advanced": {
    "mcp": {
      "servers": {
        "agent-coordinator": {
          "command": "/path/to/agent-coordinator-go/agent-coordinator",
          "args": [],
          "env": {
            "LOG_LEVEL": "info"
          }
        }
      }
    }
  }
}
```

### 4. Test It Works

```bash
# Run the demo to see it in action
go run ./examples/workflow-demo/main.go
```

## 🎮 How to Use

Once your AI agents are connected via MCP, they can:

### Register as an Agent

```bash
# An agent identifies itself with capabilities
register_agent("GitHub Copilot", ["coding", "testing"], codebase_id: "my-project")
```

### Create Tasks

```bash
# Tasks are created with requirements
create_task("Fix login bug", "Authentication fails on mobile",
  priority: "high",
  required_capabilities: ["coding", "debugging"]
)
```

### Coordinate Automatically

The coordinator automatically:

- **Matches** tasks to agents based on capabilities
- **Queues** tasks when no suitable agents are available
- **Tracks** agent heartbeats to ensure they're still working
- **Handles** cross-codebase tasks that span multiple repositories

### Available MCP Tools

All MCP-compatible AI agents get these tools automatically:

| Tool                         | Purpose                                |
| ---------------------------- | -------------------------------------- |
| `register_agent`             | Register an agent with capabilities    |
| `create_task`                | Create a new task with requirements    |
| `get_next_task`              | Get the next task assigned to an agent |
| `complete_task`              | Mark current task as completed         |
| `get_task_board`             | View all agents and their status       |
| `heartbeat`                  | Send agent heartbeat to stay active    |
| `register_codebase`          | Register a new codebase/repository     |
| `create_cross_codebase_task` | Create tasks spanning multiple repos   |

## 🧪 Development & Testing

### Running Tests

```bash
# Run all tests
go test ./...

# Run with coverage
go test -cover ./...

# Run specific package tests
go test ./internal/agent
```

### Code Quality

```bash
# Format code
go fmt ./...

# Run linter
golangci-lint run

# Generate documentation
go doc -all > docs/api.md
```

## 📁 Project Structure

```text
agent-coordinator-go/
├── cmd/
│   └── agent-coordinator/           # Main application entry point
├── internal/
│   ├── agent/                      # Agent management
│   ├── task/                       # Task data structures
│   ├── registry/                   # Task and codebase registries
│   ├── mcp/                        # MCP protocol implementation
│   ├── inbox/                      # Agent inbox management
│   └── config/                     # Configuration management
├── pkg/
│   └── types/                      # Public API types
├── examples/                       # Working examples and demos
├── scripts/                        # Build and deployment scripts
├── docs/                          # Documentation
└── tests/                         # Integration tests
```

## 🤔 Why This Design?

**The Problem**: Multiple AI agents working on the same codebase step on each other, duplicate work, or create conflicts.

**The Solution**: A coordination layer that:

- Lets agents register their capabilities
- Intelligently distributes tasks
- Tracks progress and prevents conflicts
- Scales across multiple repositories

**Why Go?**: Single binary deployment, excellent performance, great concurrency support, and broad platform compatibility.

## 🚀 Deployment

### Single Binary

```bash
# Build for current platform
go build -o agent-coordinator ./cmd/agent-coordinator

# Cross-compile for different platforms
GOOS=linux GOARCH=amd64 go build -o agent-coordinator-linux ./cmd/agent-coordinator
GOOS=windows GOARCH=amd64 go build -o agent-coordinator.exe ./cmd/agent-coordinator
GOOS=darwin GOARCH=amd64 go build -o agent-coordinator-mac ./cmd/agent-coordinator
```

### Docker

```bash
# Build Docker image
docker build -t agent-coordinator .

# Run with Docker
docker run -p 8080:8080 agent-coordinator
```

## 📄 Configuration

Agent Coordinator can be configured via:

1. **Command line flags**:
   ```bash
   ./agent-coordinator -port 8080 -log-level debug
   ```

2. **Environment variables**:
   ```bash
   export AGENT_COORDINATOR_PORT=8080
   export AGENT_COORDINATOR_LOG_LEVEL=debug
   ```

3. **Configuration file**:
   ```json
   {
     "server": {
       "port": 8080,
       "host": "localhost"
     },
     "logging": {
       "level": "info",
       "format": "json"
     },
     "features": {
       "cross_codebase": true,
       "persistence": false
     }
   }
   ```

## 🤝 Contributing

Contributions are welcome! Here's how:

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add some amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## 📄 License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## 🙏 Acknowledgments

- [Model Context Protocol](https://modelcontextprotocol.io/) for the agent communication standard
- [Go](https://golang.org/) community for the excellent ecosystem
- AI development teams pushing the boundaries of collaborative coding

---

**Agent Coordinator (Go)** - Making AI agents work together, not against each other.
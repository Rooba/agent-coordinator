package types

import (
	"time"

	"github.com/google/uuid"
)

// Agent represents an AI agent in the coordination system
type Agent struct {
	ID                    string            `json:"id"`
	Name                  string            `json:"name"`
	Capabilities          []Capability      `json:"capabilities"`
	Status                AgentStatus       `json:"status"`
	CurrentTaskID         *string           `json:"current_task_id,omitempty"`
	CodebaseID            string            `json:"codebase_id"`
	WorkspacePath         *string           `json:"workspace_path,omitempty"`
	LastHeartbeat         time.Time         `json:"last_heartbeat"`
	Metadata              map[string]any    `json:"metadata"`
	CrossCodebaseCapable  bool              `json:"cross_codebase_capable"`
}

// Task represents a task in the coordination system
type Task struct {
	ID                        string                 `json:"id"`
	Title                     string                 `json:"title"`
	Description               string                 `json:"description"`
	Status                    TaskStatus             `json:"status"`
	Priority                  Priority               `json:"priority"`
	AgentID                   *string                `json:"agent_id,omitempty"`
	CodebaseID                string                 `json:"codebase_id"`
	FilePaths                 []string               `json:"file_paths"`
	Dependencies              []string               `json:"dependencies"`
	CrossCodebaseDependencies []CrossCodebaseDep     `json:"cross_codebase_dependencies"`
	CreatedAt                 time.Time              `json:"created_at"`
	UpdatedAt                 time.Time              `json:"updated_at"`
	Metadata                  map[string]any         `json:"metadata"`
}

// Codebase represents a codebase/repository in the system
type Codebase struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	WorkspacePath string            `json:"workspace_path"`
	Description   *string           `json:"description,omitempty"`
	Metadata      map[string]any    `json:"metadata"`
	Agents        []string          `json:"agents"`        // Agent IDs
	ActiveTasks   []string          `json:"active_tasks"`  // Task IDs
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

// CrossCodebaseDep represents a cross-codebase dependency
type CrossCodebaseDep struct {
	CodebaseID string `json:"codebase_id"`
	TaskID     string `json:"task_id"`
}

// InboxStatus represents the status of an agent's inbox
type InboxStatus struct {
	AgentID       string  `json:"agent_id"`
	CurrentTask   *Task   `json:"current_task,omitempty"`
	PendingCount  int     `json:"pending_count"`
	CompletedCount int    `json:"completed_count"`
}

// TaskBoard represents the overall status of all agents and tasks
type TaskBoard struct {
	Agents        []AgentInfo `json:"agents"`
	CodebaseFilter *string    `json:"codebase_filter,omitempty"`
}

// AgentInfo represents agent information for the task board
type AgentInfo struct {
	AgentID                string  `json:"agent_id"`
	Name                   string  `json:"name"`
	Capabilities           []Capability `json:"capabilities"`
	Status                 AgentStatus  `json:"status"`
	CodebaseID             string  `json:"codebase_id"`
	WorkspacePath          *string `json:"workspace_path,omitempty"`
	Online                 bool    `json:"online"`
	CrossCodebaseCapable   bool    `json:"cross_codebase_capable"`
	CurrentTask           *TaskSummary `json:"current_task,omitempty"`
	PendingTasks          int     `json:"pending_tasks"`
	CompletedTasks        int     `json:"completed_tasks"`
}

// TaskSummary represents a summary of task information
type TaskSummary struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	CodebaseID string `json:"codebase_id"`
}

// CodebaseStats represents statistics for a codebase
type CodebaseStats struct {
	CodebaseID       string `json:"codebase_id"`
	Name             string `json:"name"`
	ActiveAgents     int    `json:"active_agents"`
	PendingTasks     int    `json:"pending_tasks"`
	ActiveTasks      int    `json:"active_tasks"`
	CompletedTasks   int    `json:"completed_tasks"`
}

// Enums

type AgentStatus string

const (
	AgentStatusIdle    AgentStatus = "idle"
	AgentStatusBusy    AgentStatus = "busy"
	AgentStatusOffline AgentStatus = "offline"
	AgentStatusError   AgentStatus = "error"
)

type TaskStatus string

const (
	TaskStatusPending    TaskStatus = "pending"
	TaskStatusInProgress TaskStatus = "in_progress"
	TaskStatusCompleted  TaskStatus = "completed"
	TaskStatusFailed     TaskStatus = "failed"
	TaskStatusBlocked    TaskStatus = "blocked"
)

type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
	PriorityUrgent Priority = "urgent"
)

type Capability string

const (
	CapabilityCoding        Capability = "coding"
	CapabilityTesting       Capability = "testing"
	CapabilityDocumentation Capability = "documentation"
	CapabilityAnalysis      Capability = "analysis"
	CapabilityReview        Capability = "review"
)

// Helper functions

// NewAgent creates a new Agent with default values
func NewAgent(name string, capabilities []Capability, opts ...AgentOption) *Agent {
	agent := &Agent{
		ID:                   uuid.New().String(),
		Name:                 name,
		Capabilities:         capabilities,
		Status:               AgentStatusIdle,
		CodebaseID:           "default",
		LastHeartbeat:        time.Now(),
		Metadata:             make(map[string]any),
		CrossCodebaseCapable: false,
	}

	for _, opt := range opts {
		opt(agent)
	}

	return agent
}

// NewTask creates a new Task with default values
func NewTask(title, description string, opts ...TaskOption) *Task {
	now := time.Now()
	task := &Task{
		ID:                        uuid.New().String(),
		Title:                     title,
		Description:               description,
		Status:                    TaskStatusPending,
		Priority:                  PriorityNormal,
		CodebaseID:                "default",
		FilePaths:                 []string{},
		Dependencies:              []string{},
		CrossCodebaseDependencies: []CrossCodebaseDep{},
		CreatedAt:                 now,
		UpdatedAt:                 now,
		Metadata:                  make(map[string]any),
	}

	for _, opt := range opts {
		opt(task)
	}

	return task
}

// NewCodebase creates a new Codebase with default values
func NewCodebase(name, workspacePath string, opts ...CodebaseOption) *Codebase {
	now := time.Now()
	codebase := &Codebase{
		ID:            uuid.New().String(),
		Name:          name,
		WorkspacePath: workspacePath,
		Metadata:      make(map[string]any),
		Agents:        []string{},
		ActiveTasks:   []string{},
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	for _, opt := range opts {
		opt(codebase)
	}

	return codebase
}

// Agent methods

// IsOnline checks if the agent is considered online based on heartbeat
func (a *Agent) IsOnline() bool {
	return time.Since(a.LastHeartbeat) < 30*time.Second
}

// CanHandle checks if the agent can handle a given task
func (a *Agent) CanHandle(task *Task) bool {
	// Check codebase compatibility
	codebaseCompatible := a.CodebaseID == task.CodebaseID || a.CrossCodebaseCapable

	// Check capability requirements
	if requiredCaps, ok := task.Metadata["required_capabilities"].([]string); ok && len(requiredCaps) > 0 {
		capabilityMatch := false
		for _, reqCap := range requiredCaps {
			for _, agentCap := range a.Capabilities {
				if string(agentCap) == reqCap {
					capabilityMatch = true
					break
				}
			}
			if capabilityMatch {
				break
			}
		}
		return codebaseCompatible && capabilityMatch
	}

	return codebaseCompatible
}

// AssignTask assigns a task to the agent
func (a *Agent) AssignTask(taskID string) {
	a.Status = AgentStatusBusy
	a.CurrentTaskID = &taskID
}

// CompleteTask marks the agent as having completed their current task
func (a *Agent) CompleteTask() {
	a.Status = AgentStatusIdle
	a.CurrentTaskID = nil
}

// Heartbeat updates the agent's last heartbeat timestamp
func (a *Agent) Heartbeat() {
	a.LastHeartbeat = time.Now()
}

// Task methods

// AssignToAgent assigns the task to an agent
func (t *Task) AssignToAgent(agentID string) {
	t.AgentID = &agentID
	t.Status = TaskStatusInProgress
	t.UpdatedAt = time.Now()
}

// Complete marks the task as completed
func (t *Task) Complete() {
	t.Status = TaskStatusCompleted
	t.UpdatedAt = time.Now()
}

// Fail marks the task as failed
func (t *Task) Fail(reason string) {
	t.Status = TaskStatusFailed
	t.UpdatedAt = time.Now()
	if reason != "" {
		t.Metadata["failure_reason"] = reason
	}
}

// Block marks the task as blocked
func (t *Task) Block(reason string) {
	t.Status = TaskStatusBlocked
	t.UpdatedAt = time.Now()
	if reason != "" {
		t.Metadata["block_reason"] = reason
	}
}

// HasFileConflict checks if this task has file conflicts with another task
func (t *Task) HasFileConflict(other *Task) bool {
	if t.CodebaseID != other.CodebaseID {
		return false
	}

	fileSet := make(map[string]bool)
	for _, path := range t.FilePaths {
		fileSet[path] = true
	}

	for _, path := range other.FilePaths {
		if fileSet[path] {
			return true
		}
	}

	return false
}

// IsCrossCodebase checks if the task spans multiple codebases
func (t *Task) IsCrossCodebase() bool {
	return len(t.CrossCodebaseDependencies) > 0
}

// Option types for constructors

type AgentOption func(*Agent)

func WithCodebaseID(codebaseID string) AgentOption {
	return func(a *Agent) {
		a.CodebaseID = codebaseID
	}
}

func WithWorkspacePath(path string) AgentOption {
	return func(a *Agent) {
		a.WorkspacePath = &path
	}
}

func WithCrossCodebaseCapability(capable bool) AgentOption {
	return func(a *Agent) {
		a.CrossCodebaseCapable = capable
	}
}

func WithAgentMetadata(metadata map[string]any) AgentOption {
	return func(a *Agent) {
		for k, v := range metadata {
			a.Metadata[k] = v
		}
	}
}

type TaskOption func(*Task)

func WithPriority(priority Priority) TaskOption {
	return func(t *Task) {
		t.Priority = priority
	}
}

func WithTaskCodebaseID(codebaseID string) TaskOption {
	return func(t *Task) {
		t.CodebaseID = codebaseID
	}
}

func WithFilePaths(filePaths []string) TaskOption {
	return func(t *Task) {
		t.FilePaths = filePaths
	}
}

func WithRequiredCapabilities(capabilities []string) TaskOption {
	return func(t *Task) {
		t.Metadata["required_capabilities"] = capabilities
	}
}

func WithCrossCodebaseDependencies(deps []CrossCodebaseDep) TaskOption {
	return func(t *Task) {
		t.CrossCodebaseDependencies = deps
	}
}

func WithTaskMetadata(metadata map[string]any) TaskOption {
	return func(t *Task) {
		for k, v := range metadata {
			t.Metadata[k] = v
		}
	}
}

type CodebaseOption func(*Codebase)

func WithDescription(description string) CodebaseOption {
	return func(c *Codebase) {
		c.Description = &description
	}
}

func WithCodebaseMetadata(metadata map[string]any) CodebaseOption {
	return func(c *Codebase) {
		for k, v := range metadata {
			c.Metadata[k] = v
		}
	}
}
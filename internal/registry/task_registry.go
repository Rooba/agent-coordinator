package registry

import (
	"fmt"
	"sync"
	"time"

	"github.com/agent-coordinator/go/pkg/types"
	log "github.com/sirupsen/logrus"
)

// TaskRegistry manages task assignment and coordination
type TaskRegistry struct {
	pendingTasks      []*types.Task
	activeTasks       map[string]*types.Task // taskID -> task
	completedTasks    []*types.Task
	fileLocks         map[string]map[string]string // codebaseID -> filePath -> taskID
	crossCodebaseTasks map[string][]*types.Task    // main taskID -> dependent tasks
	mutex             sync.RWMutex
	logger            *log.Logger
	callbacks         TaskCallbacks
	agentRegistry     *AgentRegistry
}

// TaskCallbacks defines callback functions for task events
type TaskCallbacks struct {
	OnTaskAssigned   func(*types.Task, string) error
	OnTaskCompleted  func(*types.Task) error
	OnTaskBlocked    func(*types.Task, string) error
	OnTaskQueued     func(*types.Task) error
}

// NewTaskRegistry creates a new task registry
func NewTaskRegistry(logger *log.Logger, agentRegistry *AgentRegistry) *TaskRegistry {
	if logger == nil {
		logger = log.New()
	}

	return &TaskRegistry{
		pendingTasks:       []*types.Task{},
		activeTasks:        make(map[string]*types.Task),
		completedTasks:     []*types.Task{},
		fileLocks:          make(map[string]map[string]string),
		crossCodebaseTasks: make(map[string][]*types.Task),
		logger:             logger,
		agentRegistry:      agentRegistry,
	}
}

// SetCallbacks sets callback functions for task events
func (tr *TaskRegistry) SetCallbacks(callbacks TaskCallbacks) {
	tr.callbacks = callbacks
}

// CreateTask creates a new task and attempts to assign it immediately
func (tr *TaskRegistry) CreateTask(task *types.Task) error {
	tr.mutex.Lock()
	defer tr.mutex.Unlock()

	tr.logger.WithFields(log.Fields{
		"task_id":     task.ID,
		"title":       task.Title,
		"codebase_id": task.CodebaseID,
		"priority":    task.Priority,
	}).Info("Creating new task")

	// Try to assign immediately
	if agent := tr.agentRegistry.FindAvailableAgent(task); agent != nil {
		// Check for file conflicts
		conflicts := tr.checkFileConflicts(task)
		if len(conflicts) == 0 {
			return tr.assignTaskToAgent(task, agent.ID)
		} else {
			// Block task due to conflicts
			task.Block(fmt.Sprintf("File conflicts: %v", conflicts))
			tr.pendingTasks = append(tr.pendingTasks, task)
			
			tr.logger.WithFields(log.Fields{
				"task_id":   task.ID,
				"conflicts": conflicts,
			}).Warn("Task blocked due to file conflicts")

			if tr.callbacks.OnTaskBlocked != nil {
				tr.callbacks.OnTaskBlocked(task, fmt.Sprintf("File conflicts: %v", conflicts))
			}
			
			return nil
		}
	}

	// No available agent, add to pending
	tr.pendingTasks = append(tr.pendingTasks, task)
	
	tr.logger.WithField("task_id", task.ID).Info("Task queued - no available agents")

	if tr.callbacks.OnTaskQueued != nil {
		tr.callbacks.OnTaskQueued(task)
	}

	return nil
}

// AssignTask attempts to assign a task to an available agent
func (tr *TaskRegistry) AssignTask(task *types.Task) (string, error) {
	tr.mutex.Lock()
	defer tr.mutex.Unlock()

	agent := tr.agentRegistry.FindAvailableAgent(task)
	if agent == nil {
		return "", fmt.Errorf("no available agents for task")
	}

	// Check for file conflicts
	conflicts := tr.checkFileConflicts(task)
	if len(conflicts) > 0 {
		return "", fmt.Errorf("file conflicts: %v", conflicts)
	}

	return agent.ID, tr.assignTaskToAgent(task, agent.ID)
}

// assignTaskToAgent assigns a task to a specific agent (internal method, assumes lock held)
func (tr *TaskRegistry) assignTaskToAgent(task *types.Task, agentID string) error {
	// Update task
	task.AssignToAgent(agentID)
	tr.activeTasks[task.ID] = task

	// Update agent status
	if err := tr.agentRegistry.UpdateAgentStatus(agentID, types.AgentStatusBusy, &task.ID); err != nil {
		tr.logger.WithError(err).Error("Failed to update agent status")
		return err
	}

	// Add file locks
	tr.addFileLocks(task.CodebaseID, task.ID, task.FilePaths)

	tr.logger.WithFields(log.Fields{
		"task_id":     task.ID,
		"agent_id":    agentID,
		"codebase_id": task.CodebaseID,
	}).Info("Task assigned to agent")

	if tr.callbacks.OnTaskAssigned != nil {
		tr.callbacks.OnTaskAssigned(task, agentID)
	}

	return nil
}

// CompleteTask marks a task as completed
func (tr *TaskRegistry) CompleteTask(taskID string, agentID string) error {
	tr.mutex.Lock()
	defer tr.mutex.Unlock()

	task, exists := tr.activeTasks[taskID]
	if !exists {
		return fmt.Errorf("active task not found: %s", taskID)
	}

	if task.AgentID == nil || *task.AgentID != agentID {
		return fmt.Errorf("task not assigned to agent %s", agentID)
	}

	// Complete the task
	task.Complete()
	delete(tr.activeTasks, taskID)
	tr.completedTasks = append(tr.completedTasks, task)

	// Update agent status back to idle
	if err := tr.agentRegistry.UpdateAgentStatus(agentID, types.AgentStatusIdle, nil); err != nil {
		tr.logger.WithError(err).Error("Failed to update agent status")
	}

	// Remove file locks
	tr.removeFileLocks(task.CodebaseID, taskID)

	tr.logger.WithFields(log.Fields{
		"task_id":  taskID,
		"agent_id": agentID,
		"title":    task.Title,
	}).Info("Task completed")

	if tr.callbacks.OnTaskCompleted != nil {
		tr.callbacks.OnTaskCompleted(task)
	}

	// Try to assign pending tasks that might now be unblocked
	tr.processPendingTasks()

	return nil
}

// FailTask marks a task as failed
func (tr *TaskRegistry) FailTask(taskID string, agentID string, reason string) error {
	tr.mutex.Lock()
	defer tr.mutex.Unlock()

	task, exists := tr.activeTasks[taskID]
	if !exists {
		return fmt.Errorf("active task not found: %s", taskID)
	}

	if task.AgentID == nil || *task.AgentID != agentID {
		return fmt.Errorf("task not assigned to agent %s", agentID)
	}

	// Fail the task
	task.Fail(reason)
	delete(tr.activeTasks, taskID)
	tr.completedTasks = append(tr.completedTasks, task)

	// Update agent status back to idle
	if err := tr.agentRegistry.UpdateAgentStatus(agentID, types.AgentStatusIdle, nil); err != nil {
		tr.logger.WithError(err).Error("Failed to update agent status")
	}

	// Remove file locks
	tr.removeFileLocks(task.CodebaseID, taskID)

	tr.logger.WithFields(log.Fields{
		"task_id":  taskID,
		"agent_id": agentID,
		"reason":   reason,
	}).Warn("Task failed")

	// Try to assign pending tasks that might now be unblocked
	tr.processPendingTasks()

	return nil
}

// GetTask retrieves a task by ID
func (tr *TaskRegistry) GetTask(taskID string) (*types.Task, error) {
	tr.mutex.RLock()
	defer tr.mutex.RUnlock()

	// Check active tasks
	if task, exists := tr.activeTasks[taskID]; exists {
		taskCopy := *task
		return &taskCopy, nil
	}

	// Check pending tasks
	for _, task := range tr.pendingTasks {
		if task.ID == taskID {
			taskCopy := *task
			return &taskCopy, nil
		}
	}

	// Check completed tasks
	for _, task := range tr.completedTasks {
		if task.ID == taskID {
			taskCopy := *task
			return &taskCopy, nil
		}
	}

	return nil, fmt.Errorf("task not found: %s", taskID)
}

// ListPendingTasks returns all pending tasks
func (tr *TaskRegistry) ListPendingTasks() []*types.Task {
	tr.mutex.RLock()
	defer tr.mutex.RUnlock()

	tasks := make([]*types.Task, len(tr.pendingTasks))
	for i, task := range tr.pendingTasks {
		taskCopy := *task
		tasks[i] = &taskCopy
	}
	return tasks
}

// ListActiveTasks returns all active tasks
func (tr *TaskRegistry) ListActiveTasks() []*types.Task {
	tr.mutex.RLock()
	defer tr.mutex.RUnlock()

	tasks := make([]*types.Task, 0, len(tr.activeTasks))
	for _, task := range tr.activeTasks {
		taskCopy := *task
		tasks = append(tasks, &taskCopy)
	}
	return tasks
}

// ListCompletedTasks returns all completed tasks
func (tr *TaskRegistry) ListCompletedTasks() []*types.Task {
	tr.mutex.RLock()
	defer tr.mutex.RUnlock()

	tasks := make([]*types.Task, len(tr.completedTasks))
	for i, task := range tr.completedTasks {
		taskCopy := *task
		tasks[i] = &taskCopy
	}
	return tasks
}

// CreateCrossCodebaseTask creates a task that spans multiple codebases
func (tr *TaskRegistry) CreateCrossCodebaseTask(mainTask *types.Task, dependentTasks []*types.Task, strategy string) error {
	tr.mutex.Lock()
	defer tr.mutex.Unlock()

	// Create main task
	if err := tr.CreateTask(mainTask); err != nil {
		return fmt.Errorf("failed to create main task: %w", err)
	}

	// Store dependent tasks
	tr.crossCodebaseTasks[mainTask.ID] = dependentTasks

	// Create dependent tasks based on strategy
	switch strategy {
	case "sequential":
		// Add first dependent task to pending
		if len(dependentTasks) > 0 {
			tr.pendingTasks = append(tr.pendingTasks, dependentTasks[0])
		}
	case "parallel":
		// Add all dependent tasks to pending
		tr.pendingTasks = append(tr.pendingTasks, dependentTasks...)
	case "leader_follower":
		// Wait for main task completion before creating dependent tasks
		// Dependent tasks will be created in the completion callback
	default:
		return fmt.Errorf("unsupported coordination strategy: %s", strategy)
	}

	tr.logger.WithFields(log.Fields{
		"main_task_id":      mainTask.ID,
		"dependent_count":   len(dependentTasks),
		"strategy":          strategy,
	}).Info("Cross-codebase task created")

	return nil
}

// processPendingTasks attempts to assign pending tasks to available agents
func (tr *TaskRegistry) processPendingTasks() {
	var remainingTasks []*types.Task

	for _, task := range tr.pendingTasks {
		agent := tr.agentRegistry.FindAvailableAgent(task)
		if agent == nil {
			remainingTasks = append(remainingTasks, task)
			continue
		}

		// Check for file conflicts
		conflicts := tr.checkFileConflicts(task)
		if len(conflicts) > 0 {
			remainingTasks = append(remainingTasks, task)
			continue
		}

		// Assign the task
		if err := tr.assignTaskToAgent(task, agent.ID); err != nil {
			tr.logger.WithError(err).Error("Failed to assign pending task")
			remainingTasks = append(remainingTasks, task)
		}
	}

	tr.pendingTasks = remainingTasks
}

// checkFileConflicts checks if a task has file conflicts with active tasks
func (tr *TaskRegistry) checkFileConflicts(task *types.Task) []string {
	codebaseLocks, exists := tr.fileLocks[task.CodebaseID]
	if !exists {
		return nil
	}

	var conflicts []string
	for _, filePath := range task.FilePaths {
		if _, locked := codebaseLocks[filePath]; locked {
			conflicts = append(conflicts, filePath)
		}
	}

	return conflicts
}

// addFileLocks adds file locks for a task
func (tr *TaskRegistry) addFileLocks(codebaseID, taskID string, filePaths []string) {
	if tr.fileLocks[codebaseID] == nil {
		tr.fileLocks[codebaseID] = make(map[string]string)
	}

	for _, filePath := range filePaths {
		tr.fileLocks[codebaseID][filePath] = taskID
	}
}

// removeFileLocks removes file locks for a task
func (tr *TaskRegistry) removeFileLocks(codebaseID, taskID string) {
	codebaseLocks, exists := tr.fileLocks[codebaseID]
	if !exists {
		return
	}

	for filePath, lockedTaskID := range codebaseLocks {
		if lockedTaskID == taskID {
			delete(codebaseLocks, filePath)
		}
	}

	// Clean up empty codebase locks
	if len(codebaseLocks) == 0 {
		delete(tr.fileLocks, codebaseID)
	}
}

// GetFileLocks returns current file locks for debugging
func (tr *TaskRegistry) GetFileLocks() map[string]map[string]string {
	tr.mutex.RLock()
	defer tr.mutex.RUnlock()

	// Return a copy to avoid race conditions
	result := make(map[string]map[string]string)
	for codebaseID, locks := range tr.fileLocks {
		result[codebaseID] = make(map[string]string)
		for filePath, taskID := range locks {
			result[codebaseID][filePath] = taskID
		}
	}

	return result
}

// GetStats returns statistics about tasks
func (tr *TaskRegistry) GetStats() map[string]interface{} {
	tr.mutex.RLock()
	defer tr.mutex.RUnlock()

	return map[string]interface{}{
		"pending_tasks":    len(tr.pendingTasks),
		"active_tasks":     len(tr.activeTasks),
		"completed_tasks":  len(tr.completedTasks),
		"cross_codebase_tasks": len(tr.crossCodebaseTasks),
		"file_locks":       len(tr.fileLocks),
	}
}

// CleanupCompletedTasks removes old completed tasks to prevent memory leaks
func (tr *TaskRegistry) CleanupCompletedTasks(olderThan time.Duration) int {
	tr.mutex.Lock()
	defer tr.mutex.Unlock()

	cutoff := time.Now().Add(-olderThan)
	var remaining []*types.Task
	var removedCount int

	for _, task := range tr.completedTasks {
		if task.UpdatedAt.After(cutoff) {
			remaining = append(remaining, task)
		} else {
			removedCount++
		}
	}

	tr.completedTasks = remaining

	if removedCount > 0 {
		tr.logger.WithFields(log.Fields{
			"removed_count": removedCount,
			"cutoff":        cutoff,
		}).Info("Cleaned up old completed tasks")
	}

	return removedCount
}
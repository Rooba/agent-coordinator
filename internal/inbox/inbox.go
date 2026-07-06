package inbox

import (
	"fmt"
	"sync"

	"github.com/agent-coordinator/go/pkg/types"
	log "github.com/sirupsen/logrus"
)

// Inbox manages tasks for a specific agent
type Inbox struct {
	agentID        string
	pendingTasks   []*types.Task
	currentTask    *types.Task
	completedTasks []*types.Task
	mutex          sync.RWMutex
	logger         *log.Logger
}

// InboxManager manages all agent inboxes
type InboxManager struct {
	inboxes map[string]*Inbox
	mutex   sync.RWMutex
	logger  *log.Logger
}

// NewInboxManager creates a new inbox manager
func NewInboxManager(logger *log.Logger) *InboxManager {
	if logger == nil {
		logger = log.New()
	}

	return &InboxManager{
		inboxes: make(map[string]*Inbox),
		logger:  logger,
	}
}

// CreateInbox creates a new inbox for an agent
func (im *InboxManager) CreateInbox(agentID string) *Inbox {
	im.mutex.Lock()
	defer im.mutex.Unlock()

	// Check if inbox already exists
	if inbox, exists := im.inboxes[agentID]; exists {
		return inbox
	}

	inbox := &Inbox{
		agentID:        agentID,
		pendingTasks:   []*types.Task{},
		completedTasks: []*types.Task{},
		logger:         im.logger,
	}

	im.inboxes[agentID] = inbox

	im.logger.WithField("agent_id", agentID).Debug("Created inbox for agent")

	return inbox
}

// GetInbox retrieves an inbox for an agent
func (im *InboxManager) GetInbox(agentID string) (*Inbox, error) {
	im.mutex.RLock()
	defer im.mutex.RUnlock()

	inbox, exists := im.inboxes[agentID]
	if !exists {
		return nil, fmt.Errorf("inbox not found for agent: %s", agentID)
	}

	return inbox, nil
}

// DeleteInbox removes an inbox for an agent
func (im *InboxManager) DeleteInbox(agentID string) error {
	im.mutex.Lock()
	defer im.mutex.Unlock()

	inbox, exists := im.inboxes[agentID]
	if !exists {
		return fmt.Errorf("inbox not found for agent: %s", agentID)
	}

	// Check if agent has current task
	if inbox.currentTask != nil {
		return fmt.Errorf("cannot delete inbox with current task")
	}

	delete(im.inboxes, agentID)

	im.logger.WithField("agent_id", agentID).Debug("Deleted inbox for agent")

	return nil
}

// ForceDeleteInbox removes an inbox even if it has active tasks
func (im *InboxManager) ForceDeleteInbox(agentID string) error {
	im.mutex.Lock()
	defer im.mutex.Unlock()

	if _, exists := im.inboxes[agentID]; !exists {
		return fmt.Errorf("inbox not found for agent: %s", agentID)
	}

	delete(im.inboxes, agentID)

	im.logger.WithField("agent_id", agentID).Warn("Force deleted inbox for agent")

	return nil
}

// ListInboxes returns all inbox agent IDs
func (im *InboxManager) ListInboxes() []string {
	im.mutex.RLock()
	defer im.mutex.RUnlock()

	agentIDs := make([]string, 0, len(im.inboxes))
	for agentID := range im.inboxes {
		agentIDs = append(agentIDs, agentID)
	}

	return agentIDs
}

// Inbox methods

// AddTask adds a task to the inbox
func (i *Inbox) AddTask(task *types.Task) error {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	i.pendingTasks = append(i.pendingTasks, task)

	i.logger.WithFields(log.Fields{
		"task_id": task.ID,
		"title":   task.Title,
	}).Debug("Task added to inbox")

	return nil
}

// GetNextTask retrieves the next pending task
func (i *Inbox) GetNextTask() (*types.Task, error) {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.currentTask != nil {
		return nil, fmt.Errorf("agent already has an active task")
	}

	if len(i.pendingTasks) == 0 {
		return nil, fmt.Errorf("no pending tasks")
	}

	// Get the first pending task (FIFO)
	task := i.pendingTasks[0]
	i.pendingTasks = i.pendingTasks[1:]
	i.currentTask = task

	i.logger.WithFields(log.Fields{
		"task_id": task.ID,
		"title":   task.Title,
	}).Debug("Task retrieved from inbox")

	return task, nil
}

// GetCurrentTask returns the currently active task
func (i *Inbox) GetCurrentTask() *types.Task {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	return i.currentTask
}

// CompleteCurrentTask marks the current task as completed
func (i *Inbox) CompleteCurrentTask() (*types.Task, error) {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.currentTask == nil {
		return nil, fmt.Errorf("no current task to complete")
	}

	task := i.currentTask
	task.Complete()
	i.completedTasks = append(i.completedTasks, task)
	i.currentTask = nil

	i.logger.WithFields(log.Fields{
		"task_id": task.ID,
		"title":   task.Title,
	}).Debug("Task completed in inbox")

	return task, nil
}

// FailCurrentTask marks the current task as failed
func (i *Inbox) FailCurrentTask(reason string) (*types.Task, error) {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.currentTask == nil {
		return nil, fmt.Errorf("no current task to fail")
	}

	task := i.currentTask
	task.Fail(reason)
	i.completedTasks = append(i.completedTasks, task)
	i.currentTask = nil

	i.logger.WithFields(log.Fields{
		"task_id": task.ID,
		"title":   task.Title,
		"reason":  reason,
	}).Warn("Task failed in inbox")

	return task, nil
}

// GetStatus returns the current status of the inbox
func (i *Inbox) GetStatus() *types.InboxStatus {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	return &types.InboxStatus{
		AgentID:        i.agentID,
		CurrentTask:    i.currentTask,
		PendingCount:   len(i.pendingTasks),
		CompletedCount: len(i.completedTasks),
	}
}

// ListPendingTasks returns all pending tasks
func (i *Inbox) ListPendingTasks() []*types.Task {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	// Return copies to avoid race conditions
	tasks := make([]*types.Task, len(i.pendingTasks))
	for idx, task := range i.pendingTasks {
		taskCopy := *task
		tasks[idx] = &taskCopy
	}

	return tasks
}

// ListCompletedTasks returns all completed tasks
func (i *Inbox) ListCompletedTasks() []*types.Task {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	// Return copies to avoid race conditions
	tasks := make([]*types.Task, len(i.completedTasks))
	for idx, task := range i.completedTasks {
		taskCopy := *task
		tasks[idx] = &taskCopy
	}

	return tasks
}

// ClearCompletedTasks removes completed tasks from the inbox
func (i *Inbox) ClearCompletedTasks() int {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	count := len(i.completedTasks)
	i.completedTasks = []*types.Task{}

	i.logger.WithField("cleared_count", count).Debug("Cleared completed tasks from inbox")

	return count
}

// RemoveTask removes a specific task from pending tasks
func (i *Inbox) RemoveTask(taskID string) error {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	// Check if it's the current task
	if i.currentTask != nil && i.currentTask.ID == taskID {
		return fmt.Errorf("cannot remove current task, complete or fail it first")
	}

	// Remove from pending tasks
	var newPendingTasks []*types.Task
	var found bool

	for _, task := range i.pendingTasks {
		if task.ID != taskID {
			newPendingTasks = append(newPendingTasks, task)
		} else {
			found = true
		}
	}

	if !found {
		return fmt.Errorf("task not found in pending tasks: %s", taskID)
	}

	i.pendingTasks = newPendingTasks

	i.logger.WithField("task_id", taskID).Debug("Task removed from inbox")

	return nil
}

// GetTaskByID finds a task by ID in any state
func (i *Inbox) GetTaskByID(taskID string) (*types.Task, error) {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	// Check current task
	if i.currentTask != nil && i.currentTask.ID == taskID {
		taskCopy := *i.currentTask
		return &taskCopy, nil
	}

	// Check pending tasks
	for _, task := range i.pendingTasks {
		if task.ID == taskID {
			taskCopy := *task
			return &taskCopy, nil
		}
	}

	// Check completed tasks
	for _, task := range i.completedTasks {
		if task.ID == taskID {
			taskCopy := *task
			return &taskCopy, nil
		}
	}

	return nil, fmt.Errorf("task not found: %s", taskID)
}

// IsEmpty returns true if the inbox has no tasks in any state
func (i *Inbox) IsEmpty() bool {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	return len(i.pendingTasks) == 0 && i.currentTask == nil && len(i.completedTasks) == 0
}

// HasPendingTasks returns true if there are pending tasks
func (i *Inbox) HasPendingTasks() bool {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	return len(i.pendingTasks) > 0
}

// HasCurrentTask returns true if there's a current active task
func (i *Inbox) HasCurrentTask() bool {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	return i.currentTask != nil
}
package registry

import (
	"fmt"
	"sync"
	"time"

	"github.com/agent-coordinator/go/pkg/types"
	log "github.com/sirupsen/logrus"
)

// CodebaseRegistry manages codebases and their relationships
type CodebaseRegistry struct {
	codebases    map[string]*types.Codebase
	dependencies map[string][]CodebaseDependency // source -> dependencies
	mutex        sync.RWMutex
	logger       *log.Logger
}

// CodebaseDependency represents a dependency between codebases
type CodebaseDependency struct {
	TargetCodebaseID string            `json:"target_codebase_id"`
	DependencyType   string            `json:"dependency_type"`
	Metadata         map[string]any    `json:"metadata"`
	CreatedAt        time.Time         `json:"created_at"`
}

// NewCodebaseRegistry creates a new codebase registry
func NewCodebaseRegistry(logger *log.Logger) *CodebaseRegistry {
	if logger == nil {
		logger = log.New()
	}

	return &CodebaseRegistry{
		codebases:    make(map[string]*types.Codebase),
		dependencies: make(map[string][]CodebaseDependency),
		logger:       logger,
	}
}

// RegisterCodebase registers a new codebase
func (cr *CodebaseRegistry) RegisterCodebase(codebase *types.Codebase) error {
	cr.mutex.Lock()
	defer cr.mutex.Unlock()

	if _, exists := cr.codebases[codebase.ID]; exists {
		return fmt.Errorf("codebase with ID '%s' already exists", codebase.ID)
	}

	cr.codebases[codebase.ID] = codebase
	cr.logger.WithFields(log.Fields{
		"codebase_id":    codebase.ID,
		"codebase_name":  codebase.Name,
		"workspace_path": codebase.WorkspacePath,
	}).Info("Codebase registered")

	return nil
}

// GetCodebase retrieves a codebase by ID
func (cr *CodebaseRegistry) GetCodebase(codebaseID string) (*types.Codebase, error) {
	cr.mutex.RLock()
	defer cr.mutex.RUnlock()

	codebase, exists := cr.codebases[codebaseID]
	if !exists {
		return nil, fmt.Errorf("codebase not found: %s", codebaseID)
	}

	// Return a copy to avoid race conditions
	codebaseCopy := *codebase
	return &codebaseCopy, nil
}

// ListCodebases returns all registered codebases
func (cr *CodebaseRegistry) ListCodebases() []*types.Codebase {
	cr.mutex.RLock()
	defer cr.mutex.RUnlock()

	codebases := make([]*types.Codebase, 0, len(cr.codebases))
	for _, codebase := range cr.codebases {
		// Return copies to avoid race conditions
		codebaseCopy := *codebase
		codebases = append(codebases, &codebaseCopy)
	}

	return codebases
}

// AddAgentToCodebase adds an agent to a codebase
func (cr *CodebaseRegistry) AddAgentToCodebase(codebaseID, agentID string) error {
	cr.mutex.Lock()
	defer cr.mutex.Unlock()

	codebase, exists := cr.codebases[codebaseID]
	if !exists {
		return fmt.Errorf("codebase not found: %s", codebaseID)
	}

	// Check if agent is already added
	for _, existingAgentID := range codebase.Agents {
		if existingAgentID == agentID {
			return nil // Already added
		}
	}

	codebase.Agents = append(codebase.Agents, agentID)
	codebase.UpdatedAt = time.Now()

	cr.logger.WithFields(log.Fields{
		"codebase_id": codebaseID,
		"agent_id":    agentID,
	}).Debug("Agent added to codebase")

	return nil
}

// RemoveAgentFromCodebase removes an agent from a codebase
func (cr *CodebaseRegistry) RemoveAgentFromCodebase(codebaseID, agentID string) error {
	cr.mutex.Lock()
	defer cr.mutex.Unlock()

	codebase, exists := cr.codebases[codebaseID]
	if !exists {
		return fmt.Errorf("codebase not found: %s", codebaseID)
	}

	// Remove agent from list
	var newAgents []string
	for _, existingAgentID := range codebase.Agents {
		if existingAgentID != agentID {
			newAgents = append(newAgents, existingAgentID)
		}
	}

	codebase.Agents = newAgents
	codebase.UpdatedAt = time.Now()

	cr.logger.WithFields(log.Fields{
		"codebase_id": codebaseID,
		"agent_id":    agentID,
	}).Debug("Agent removed from codebase")

	return nil
}

// AddTaskToCodebase adds a task to a codebase's active tasks
func (cr *CodebaseRegistry) AddTaskToCodebase(codebaseID, taskID string) error {
	cr.mutex.Lock()
	defer cr.mutex.Unlock()

	codebase, exists := cr.codebases[codebaseID]
	if !exists {
		return fmt.Errorf("codebase not found: %s", codebaseID)
	}

	// Check if task is already added
	for _, existingTaskID := range codebase.ActiveTasks {
		if existingTaskID == taskID {
			return nil // Already added
		}
	}

	codebase.ActiveTasks = append(codebase.ActiveTasks, taskID)
	codebase.UpdatedAt = time.Now()

	cr.logger.WithFields(log.Fields{
		"codebase_id": codebaseID,
		"task_id":     taskID,
	}).Debug("Task added to codebase")

	return nil
}

// RemoveTaskFromCodebase removes a task from a codebase's active tasks
func (cr *CodebaseRegistry) RemoveTaskFromCodebase(codebaseID, taskID string) error {
	cr.mutex.Lock()
	defer cr.mutex.Unlock()

	codebase, exists := cr.codebases[codebaseID]
	if !exists {
		return fmt.Errorf("codebase not found: %s", codebaseID)
	}

	// Remove task from list
	var newTasks []string
	for _, existingTaskID := range codebase.ActiveTasks {
		if existingTaskID != taskID {
			newTasks = append(newTasks, existingTaskID)
		}
	}

	codebase.ActiveTasks = newTasks
	codebase.UpdatedAt = time.Now()

	cr.logger.WithFields(log.Fields{
		"codebase_id": codebaseID,
		"task_id":     taskID,
	}).Debug("Task removed from codebase")

	return nil
}

// AddCrossCodebaseDependency adds a dependency relationship between codebases
func (cr *CodebaseRegistry) AddCrossCodebaseDependency(sourceCodebaseID, targetCodebaseID, dependencyType string, metadata map[string]any) error {
	cr.mutex.Lock()
	defer cr.mutex.Unlock()

	// Verify both codebases exist
	if _, exists := cr.codebases[sourceCodebaseID]; !exists {
		return fmt.Errorf("source codebase not found: %s", sourceCodebaseID)
	}
	if _, exists := cr.codebases[targetCodebaseID]; !exists {
		return fmt.Errorf("target codebase not found: %s", targetCodebaseID)
	}

	dependency := CodebaseDependency{
		TargetCodebaseID: targetCodebaseID,
		DependencyType:   dependencyType,
		Metadata:         metadata,
		CreatedAt:        time.Now(),
	}

	cr.dependencies[sourceCodebaseID] = append(cr.dependencies[sourceCodebaseID], dependency)

	cr.logger.WithFields(log.Fields{
		"source_codebase": sourceCodebaseID,
		"target_codebase": targetCodebaseID,
		"dependency_type": dependencyType,
	}).Info("Cross-codebase dependency added")

	return nil
}

// GetCodebaseDependencies returns dependencies for a codebase
func (cr *CodebaseRegistry) GetCodebaseDependencies(codebaseID string) []CodebaseDependency {
	cr.mutex.RLock()
	defer cr.mutex.RUnlock()

	deps, exists := cr.dependencies[codebaseID]
	if !exists {
		return []CodebaseDependency{}
	}

	// Return copies to avoid race conditions
	result := make([]CodebaseDependency, len(deps))
	copy(result, deps)
	return result
}

// GetCodebaseStats returns statistics for a codebase
func (cr *CodebaseRegistry) GetCodebaseStats(codebaseID string) (*types.CodebaseStats, error) {
	cr.mutex.RLock()
	defer cr.mutex.RUnlock()

	codebase, exists := cr.codebases[codebaseID]
	if !exists {
		return nil, fmt.Errorf("codebase not found: %s", codebaseID)
	}

	stats := &types.CodebaseStats{
		CodebaseID:     codebase.ID,
		Name:           codebase.Name,
		ActiveAgents:   len(codebase.Agents),
		ActiveTasks:    len(codebase.ActiveTasks),
		PendingTasks:   0, // This would need to be computed from TaskRegistry
		CompletedTasks: 0, // This would need to be computed from TaskRegistry
	}

	return stats, nil
}

// UpdateCodebase updates an existing codebase
func (cr *CodebaseRegistry) UpdateCodebase(codebaseID string, updates map[string]any) error {
	cr.mutex.Lock()
	defer cr.mutex.Unlock()

	codebase, exists := cr.codebases[codebaseID]
	if !exists {
		return fmt.Errorf("codebase not found: %s", codebaseID)
	}

	// Update fields based on the updates map
	if name, ok := updates["name"].(string); ok {
		codebase.Name = name
	}
	if workspacePath, ok := updates["workspace_path"].(string); ok {
		codebase.WorkspacePath = workspacePath
	}
	if description, ok := updates["description"].(string); ok {
		codebase.Description = &description
	}
	if metadata, ok := updates["metadata"].(map[string]any); ok {
		for k, v := range metadata {
			codebase.Metadata[k] = v
		}
	}

	codebase.UpdatedAt = time.Now()

	cr.logger.WithFields(log.Fields{
		"codebase_id": codebaseID,
		"updates":     updates,
	}).Info("Codebase updated")

	return nil
}

// DeleteCodebase removes a codebase from the registry
func (cr *CodebaseRegistry) DeleteCodebase(codebaseID string) error {
	cr.mutex.Lock()
	defer cr.mutex.Unlock()

	codebase, exists := cr.codebases[codebaseID]
	if !exists {
		return fmt.Errorf("codebase not found: %s", codebaseID)
	}

	// Check if codebase has active agents or tasks
	if len(codebase.Agents) > 0 {
		return fmt.Errorf("cannot delete codebase with active agents")
	}
	if len(codebase.ActiveTasks) > 0 {
		return fmt.Errorf("cannot delete codebase with active tasks")
	}

	// Remove the codebase
	delete(cr.codebases, codebaseID)

	// Clean up dependencies
	delete(cr.dependencies, codebaseID)
	for sourceID, deps := range cr.dependencies {
		var newDeps []CodebaseDependency
		for _, dep := range deps {
			if dep.TargetCodebaseID != codebaseID {
				newDeps = append(newDeps, dep)
			}
		}
		cr.dependencies[sourceID] = newDeps
	}

	cr.logger.WithField("codebase_id", codebaseID).Info("Codebase deleted")

	return nil
}

// GetOverallStats returns overall statistics across all codebases
func (cr *CodebaseRegistry) GetOverallStats() map[string]interface{} {
	cr.mutex.RLock()
	defer cr.mutex.RUnlock()

	totalAgents := 0
	totalActiveTasks := 0
	
	for _, codebase := range cr.codebases {
		totalAgents += len(codebase.Agents)
		totalActiveTasks += len(codebase.ActiveTasks)
	}

	return map[string]interface{}{
		"total_codebases":     len(cr.codebases),
		"total_agents":        totalAgents,
		"total_active_tasks":  totalActiveTasks,
		"total_dependencies":  len(cr.dependencies),
	}
}

// ValidateCodebaseExists checks if a codebase exists
func (cr *CodebaseRegistry) ValidateCodebaseExists(codebaseID string) bool {
	cr.mutex.RLock()
	defer cr.mutex.RUnlock()

	_, exists := cr.codebases[codebaseID]
	return exists
}
package registry

import (
	"fmt"
	"sync"
	"time"

	"github.com/agent-coordinator/go/pkg/types"
	log "github.com/sirupsen/logrus"
)

// AgentRegistry manages agent registration and lifecycle
type AgentRegistry struct {
	agents    map[string]*types.Agent
	mutex     sync.RWMutex
	logger    *log.Logger
	callbacks AgentCallbacks
}

// AgentCallbacks defines callback functions for agent events
type AgentCallbacks struct {
	OnAgentRegistered   func(*types.Agent) error
	OnAgentUnregistered func(string, string) error
	OnAgentHeartbeat    func(string) error
}

// NewAgentRegistry creates a new agent registry
func NewAgentRegistry(logger *log.Logger) *AgentRegistry {
	if logger == nil {
		logger = log.New()
	}

	return &AgentRegistry{
		agents: make(map[string]*types.Agent),
		logger: logger,
	}
}

// SetCallbacks sets callback functions for agent events
func (ar *AgentRegistry) SetCallbacks(callbacks AgentCallbacks) {
	ar.callbacks = callbacks
}

// RegisterAgent registers a new agent in the system
func (ar *AgentRegistry) RegisterAgent(agent *types.Agent) error {
	ar.mutex.Lock()
	defer ar.mutex.Unlock()

	// Check for duplicate names
	for _, existingAgent := range ar.agents {
		if existingAgent.Name == agent.Name {
			return fmt.Errorf("agent with name '%s' already exists", agent.Name)
		}
	}

	ar.agents[agent.ID] = agent
	ar.logger.WithFields(log.Fields{
		"agent_id":    agent.ID,
		"agent_name":  agent.Name,
		"codebase_id": agent.CodebaseID,
	}).Info("Agent registered")

	// Trigger callback
	if ar.callbacks.OnAgentRegistered != nil {
		if err := ar.callbacks.OnAgentRegistered(agent); err != nil {
			ar.logger.WithError(err).Error("Agent registration callback failed")
			// Don't fail registration due to callback failure
		}
	}

	return nil
}

// UnregisterAgent removes an agent from the system
func (ar *AgentRegistry) UnregisterAgent(agentID, reason string) error {
	ar.mutex.Lock()
	defer ar.mutex.Unlock()

	agent, exists := ar.agents[agentID]
	if !exists {
		return fmt.Errorf("agent not found: %s", agentID)
	}

	// Check if agent has current task
	if agent.CurrentTaskID != nil {
		return fmt.Errorf("agent has active task %s. Complete task first or use force unregister", *agent.CurrentTaskID)
	}

	delete(ar.agents, agentID)
	ar.logger.WithFields(log.Fields{
		"agent_id":   agentID,
		"agent_name": agent.Name,
		"reason":     reason,
	}).Info("Agent unregistered")

	// Trigger callback
	if ar.callbacks.OnAgentUnregistered != nil {
		if err := ar.callbacks.OnAgentUnregistered(agentID, reason); err != nil {
			ar.logger.WithError(err).Error("Agent unregistration callback failed")
		}
	}

	return nil
}

// ForceUnregisterAgent removes an agent even if it has active tasks
func (ar *AgentRegistry) ForceUnregisterAgent(agentID, reason string) error {
	ar.mutex.Lock()
	defer ar.mutex.Unlock()

	agent, exists := ar.agents[agentID]
	if !exists {
		return fmt.Errorf("agent not found: %s", agentID)
	}

	delete(ar.agents, agentID)
	ar.logger.WithFields(log.Fields{
		"agent_id":        agentID,
		"agent_name":      agent.Name,
		"reason":          reason,
		"had_active_task": agent.CurrentTaskID != nil,
	}).Warn("Agent force unregistered")

	// Trigger callback
	if ar.callbacks.OnAgentUnregistered != nil {
		if err := ar.callbacks.OnAgentUnregistered(agentID, reason); err != nil {
			ar.logger.WithError(err).Error("Agent unregistration callback failed")
		}
	}

	return nil
}

// GetAgent retrieves an agent by ID
func (ar *AgentRegistry) GetAgent(agentID string) (*types.Agent, error) {
	ar.mutex.RLock()
	defer ar.mutex.RUnlock()

	agent, exists := ar.agents[agentID]
	if !exists {
		return nil, fmt.Errorf("agent not found: %s", agentID)
	}

	// Return a copy to avoid race conditions
	agentCopy := *agent
	return &agentCopy, nil
}

// GetAgentByName retrieves an agent by name
func (ar *AgentRegistry) GetAgentByName(name string) (*types.Agent, error) {
	ar.mutex.RLock()
	defer ar.mutex.RUnlock()

	for _, agent := range ar.agents {
		if agent.Name == name {
			// Return a copy to avoid race conditions
			agentCopy := *agent
			return &agentCopy, nil
		}
	}

	return nil, fmt.Errorf("agent not found with name: %s", name)
}

// ListAgents returns all registered agents
func (ar *AgentRegistry) ListAgents() []*types.Agent {
	ar.mutex.RLock()
	defer ar.mutex.RUnlock()

	agents := make([]*types.Agent, 0, len(ar.agents))
	for _, agent := range ar.agents {
		// Return copies to avoid race conditions
		agentCopy := *agent
		agents = append(agents, &agentCopy)
	}

	return agents
}

// ListAgentsByCodebase returns agents for a specific codebase
func (ar *AgentRegistry) ListAgentsByCodebase(codebaseID string) []*types.Agent {
	ar.mutex.RLock()
	defer ar.mutex.RUnlock()

	var agents []*types.Agent
	for _, agent := range ar.agents {
		if agent.CodebaseID == codebaseID {
			// Return copy to avoid race conditions
			agentCopy := *agent
			agents = append(agents, &agentCopy)
		}
	}

	return agents
}

// UpdateAgentHeartbeat updates an agent's last heartbeat
func (ar *AgentRegistry) UpdateAgentHeartbeat(agentID string) error {
	ar.mutex.Lock()
	defer ar.mutex.Unlock()

	agent, exists := ar.agents[agentID]
	if !exists {
		return fmt.Errorf("agent not found: %s", agentID)
	}

	agent.Heartbeat()
	ar.logger.WithField("agent_id", agentID).Debug("Agent heartbeat updated")

	// Trigger callback
	if ar.callbacks.OnAgentHeartbeat != nil {
		if err := ar.callbacks.OnAgentHeartbeat(agentID); err != nil {
			ar.logger.WithError(err).Error("Agent heartbeat callback failed")
		}
	}

	return nil
}

// UpdateAgentStatus updates an agent's status and current task
func (ar *AgentRegistry) UpdateAgentStatus(agentID string, status types.AgentStatus, currentTaskID *string) error {
	ar.mutex.Lock()
	defer ar.mutex.Unlock()

	agent, exists := ar.agents[agentID]
	if !exists {
		return fmt.Errorf("agent not found: %s", agentID)
	}

	agent.Status = status
	agent.CurrentTaskID = currentTaskID

	ar.logger.WithFields(log.Fields{
		"agent_id":        agentID,
		"status":          status,
		"current_task_id": currentTaskID,
	}).Debug("Agent status updated")

	return nil
}

// FindAvailableAgent finds an available agent that can handle the given task
func (ar *AgentRegistry) FindAvailableAgent(task *types.Task) *types.Agent {
	ar.mutex.RLock()
	defer ar.mutex.RUnlock()

	var candidates []*types.Agent

	// Find agents that can handle the task
	for _, agent := range ar.agents {
		if agent.Status == types.AgentStatusIdle &&
			agent.IsOnline() &&
			agent.CanHandle(task) {
			candidates = append(candidates, agent)
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	// Sort by preference (same codebase first, then by capabilities)
	// For now, just return the first candidate
	// TODO: Implement more sophisticated agent selection
	
	// Return copy to avoid race conditions
	selected := *candidates[0]
	return &selected
}

// GetStats returns statistics about registered agents
func (ar *AgentRegistry) GetStats() map[string]interface{} {
	ar.mutex.RLock()
	defer ar.mutex.RUnlock()

	stats := map[string]interface{}{
		"total_agents":   len(ar.agents),
		"online_agents":  0,
		"idle_agents":    0,
		"busy_agents":    0,
		"offline_agents": 0,
	}

	codebaseStats := make(map[string]int)

	for _, agent := range ar.agents {
		// Count by codebase
		codebaseStats[agent.CodebaseID]++

		// Count by status
		if agent.IsOnline() {
			stats["online_agents"] = stats["online_agents"].(int) + 1
			switch agent.Status {
			case types.AgentStatusIdle:
				stats["idle_agents"] = stats["idle_agents"].(int) + 1
			case types.AgentStatusBusy:
				stats["busy_agents"] = stats["busy_agents"].(int) + 1
			}
		} else {
			stats["offline_agents"] = stats["offline_agents"].(int) + 1
		}
	}

	stats["codebase_distribution"] = codebaseStats

	return stats
}

// Cleanup removes offline agents that haven't sent heartbeat in specified duration
func (ar *AgentRegistry) Cleanup(offlineThreshold time.Duration) int {
	ar.mutex.Lock()
	defer ar.mutex.Unlock()

	var removedCount int
	cutoff := time.Now().Add(-offlineThreshold)

	for agentID, agent := range ar.agents {
		if agent.LastHeartbeat.Before(cutoff) {
			delete(ar.agents, agentID)
			removedCount++

			ar.logger.WithFields(log.Fields{
				"agent_id":       agentID,
				"agent_name":     agent.Name,
				"last_heartbeat": agent.LastHeartbeat,
			}).Info("Cleaned up offline agent")

			// Trigger callback
			if ar.callbacks.OnAgentUnregistered != nil {
				if err := ar.callbacks.OnAgentUnregistered(agentID, "cleanup: offline too long"); err != nil {
					ar.logger.WithError(err).Error("Agent cleanup callback failed")
				}
			}
		}
	}

	return removedCount
}
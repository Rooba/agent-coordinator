package protocol

const (
	OpRegister   = "register"
	OpDeregister = "deregister"
	OpIdle       = "idle"
	OpEvent      = "event"
	OpAgents     = "agents"
	OpBoard      = "board"
	OpSend       = "send"
	OpRead       = "read"
	OpBroadcast  = "broadcast"
)

type TaskEvent struct {
	Kind    string `json:"kind"` // "create" | "update"
	Key     string `json:"key"`  // task id/number as string
	Subject string `json:"subject,omitempty"`
	Status  string `json:"status,omitempty"` // pending|in_progress|completed|deleted
}

type Request struct {
	Op        string     `json:"op"`
	Scope     string     `json:"scope"`
	SessionID string     `json:"session_id,omitempty"`
	Source    string     `json:"source,omitempty"`
	Tool      string     `json:"tool,omitempty"`
	Activity  string     `json:"activity,omitempty"`
	Files     []string   `json:"files,omitempty"`
	Writes    []string   `json:"writes,omitempty"`
	TaskEv    *TaskEvent `json:"task_ev,omitempty"`
	From      string     `json:"from,omitempty"` // agent name (send/read/broadcast)
	To        string     `json:"to,omitempty"`   // agent name or agent_id (send)
	Body      string     `json:"body,omitempty"`
}

type AgentInfo struct {
	AgentID      string   `json:"agent_id"`
	Name         string   `json:"name"`
	Status       string   `json:"status"` // active|idle|stale|gone
	CurrentTask  string   `json:"current_task,omitempty"`
	Activity     string   `json:"activity,omitempty"`
	Files        []string `json:"files,omitempty"`
	LastSeen     int64    `json:"last_seen"`
	TasksPending int      `json:"tasks_pending"`
	TasksDone    int      `json:"tasks_completed"`
}

type Message struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	Body      string `json:"body"`
	SentAt    int64  `json:"sent_at"`
	Broadcast bool   `json:"broadcast"`
}

type Response struct {
	OK       bool        `json:"ok"`
	Error    string      `json:"error,omitempty"`
	Name     string      `json:"name,omitempty"`    // register: assigned friendly name
	Notices  []string    `json:"notices,omitempty"` // event: inbox/conflict lines
	Agents   []AgentInfo `json:"agents,omitempty"`
	Messages []Message   `json:"messages,omitempty"`
}

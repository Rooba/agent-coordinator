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
	OpPeek       = "peek"
	OpBroadcast  = "broadcast"
	OpWhoami     = "whoami"
	OpClaim      = "claim"
	OpRelease    = "release"
	OpClaims     = "claims"
	OpHistory    = "history"
)

type TaskEvent struct {
	Kind    string `json:"kind"` // "create" | "update"
	Key     string `json:"key"`  // task id/number as string
	Subject string `json:"subject,omitempty"`
	Status  string `json:"status,omitempty"` // pending|in_progress|completed|deleted
}

type Request struct {
	Op           string      `json:"op"`
	Scope        string      `json:"scope"`
	SessionID    string      `json:"session_id,omitempty"`
	Source       string      `json:"source,omitempty"`
	Tool         string      `json:"tool,omitempty"`
	Activity     string      `json:"activity,omitempty"`
	Files        []string    `json:"files,omitempty"`
	Writes       []string    `json:"writes,omitempty"`
	TaskEv       *TaskEvent  `json:"task_ev,omitempty"`
	Tasks        []TaskEvent `json:"tasks,omitempty"` // complete task snapshot (for update_plan-style clients)
	ReplaceTasks bool        `json:"replace_tasks,omitempty"`
	From         string      `json:"from,omitempty"` // agent name (send/read/broadcast)
	To           string      `json:"to,omitempty"`   // agent name or agent_id (send)
	Body         string      `json:"body,omitempty"`
	// AfterID limits OpPeek to unread messages with id strictly greater than
	// this value. Wait baselines on HighWater at arm time so stale backlog
	// never wakes; only newer mail does.
	AfterID int64 `json:"after_id,omitempty"`
	// OnlyIfNoHook makes OpRegister refuse to mint a new identity while the
	// scope has live hook-registered agents (the caller should bind instead).
	OnlyIfNoHook bool `json:"only_if_no_hook,omitempty"`
	// IncludeGone makes OpBoard include agents whose presence has decayed to
	// gone; the default board hides them.
	IncludeGone bool `json:"include_gone,omitempty"`
	// AgentID / AgentType are the subagent fields from hook events. When
	// AgentID is set, register/event/deregister target the CHILD row derived
	// from SessionID (the parent session), never the parent itself.
	AgentID   string `json:"agent_id,omitempty"`
	AgentType string `json:"agent_type,omitempty"`
	// Path / Note are the claims-ledger fields (claim/release).
	Path string `json:"path,omitempty"`
	Note string `json:"note,omitempty"`
	// Peer / Limit filter OpHistory: only exchanges with Peer, at most Limit
	// rows (default 20, capped at 100).
	Peer  string `json:"peer,omitempty"`
	Limit int    `json:"limit,omitempty"`
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
	Parent       string   `json:"parent,omitempty"` // parent agent's name for subagent rows
	Claims       []string `json:"claims,omitempty"` // paths this agent holds in the claims ledger
}

// ClaimInfo is one row of the claims ledger, holder resolved live.
type ClaimInfo struct {
	Path     string `json:"path"`
	Holder   string `json:"holder"` // holder's current name; empty if the row was purged
	HolderID string `json:"holder_id"`
	Note     string `json:"note,omitempty"`
	Since    int64  `json:"since"`
}

// HistoryInfo is one row of the message journal: a delivery seen from either
// side, with read_at exposing who read what and when (0 = still unread).
type HistoryInfo struct {
	MessageID   int64  `json:"message_id"`
	From        string `json:"from"`
	To          string `json:"to"`
	BodyPreview string `json:"body_preview"`
	SentAt      int64  `json:"sent_at"`
	ReadAt      int64  `json:"read_at"`
	Broadcast   bool   `json:"broadcast"`
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
	Unread   int         `json:"unread,omitempty"` // peek: unread delivery count (respects AfterID)
	// HighWater is the max message id ever delivered to the agent in this
	// scope (read or unread). Wait arms against this so only newer mail wakes.
	HighWater int64 `json:"high_water,omitempty"`
	// PeekIDs / PeekFroms summarize the matching unread set for machine-parseable wait stdout.
	PeekIDs   []int64  `json:"peek_ids,omitempty"`
	PeekFroms []string `json:"peek_froms,omitempty"`
	// Whoami: the bound identity's stable id, registration source, and parent
	// agent name (set only for subagent child rows).
	AgentID string `json:"agent_id,omitempty"`
	Source  string `json:"source,omitempty"`
	Parent  string `json:"parent,omitempty"`
	// Claims / History carry the claims-ledger and message-journal listings.
	Claims  []ClaimInfo   `json:"claims,omitempty"`
	History []HistoryInfo `json:"history,omitempty"`
}

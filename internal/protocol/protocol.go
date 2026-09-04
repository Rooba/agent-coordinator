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
	// Cross-workspace / host-eyes ops. TCP callers may use the same strings;
	// the daemon's TCP path allowlists them and binds identity itself.
	OpListWorkspaces = "list_workspaces"
	OpListEyes       = "list_eyes"
	OpSendWorkspace  = "send_workspace"
	OpRequestEyes    = "request_eyes"
	OpCancelEyes     = "cancel_eyes"

	KindEyes     = "eyes"
	KindLauncher = "launcher"
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
	// Token authenticates TCP relay frames. Unix-socket callers leave it empty.
	Token string `json:"token,omitempty"`
	// SessionSecret is the per-session TCP credential returned on relay register.
	SessionSecret string `json:"session_secret,omitempty"`
	// Kind is the agent role at register: "" (normal), "eyes", or "launcher".
	Kind string `json:"kind,omitempty"`
	// Runtime selects a host provider on OpRequestEyes: "claude" | "codex" | "grok".
	Runtime string `json:"runtime,omitempty"`
	// Brief is the eyes task document on OpRequestEyes.
	Brief string `json:"brief,omitempty"`
	// ReplyTo is the return-to-sender stamp. Callers omit it; the daemon fills
	// it from the authenticated session so results cannot be redirected.
	ReplyTo *AgentRef `json:"reply_to,omitempty"`
	// Target addresses OpSendWorkspace: Scope required, Name/AgentID optional for unicast.
	Target *AgentRef `json:"target,omitempty"`
	// TaskID identifies an eyes job (OpCancelEyes, and responses to OpRequestEyes).
	TaskID string `json:"task_id,omitempty"`
	// Platform / Capabilities are advertised at register by a host broker.
	Platform     string   `json:"platform,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// DeadlineS is the eyes-job deadline in seconds (300..1800).
	DeadlineS int `json:"deadline_s,omitempty"`
	// AuthSessionID is the launcher session that is minting a kind=eyes child.
	// SessionID names the new child; AuthSessionID+SessionSecret prove the launcher.
	AuthSessionID string `json:"auth_session_id,omitempty"`
}

// AgentRef is a structured address (never a smashed name@path string).
type AgentRef struct {
	Name    string `json:"name,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	Scope   string `json:"scope,omitempty"`
}

const (
	TaskLaunch   = "task.launch"
	TaskCancel   = "task.cancel"
	TaskAccepted = "task.accepted"
	TaskResult   = "task.result"
	TaskFailed   = "task.failed"
)

// TaskLaunchMsg is the body the daemon delivers to a launcher inbox.
type TaskLaunchMsg struct {
	Type      string   `json:"type"`
	TaskID    string   `json:"task_id"`
	Runtime   string   `json:"runtime,omitempty"`
	Scope     string   `json:"scope"`
	Brief     string   `json:"brief"`
	ReplyTo   AgentRef `json:"reply_to"`
	DeadlineS int      `json:"deadline_s"`
}

// TaskCancelMsg is the body the daemon delivers to cancel an eyes job.
type TaskCancelMsg struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id"`
}

// TaskAcceptedMsg is the launcher ack to the requester.
type TaskAcceptedMsg struct {
	Type   string    `json:"type"`
	TaskID string    `json:"task_id"`
	Child  *AgentRef `json:"child,omitempty"`
}

// TaskResultMsg is the eyes report DMed to reply_to.
type TaskResultMsg struct {
	Type         string   `json:"type"`
	TaskID       string   `json:"task_id"`
	Status       string   `json:"status"`
	Summary      string   `json:"summary"`
	Observations []string `json:"observations"`
	Actions      []string `json:"actions"`
	Evidence     []string `json:"evidence"`
	Error        string   `json:"error,omitempty"`
}

// TaskFailedMsg is sent when the child dies without a result.
type TaskFailedMsg struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id"`
	Error  string `json:"error"`
}

// WorkspaceInfo is one occupancy row from OpListWorkspaces.
type WorkspaceInfo struct {
	Scope          string `json:"scope"`
	LiveAgents     int    `json:"live_agents"`
	EyesAgents     int    `json:"eyes_agents"`
	LauncherAgents int    `json:"launcher_agents,omitempty"`
	LastSeen       int64  `json:"last_seen,omitempty"`
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
	Kind         string   `json:"kind,omitempty"`
	Scope        string   `json:"scope,omitempty"`
	Origin       string   `json:"origin,omitempty"`
	Platform     string   `json:"platform,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
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
	ID        int64     `json:"id"`
	From      string    `json:"from"`
	Body      string    `json:"body"`
	SentAt    int64     `json:"sent_at"`
	Broadcast bool      `json:"broadcast"`
	ReplyTo   *AgentRef `json:"reply_to,omitempty"`
	FromScope string    `json:"from_scope,omitempty"` // set only when it differs from the message scope
	Kind      string    `json:"kind,omitempty"`
	TaskID    string    `json:"task_id,omitempty"`
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
	// Workspaces is the occupancy directory from OpListWorkspaces.
	Workspaces    []WorkspaceInfo `json:"workspaces,omitempty"`
	TaskID        string          `json:"task_id,omitempty"`
	Launcher      *AgentRef       `json:"launcher,omitempty"`
	SessionSecret string          `json:"session_secret,omitempty"`
}

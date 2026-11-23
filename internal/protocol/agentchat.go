package protocol

import "time"

// Agent chat: chatting with a subscription-backed agent CLI (claude, codex, grok, agy,
// cursor, copilot) through its structured headless protocol, never a TTY:
//
//   - claude: `-p --input-format stream-json --output-format stream-json` (persistent)
//   - grok, agy, copilot, cursor: Agent Client Protocol (ACP) over stdio (persistent)
//   - codex: `codex app-server` JSON-RPC (persistent)
//
// konsertd owns one engine process per chat session, normalizes everything the CLI emits
// into AgentChatEvent, and routes elicitations (permission requests, questions) to the
// user. See docs/design/agent-chat.md.

// Hub <-> worker messages.
const (
	// MsgAgentChatOpen (hub->worker) opens or resumes an engine session. Idempotent per ChatID.
	MsgAgentChatOpen = "agent.chat.open"
	// MsgAgentChatPrompt (hub->worker) sends one user turn.
	MsgAgentChatPrompt = "agent.chat.prompt"
	// MsgAgentChatRespond (hub->worker) answers a pending elicitation.
	MsgAgentChatRespond = "agent.chat.respond"
	// MsgAgentChatControl (hub->worker) changes model/effort/approval mode or cancels the turn.
	MsgAgentChatControl = "agent.chat.control"
	// MsgAgentChatClose (hub->worker) ends the engine process (the chat can be resumed later).
	MsgAgentChatClose = "agent.chat.close"
	// MsgAgentChatEvent (worker->hub) carries one normalized event.
	MsgAgentChatEvent = "agent.chat.event"

	// MsgAgentCapabilities (worker->hub) reports every account's capabilities.
	MsgAgentCapabilities = "agent.capabilities"
	// MsgAgentCapabilitiesRefresh (hub->worker) asks for a fresh probe (optionally one account).
	MsgAgentCapabilitiesRefresh = "agent.capabilities.refresh"
)

// AgentChatOpen opens (or re-attaches to) an engine session for ChatID.
type AgentChatOpen struct {
	ChatID       string `json:"chat_id"`
	Agent        string `json:"agent"`      // driver name: claude, codex, grok, agy, cursor, copilot, mock
	AccountID    string `json:"account_id"` // worker-local account id; "" = the agent's default account
	Cwd          string `json:"cwd"`
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
	ApprovalMode string `json:"approval_mode,omitempty"` // an AgentApprovalMode.ID of the agent
	// AgentSessionID resumes the CLI's own session (claude session_id, ACP sessionId,
	// codex thread id) when the engine process was restarted.
	AgentSessionID string `json:"agent_session_id,omitempty"`
	// SystemPrompt is appended to the agent's own system prompt (e.g. orchestrator role).
	SystemPrompt string `json:"system_prompt,omitempty"`
	// MCPServers are extra MCP servers exposed to the agent (e.g. Konsert fleet tools and
	// ask_user, served by the hub over HTTP with a per-chat bearer token).
	MCPServers []AgentMCPServer `json:"mcp_servers,omitempty"`
}

// AgentMCPServer is a Streamable HTTP MCP server handed to the agent.
type AgentMCPServer struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"` // e.g. Authorization; never logged
}

// AgentChatPrompt is one user turn.
type AgentChatPrompt struct {
	ChatID      string   `json:"chat_id"`
	TurnID      string   `json:"turn_id"`
	Text        string   `json:"text"`
	Attachments []string `json:"attachments,omitempty"` // worker-local file paths
}

// AgentChatRespond answers an elicitation raised by an EventElicitation.
type AgentChatRespond struct {
	ChatID        string `json:"chat_id"`
	ElicitationID string `json:"elicitation_id"`
	// OptionID is the chosen AgentElicitationOption.ID (e.g. "allow_once", "reject").
	OptionID string `json:"option_id,omitempty"`
	// Text is a free-form answer for question elicitations.
	Text string `json:"text,omitempty"`
}

// AgentChatControl adjusts a live session. Empty fields are left unchanged.
type AgentChatControl struct {
	ChatID       string `json:"chat_id"`
	Cancel       bool   `json:"cancel,omitempty"` // interrupt the in-flight turn
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
	ApprovalMode string `json:"approval_mode,omitempty"`
}

// AgentChatClose ends the engine process for ChatID.
type AgentChatClose struct {
	ChatID string `json:"chat_id"`
}

// Event kinds.
const (
	EventSessionReady  = "session_ready" // engine up; AgentSessionID, Models/Modes may be set
	EventStatus        = "status"        // Status changed (see ChatStatus*)
	EventMessageDelta  = "message_delta" // streamed assistant text chunk
	EventMessage       = "message"       // complete assistant message (replaces deltas of the same MessageID)
	EventThoughtDelta  = "thought_delta" // streamed reasoning/thinking text
	EventToolCall      = "tool_call"     // tool call started or updated (same ToolCall.ID)
	EventPlan          = "plan"          // agent plan / todo list
	EventElicitation   = "elicitation"   // the agent needs the user: permission or question
	EventElicitationOK = "elicitation_resolved"
	EventUsage         = "usage"      // token usage for the turn
	EventRateLimit     = "rate_limit" // subscription window state (from the CLI's own stream)
	EventTurnEnd       = "turn_end"   // the turn finished (StopReason)
	EventError         = "error"      // classified failure (Error)
)

// Chat statuses (the chat window's "working / needs you / done / failed" pill).
const (
	ChatStatusStarting  = "starting"  // engine process launching / authenticating
	ChatStatusIdle      = "idle"      // ready for the user's next message
	ChatStatusWorking   = "working"   // a turn is in flight
	ChatStatusNeedsYou  = "needs_you" // blocked on an elicitation
	ChatStatusDone      = "done"      // last turn ended normally
	ChatStatusFailed    = "failed"    // last turn failed (see Error)
	ChatStatusCancelled = "cancelled" // last turn was cancelled by the user
)

// AgentChatEvent is the single normalized event stream for every agent CLI.
type AgentChatEvent struct {
	ChatID string    `json:"chat_id"`
	TurnID string    `json:"turn_id,omitempty"`
	Seq    int64     `json:"seq"` // per-chat, monotonically increasing (worker-assigned)
	Kind   string    `json:"kind"`
	At     time.Time `json:"at"`

	Status         string `json:"status,omitempty"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	Text           string `json:"text,omitempty"`

	ToolCall    *AgentToolCall    `json:"tool_call,omitempty"`
	Plan        []AgentPlanItem   `json:"plan,omitempty"`
	Elicitation *AgentElicitation `json:"elicitation,omitempty"`
	Usage       *AgentTokenUsage  `json:"usage,omitempty"`
	RateLimit   *AgentRateLimit   `json:"rate_limit,omitempty"`
	Error       *AgentChatError   `json:"error,omitempty"`

	// StopReason on turn_end: end_turn, max_tokens, refusal, cancelled, error.
	StopReason string `json:"stop_reason,omitempty"`

	// Session details on session_ready (current model/mode as the agent reports them).
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
	ApprovalMode string `json:"approval_mode,omitempty"`
}

// AgentToolCall is a tool invocation shown as a card in the transcript.
type AgentToolCall struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`             // e.g. Bash, Edit, read_file
	Kind   string   `json:"kind,omitempty"`   // read, edit, execute, search, fetch, think, other
	Title  string   `json:"title,omitempty"`  // human summary, e.g. "Run: go test ./..."
	Status string   `json:"status,omitempty"` // pending, running, completed, failed
	Input  string   `json:"input,omitempty"`  // truncated JSON/text
	Output string   `json:"output,omitempty"` // truncated text
	Paths  []string `json:"paths,omitempty"`
}

// AgentPlanItem is one entry of the agent's plan / todo list.
type AgentPlanItem struct {
	Text   string `json:"text"`
	Status string `json:"status,omitempty"` // pending, in_progress, completed
}

// Elicitation kinds.
const (
	ElicitPermission = "permission" // approve a tool call / edit / command
	ElicitQuestion   = "question"   // the agent asks the user a question (choices and/or free text)
	ElicitAuth       = "auth"       // the account must (re)authenticate
)

// AgentElicitation is a request for the user's decision or input.
type AgentElicitation struct {
	ID       string                   `json:"id"`
	Kind     string                   `json:"kind"`
	Title    string                   `json:"title"`
	Detail   string                   `json:"detail,omitempty"`
	ToolCall *AgentToolCall           `json:"tool_call,omitempty"`
	Options  []AgentElicitationOption `json:"options,omitempty"`
	// AllowText means a free-form answer is accepted (questions).
	AllowText bool `json:"allow_text,omitempty"`
}

// AgentElicitationOption is one choice; Kind hints the UI: allow_once, allow_always,
// reject_once, reject_always, choice.
type AgentElicitationOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind,omitempty"`
}

// AgentTokenUsage is token accounting for a turn (when the CLI reports it).
type AgentTokenUsage struct {
	InputTokens       int64   `json:"input_tokens,omitempty"`
	OutputTokens      int64   `json:"output_tokens,omitempty"`
	CacheReadTokens   int64   `json:"cache_read_tokens,omitempty"`
	CacheCreateTokens int64   `json:"cache_create_tokens,omitempty"`
	CostUSD           float64 `json:"cost_usd,omitempty"` // API-equivalent cost, informational
}

// AgentRateLimit is one subscription window as reported by the CLI or a usage probe.
type AgentRateLimit struct {
	Window      string     `json:"window"`                 // five_hour, seven_day, seven_day_opus, monthly, daily, ...
	UsedPercent float64    `json:"used_percent,omitempty"` // 0-100
	ResetsAt    *time.Time `json:"resets_at,omitempty"`
	// State: ok, warning (approaching), exhausted (requests rejected until ResetsAt).
	State string `json:"state,omitempty"`
}

// Error kinds (AgentChatError.Kind).
const (
	ErrKindAuth       = "auth"        // not logged in / token expired: re-login needed
	ErrKindUsageLimit = "usage_limit" // subscription window exhausted: wait until ResetsAt
	ErrKindRateLimit  = "rate_limit"  // transient throttling: retry shortly
	ErrKindExit       = "exit"        // engine process exited unexpectedly (ExitCode, Stderr)
	ErrKindNotFound   = "not_installed"
	ErrKindModel      = "model"   // unknown/unavailable model
	ErrKindTimeout    = "timeout" // no progress within the watchdog
	ErrKindProtocol   = "protocol"
	ErrKindOther      = "other"
)

// AgentChatError explains why a turn or session failed, with what the user can do.
type AgentChatError struct {
	Kind     string     `json:"kind"`
	Message  string     `json:"message"`
	ExitCode *int       `json:"exit_code,omitempty"`
	Stderr   string     `json:"stderr,omitempty"`    // tail, secrets redacted
	ResetsAt *time.Time `json:"resets_at,omitempty"` // usage_limit
	Window   string     `json:"window,omitempty"`    // usage_limit: five_hour, seven_day, monthly...
	// Relogin is set for auth errors: how to fix it on that host.
	Relogin *AgentRelogin `json:"relogin,omitempty"`
	// Retryable means sending the message again may succeed (transient errors).
	Retryable bool `json:"retryable,omitempty"`
}

// AgentRelogin tells the user how to re-authenticate an account on its host.
type AgentRelogin struct {
	Command string            `json:"command"`       // e.g. "claude /login", "codex login --device-auth"
	Env     map[string]string `json:"env,omitempty"` // account profile env needed (no secrets), e.g. CLAUDE_CONFIG_DIR
	URL     string            `json:"url,omitempty"` // vendor help/login page
	Note    string            `json:"note,omitempty"`
}

// --- Capabilities ---------------------------------------------------------------------

// AgentCapabilitiesReport lists every agent account on a worker.
type AgentCapabilitiesReport struct {
	Accounts []AgentAccount `json:"accounts"`
}

// Auth states.
const (
	AuthOK        = "ok"
	AuthLoggedOut = "logged_out"
	AuthExpiring  = "expiring" // valid but expires soon
	AuthUnknown   = "unknown"
)

// AgentAccount is one subscription (or API key) usable by one agent CLI on a worker.
type AgentAccount struct {
	ID        string `json:"id"`         // worker-local, stable: "claude", "claude2", "grok"
	Agent     string `json:"agent"`      // driver name
	Label     string `json:"label"`      // "Claude", "Claude 2"
	IsDefault bool   `json:"is_default"` // the agent's default account on this worker
	Version   string `json:"version,omitempty"`

	Auth            AgentAuthStatus     `json:"auth"`
	Plan            string              `json:"plan,omitempty"` // e.g. max, pro, team (when known)
	Models          []AgentModel        `json:"models,omitempty"`
	ModelsSource    string              `json:"models_source,omitempty"` // discovered, cache, fallback
	DefaultModel    string              `json:"default_model,omitempty"`
	Approvals       []AgentApprovalMode `json:"approval_modes,omitempty"`
	DefaultApproval string              `json:"default_approval_mode,omitempty"`
	Usage           []AgentRateLimit    `json:"usage,omitempty"`
	// UsageNote explains missing usage (e.g. "Cursor exposes usage only in its TUI").
	UsageNote string    `json:"usage_note,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	// ChatSupported is false when this agent has no structured headless protocol.
	ChatSupported bool `json:"chat_supported"`
}

// AgentAuthStatus is an account's login state.
type AgentAuthStatus struct {
	State     string        `json:"state"`            // AuthOK, AuthLoggedOut, AuthExpiring, AuthUnknown
	Method    string        `json:"method,omitempty"` // oauth, setup_token, api_key, keychain
	Email     string        `json:"email,omitempty"`  // shown to the owner only; may be empty
	ExpiresAt *time.Time    `json:"expires_at,omitempty"`
	Detail    string        `json:"detail,omitempty"`
	Relogin   *AgentRelogin `json:"relogin,omitempty"`
}

// AgentModel is one selectable model with its effort levels.
type AgentModel struct {
	ID            string        `json:"id"`
	DisplayName   string        `json:"display_name"`
	Description   string        `json:"description,omitempty"`
	IsDefault     bool          `json:"is_default,omitempty"`
	Efforts       []AgentEffort `json:"efforts,omitempty"`
	DefaultEffort string        `json:"default_effort,omitempty"`
	ContextTokens int64         `json:"context_tokens,omitempty"`
}

// AgentEffort is one reasoning-effort level of a model.
type AgentEffort struct {
	ID    string `json:"id"`              // value passed to the CLI
	Label string `json:"label"`           // "Medium"
	Short string `json:"short,omitempty"` // "Med"
}

// AgentApprovalMode is one of the agent's own approval/permission modes.
type AgentApprovalMode struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	// Risk orders modes for sandbox caps and UI color: 0 read-only/plan, 1 asks,
	// 2 accepts edits, 3 auto/approve-for-me, 4 full access (never asks).
	Risk int    `json:"risk"`
	Icon string `json:"icon,omitempty"` // hand, edit, shield-check, shield-alert, eye
	// Disabled, when set, explains why the mode can't be used on this worker (e.g.
	// konsertd runs with --sandbox, which refuses full-access modes).
	Disabled string `json:"disabled,omitempty"`
}

// SandboxMaxRisk is the highest approval-mode risk a --sandbox worker allows.
const SandboxMaxRisk = 3

// SandboxDisabledReason is the AgentApprovalMode.Disabled text on sandboxed workers.
const SandboxDisabledReason = "This machine runs konsertd in sandbox mode, which refuses full-access modes."

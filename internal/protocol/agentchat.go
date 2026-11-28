package protocol

import "time"

// Agent chat: chatting with a subscription-backed agent CLI (claude, codex, grok, agy,
// cursor, copilot) through its structured headless protocol, never a TTY:
//
//   - claude: `-p --input-format stream-json --output-format stream-json` (persistent)
//   - grok, agy, copilot, cursor: Agent Client Protocol (ACP) over stdio (persistent)
//   - codex: `codex app-server` JSON-RPC (persistent)
//
// syred owns one engine process per chat session, normalizes everything the CLI emits
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
	// SystemPrompt is appended to the agent's own system prompt (e.g. commander role).
	SystemPrompt string `json:"system_prompt,omitempty"`
	// MCPServers are extra MCP servers exposed to the agent (e.g. Syre fleet tools and
	// ask_user, served by the hub with a per-chat bearer token and relayed by syred).
	MCPServers []AgentMCPServer `json:"mcp_servers,omitempty"`
	// ForkFrom marks a fork: when AgentSessionID is empty it starts this chat as a fork of another
	// session of the same account: a copy of that conversation that runs independently
	// (claude --resume X --fork-session, codex thread/fork, ACP session/fork). Used for
	// read-only side conversations about a question; the original stays undisturbed.
	ForkFrom string `json:"fork_from,omitempty"`
	// UseGitBash asks Windows chat engines to configure their shell tool to use
	// Git Bash when Git Bash is installed on the worker.
	UseGitBash bool `json:"use_git_bash,omitzero"`
	// TicketSID and Bindings identify a ticket-backed chat whose per-repo worktrees
	// (<root>/.syre/tickets/<ticket sid>/<repo>) are provisioned by syred before
	// opening the engine. The chat cwd is the ticket workdir, the parent of those
	// worktrees.
	TicketSID string            `json:"ticket_sid,omitempty"`
	Bindings  []TaskRepoBinding `json:"bindings,omitempty"`
	// Verifies are the verification commands syred runs when a ticket chat's turn
	// ends OK. Each RepoName selects that binding's worktree as cwd; an empty
	// RepoName runs Cmd once in the ticket workdir (a ticket-level verify_cmd when
	// the ticket has several bindings). Empty skips verification. All must pass.
	Verifies []AgentChatVerify `json:"verifies,omitempty"`
	// VerifyTimeoutSeconds bounds a single verification run (0 uses syred's default 20m).
	VerifyTimeoutSeconds int `json:"verify_timeout_seconds,omitzero"`
	// AutoApproveWorkdir instructs syred (for ACP and tool-prompting engines) to
	// automatically approve permission requests whose targets stay inside the ticket
	// workdir and are not obviously destructive, escalating everything else to needs_you.
	AutoApproveWorkdir bool `json:"auto_approve_workdir,omitzero"`
	// EngineWorktrees is filled by syred after provisioning, for in-process engines.
	// The hub does not send it.
	EngineWorktrees []TaskWorktree `json:"-"`
}

// AgentChatVerify is one verification command for a ticket chat.
type AgentChatVerify struct {
	// RepoName is the binding whose worktree is the command's cwd.
	// Empty means the ticket workdir (parent of the per-repo worktrees).
	RepoName string `json:"repo_name,omitempty"`
	Cmd      string `json:"cmd"`
}

// AgentMCPServer is a Streamable HTTP MCP server handed to the agent.
type AgentMCPServer struct {
	Name string `json:"name"`
	// URL is the server's URL. With Relay it is a path on the hub (MCPChatPathPrefix +
	// chat id) that syred rewrites to its loopback relay before the agent sees it.
	URL     string            `json:"url"`
	Relay   bool              `json:"relay,omitzero"`
	Headers map[string]string `json:"headers,omitempty"` // e.g. Authorization; never logged
	// ToolTimeoutSeconds is how long the agent's MCP client should wait for one tool
	// call (ask_user blocks until the user answers). 0 = the agent's default.
	ToolTimeoutSeconds int `json:"tool_timeout_seconds,omitzero"`
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
	Cancel       bool   `json:"cancel,omitzero"` // interrupt the in-flight turn
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
	ApprovalMode string `json:"approval_mode,omitempty"`
}

// AgentChatClose ends the engine process for ChatID.
type AgentChatClose struct {
	ChatID string `json:"chat_id"`
	// Discard also deletes what the engine stored for this chat's agent session (a
	// side-conversation fork that is no longer needed), when the agent supports it.
	Discard bool `json:"discard,omitzero"`
	// CancelTurn requests a graceful stop: cancel any in-flight turn, wait up to
	// GraceS (default 10, max 300) for it to finish, then close/kill the session.
	CancelTurn bool `json:"cancel_turn,omitzero"`
	GraceS     int  `json:"grace_s,omitzero"`
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
	// EventSubagent reports a sub-agent's lifecycle from the agent's own task events
	// (Subagent): started (with its title, and whether it runs in the background, i.e.
	// may outlive its Task call and the turn), progress, done. Live activity only: the
	// chat manager keeps it out of the transcript.
	EventSubagent = "subagent"
	EventError    = "error" // classified failure (Error)
	// EventUserMessage is a prompt the user typed (Text, MessageID), and EventTitle the
	// session title the agent derived (Text). Only observed sessions carry them: a
	// managed chat's prompts come from the hub, and its title is the chat's.
	EventUserMessage = "user_message"
	EventTitle       = "title"
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

	Status string `json:"status,omitempty"`
	// ParentToolCallID marks an event of a sub-agent: the parent's tool call that
	// started it (claude's parent_tool_use_id). Empty for the main agent.
	ParentToolCallID string `json:"parent_tool_call_id,omitempty"`
	AgentSessionID   string `json:"agent_session_id,omitempty"`
	MessageID        string `json:"message_id,omitempty"`
	Text             string `json:"text,omitempty"`

	ToolCall    *AgentToolCall    `json:"tool_call,omitzero"`
	Subagent    *AgentSubagent    `json:"subagent,omitzero"`
	Plan        []AgentPlanItem   `json:"plan,omitempty"`
	Elicitation *AgentElicitation `json:"elicitation,omitzero"`
	Usage       *AgentTokenUsage  `json:"usage,omitzero"`
	RateLimit   *AgentRateLimit   `json:"rate_limit,omitzero"`
	Error       *AgentChatError   `json:"error,omitzero"`

	// StopReason on turn_end: end_turn, max_tokens, refusal, cancelled, error.
	StopReason string `json:"stop_reason,omitempty"`

	// Session details on session_ready (current model/mode as the agent reports them).
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
	ApprovalMode string `json:"approval_mode,omitempty"`
	// Workdir and Worktrees are populated on session_ready for ticket-backed chats
	// after syred provisions per-binding worktrees.
	Workdir   string         `json:"workdir,omitempty"`
	Worktrees []TaskWorktree `json:"worktrees,omitempty"`
	// Digest is populated on turn_end, error, and elicitation events for ticket-backed chats.
	Digest *RunDigest `json:"digest,omitzero"`
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

// Sub-agent statuses (AgentSubagent.Status).
const (
	SubagentRunning = "running"
	SubagentDone    = "done"
)

// AgentSubagent is one sub-agent's lifecycle event (EventSubagent).
type AgentSubagent struct {
	// ID is the parent's tool call id that started it.
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status"`
	// Label is what it does now ("Running List files…"), when the agent says.
	Label string `json:"label,omitempty"`
	// Background: it runs on after its Task call returns (and after the turn).
	Background bool `json:"background,omitzero"`
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
	ToolCall *AgentToolCall           `json:"tool_call,omitzero"`
	Options  []AgentElicitationOption `json:"options,omitempty"`
	// AllowText means a free-form answer is accepted (questions).
	AllowText bool `json:"allow_text,omitzero"`
	// QuestionID links the hub's question object (q-…), set by the hub.
	QuestionID string `json:"question_id,omitempty"`
}

// AgentElicitationOption is one choice; Kind hints the UI: allow_once, allow_always,
// reject_once, reject_always, choice.
type AgentElicitationOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind,omitempty"`
	// Consequence says what happens if the user picks it; Recommended marks the
	// asker's recommendation (ask_user questions).
	Consequence string `json:"consequence,omitempty"`
	Recommended bool   `json:"recommended,omitzero"`
}

// AgentTokenUsage is token accounting for a turn (when the CLI reports it).
type AgentTokenUsage struct {
	InputTokens       int64   `json:"input_tokens,omitzero"`
	OutputTokens      int64   `json:"output_tokens,omitzero"`
	CacheReadTokens   int64   `json:"cache_read_tokens,omitzero"`
	CacheCreateTokens int64   `json:"cache_create_tokens,omitzero"`
	CostUSD           float64 `json:"cost_usd,omitzero"` // API-equivalent cost, informational
}

// AgentRateLimit is one subscription window as reported by the CLI or a usage probe.
type AgentRateLimit struct {
	Window      string     `json:"window"`                // five_hour, seven_day, seven_day_opus, monthly, daily, ...
	UsedPercent float64    `json:"used_percent,omitzero"` // 0-100
	ResetsAt    *time.Time `json:"resets_at,omitzero"`
	// State: ok, warning (approaching), exhausted (requests rejected until ResetsAt).
	State string `json:"state,omitempty"`
}

// Error kinds (AgentChatError.Kind and RunDigest.Failure.Category).
const (
	ErrKindAuth        = "auth"        // not logged in / token expired: re-login needed
	ErrKindUsageLimit  = "usage_limit" // subscription window exhausted: wait until ResetsAt
	ErrKindRateLimit   = "rate_limit"  // transient throttling: retry shortly
	ErrKindExit        = "exit"        // engine process exited unexpectedly (ExitCode, Stderr)
	ErrKindNotFound    = "not_installed"
	ErrKindModel       = "model"   // unknown/unavailable model
	ErrKindTimeout     = "timeout" // no progress within the watchdog
	ErrKindProtocol    = "protocol"
	ErrKindOther       = "other"
	ErrKindOOM         = "oom"          // out of memory (SIGKILL/137, Windows job memory)
	ErrKindDiskFull    = "disk_full"    // ENOSPC / out of disk space
	ErrKindMissingTool = "missing_tool" // command not found / exit 127
	ErrKindVerify      = "verify"       // verification command failed (reserved)
	ErrKindAgentError  = "agent_error"  // generic agent/task failure
	ErrKindCanceled    = "canceled"     // task was canceled
	ErrKindLost        = "lost"         // worker disappeared; the attempt was requeued or failed
	ErrKindConflict    = "conflict"     // git merge conflict while stacking blocker branches
	// ErrKindMemoryPressureShed: the machine's guardrails shed the session to keep the
	// host alive (docs/design/guardrails.md). Not a failure of the work: the hub
	// requeues it without spending a retry. Distinct from oom, which is the job itself
	// running out of memory.
	ErrKindMemoryPressureShed = "memory_pressure_shed"
	// FailureHostMemoryFull is the attempt category for a shed whose session held
	// little of the memory in use (ShedEvidence.Bystander): the machine was full for
	// other reasons. Requeued like any shed, but not counted toward escalation.
	FailureHostMemoryFull = "host_memory_full"
)

// AgentChatError explains why a turn or session failed, with what the user can do.
type AgentChatError struct {
	Kind     string     `json:"kind"`
	Message  string     `json:"message"`
	ExitCode *int       `json:"exit_code,omitzero"`
	Stderr   string     `json:"stderr,omitempty"`   // tail, secrets redacted
	ResetsAt *time.Time `json:"resets_at,omitzero"` // usage_limit
	Window   string     `json:"window,omitempty"`   // usage_limit: five_hour, seven_day, monthly...
	// Relogin is set for auth errors: how to fix it on that host.
	Relogin *AgentRelogin `json:"relogin,omitzero"`
	// Retryable means sending the message again may succeed (transient errors).
	Retryable bool `json:"retryable,omitzero"`
	// Shed is set with ErrKindMemoryPressureShed.
	Shed *ShedEvidence `json:"shed,omitzero"`
}

// ShedEvidence describes a session the guardrails shed under memory pressure.
type ShedEvidence struct {
	// Session is what was shed: "chat" (the agent engine) or "verify".
	Session string `json:"session"`
	// Reason: host_memory_below_reserve, kernel_memory_pressure_critical or
	// worker_pool_memory_stall, swap_growth or commit_limit_low (Windows).
	Reason      string `json:"reason"`
	RSSMB       uint64 `json:"rss_mb,omitzero"`
	HostAvailMB uint64 `json:"host_avail_mb,omitzero"`
	ReserveMB   uint64 `json:"reserve_mb,omitzero"`
	// ActiveSessions counts the managed sessions running at the shed, this one included.
	ActiveSessions int `json:"active_sessions,omitzero"`
	// HostUsedMB is the host's memory in use at the shed.
	HostUsedMB uint64 `json:"host_used_mb,omitzero"`
	// Bystander: the shed session held little of the memory in use, so the machine
	// was full for other reasons. The hub doesn't count it toward escalation.
	Bystander bool `json:"bystander,omitzero"`
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
	// Fork is true when the engine can fork a session (AgentChatOpen.ForkFrom): a copy
	// of the conversation that runs independently of the original.
	Fork bool `json:"fork,omitzero"`
}

// AgentAuthStatus is an account's login state.
type AgentAuthStatus struct {
	State     string        `json:"state"`            // AuthOK, AuthLoggedOut, AuthExpiring, AuthUnknown
	Method    string        `json:"method,omitempty"` // oauth, setup_token, api_key, keychain
	Email     string        `json:"email,omitempty"`  // shown to the owner only; may be empty
	ExpiresAt *time.Time    `json:"expires_at,omitzero"`
	Detail    string        `json:"detail,omitempty"`
	Relogin   *AgentRelogin `json:"relogin,omitzero"`
}

// AgentModel is one selectable model with its effort levels.
type AgentModel struct {
	ID            string        `json:"id"`
	DisplayName   string        `json:"display_name"`
	Description   string        `json:"description,omitempty"`
	IsDefault     bool          `json:"is_default,omitzero"`
	Efforts       []AgentEffort `json:"efforts,omitempty"`
	DefaultEffort string        `json:"default_effort,omitempty"`
	ContextTokens int64         `json:"context_tokens,omitzero"`
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
	// syred runs with --sandbox, which refuses full-access modes).
	Disabled string `json:"disabled,omitempty"`
}

// SandboxMaxRisk is the highest approval-mode risk a --sandbox worker allows.
const SandboxMaxRisk = 3

// SandboxDisabledReason is the AgentApprovalMode.Disabled text on sandboxed workers.
const SandboxDisabledReason = "This machine runs syred in sandbox mode, which refuses full-access modes."

// Observed session transcripts (SYR-XHAHC). The hub asks a daemon for an observed
// session's transcript when a browser opens its tab (MsgObservedTranscript, answered
// with the same message type and envelope id); with Watch the daemon then streams the
// session's new events (MsgObservedEvents) until the watch lapses. The hub relays them
// to browsers and keeps transcripts in memory only, for a short while (never stored).
const (
	MsgObservedTranscript = "observed.transcript"
	MsgObservedEvents     = "observed.events"
)

// ObservedTranscriptRequest asks for an observed session's transcript. Watch starts (or
// renews) streaming its new events; a watch lapses ObservedWatchTTL after its last renewal.
type ObservedTranscriptRequest struct {
	ExternalID string `json:"external_id"`
	Watch      bool   `json:"watch,omitzero"`
}

// ObservedWatchTTL is how long a watch lasts without renewal.
const ObservedWatchTTL = 5 * time.Minute

// ObservedTranscript is an observed session's transcript as the agent's own store holds
// it, as normalized events (Seq from 1 in file order; TurnID per prompt). Truncated: only
// the latest events are included. Available false: the daemon can't read it (Reason).
type ObservedTranscript struct {
	ExternalID string           `json:"external_id"`
	Available  bool             `json:"available"`
	Reason     string           `json:"reason,omitempty"`
	Truncated  bool             `json:"truncated,omitzero"`
	Events     []AgentChatEvent `json:"events,omitempty"`
}

// ObservedEvents are an observed session's new events (daemon to hub, hub to browsers;
// TaskID is filled in by the hub).
type ObservedEvents struct {
	ExternalID string           `json:"external_id"`
	TaskID     string           `json:"task_id,omitempty"`
	Events     []AgentChatEvent `json:"events"`
}

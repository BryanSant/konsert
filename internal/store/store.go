package store

import (
	"context"
	"errors"
	"time"

	"github.com/BryanSant/syre/internal/ids"
)

var (
	ErrNotFound         = errors.New("record not found")
	ErrProtectedDefault = errors.New("cannot delete built-in default resource")
)

const (
	DefaultProjectID = ids.DefaultProjectID
)

type PermissionMode string

const (
	PermissionFullAccess PermissionMode = "full_access" // Default ("YOLO")
	PermissionSandboxed  PermissionMode = "sandboxed"   // Opt-in restriction
)

type Worker struct {
	ID             string            `json:"id"`
	SID            string            `json:"sid"`
	Name           string            `json:"name"`
	HostGroup      string            `json:"host_group"`
	OS             string            `json:"os"`
	Arch           string            `json:"arch"`
	Isolation      string            `json:"isolation"`       // "vm", "container", "host"
	PermissionMode PermissionMode    `json:"permission_mode"` // "full_access" | "sandboxed"
	Agents         []string          `json:"agents"`
	Tags           map[string]string `json:"tags"`
	MaxConcurrent  int               `json:"max_concurrent"`
	Status         string            `json:"status"` // "online", "offline"
	LastSeenAt     time.Time         `json:"last_seen_at"`
	FactsJSON      string            `json:"facts_json,omitempty"`
	FactsUpdatedAt *time.Time        `json:"facts_updated_at,omitzero"`
	// Version is the syred build version last reported in worker.hello.
	Version string `json:"version,omitempty"`

	// Launch is how the hub (re)starts this machine's syred: LaunchManual (""),
	// LaunchLocal or LaunchSSH, which also picks the transport (local socket, SSH, or
	// outbound for manual). SSHTarget is what an SSH (re)start and the SSH transport
	// use. The fields are set by EnsureWorker/SetWorkerLaunchState, never by UpsertWorker,
	// so a worker.hello keeps them.
	Launch      string     `json:"launch,omitempty"`
	SSHTarget   string     `json:"ssh_target,omitempty"`
	LaunchState string     `json:"launch_state,omitempty"` // "" | "starting" | "failed"
	LaunchError string     `json:"launch_error,omitempty"`
	LaunchAt    *time.Time `json:"launch_at,omitzero"`

	// Drained machines finish their current work and receive nothing new (persisted).
	Drained   bool       `json:"drained"`
	DrainedAt *time.Time `json:"drained_at,omitzero"`

	// DefaultShell is the operator's preferred terminal shell ID for this machine
	// (e.g. "git-bash", "pwsh", "powershell", "zsh", "bash", "fish"); "" = worker default.
	DefaultShell string `json:"default_shell,omitempty"`

	// AgentsUseGitBash, on Windows machines with Git Bash installed, asks supported
	// agent drivers to point their shell tool at Git Bash (default true).
	AgentsUseGitBash bool `json:"agents_use_git_bash"`
}

// MachineCredential is one syred credential (SYR-RK9YD). The secret itself is never
// stored, only its hash (see internal/auth). A credential authenticates exactly its
// machine (WorkerID).
type MachineCredential struct {
	ID            string     `json:"id"`
	SID           string     `json:"sid"`
	WorkerID      string     `json:"worker_id"`
	Source        string     `json:"source"` // MachineCredJoin, MachineCredRotate
	CreatedAt     time.Time  `json:"created_at"`
	LastUsedAt    *time.Time `json:"last_used_at,omitzero"`
	RevokedAt     *time.Time `json:"revoked_at,omitzero"`
	RevokedReason string     `json:"revoked_reason,omitempty"`
}

// Machine credential sources: how the credential reached the machine.
const (
	MachineCredJoin   = "join"   // traded for a join code (syred join)
	MachineCredRotate = "rotate" // pushed to a connected syred by a rotation
)

// MachineJoinCode is a short-lived, single-use code a machine trades for a credential.
// Only the code's hash is stored.
type MachineJoinCode struct {
	ID        string     `json:"id"`
	CreatedBy string     `json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at,omitzero"`
	WorkerID  string     `json:"worker_id,omitempty"`
}

// Worker launch modes and states (workers.launch / workers.launch_state).
const (
	LaunchManual = ""
	LaunchLocal  = "local"
	LaunchSSH    = "ssh"

	LaunchStateNone     = ""
	LaunchStateStarting = "starting"
	LaunchStateFailed   = "failed"
)

// MemoryEntry persists compact Commander fleet/thread/project state in SQLite across context resets.
type MemoryEntry struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"` // "fleet" | "session:<id>" | "project:<id>"
	Key       string    `json:"key"`
	Summary   string    `json:"summary"`
	MetaJSON  string    `json:"meta_json,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Project is a conceptual product owning a repo registry, work hierarchy, and defaults
// (docs/design/work.md).
type Project struct {
	ID        string `json:"id"`
	SID       string `json:"sid"`
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
	// Headline is the project's current goal as the commander states it ("" = none;
	// the UI falls back to facts). HeadlineAt is when it was set.
	Headline        string         `json:"headline,omitempty"`
	HeadlineAt      *time.Time     `json:"headline_at,omitzero"`
	VerifyCmd       string         `json:"verify_cmd,omitempty"`
	Autonomy        AutonomyPolicy `json:"autonomy,omitzero"`
	Autopilot       bool           `json:"autopilot"`
	AutopilotPaused bool           `json:"autopilot_paused"`
	DeliveryMode    string         `json:"delivery_mode,omitempty"`
	MergeStyle      string         `json:"merge_style,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
}

// Repo is a per-project repository registry entry keyed by RemoteURL (docs/design/work.md).
// Per-machine checkouts and worktrees are reported as daemon facts, not stored here.
type Repo struct {
	ID            string    `json:"id"`
	SID           string    `json:"sid"`
	ProjectID     string    `json:"project_id"`
	Name          string    `json:"name"`
	RemoteURL     string    `json:"remote_url"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	VerifyCmd     string    `json:"verify_cmd,omitempty"`
	DeliveryMode  string    `json:"delivery_mode,omitempty"`
	MergeStyle    string    `json:"merge_style,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type Task struct {
	ID                 string         `json:"id"`
	SID                string         `json:"sid"`
	ExternalID         string         `json:"external_id,omitempty"` // worker's key for a discovered session
	WorkerID           string         `json:"worker_id"`
	ProjectID          string         `json:"project_id,omitempty"`
	RepoID             string         `json:"repo_id,omitempty"`
	Agent              string         `json:"agent"`
	Model              string         `json:"model,omitempty"`
	SessionKind        string         `json:"session_kind,omitempty"` // "chat" | "terminal"
	Workdir            string         `json:"workdir"`
	EffectiveDir       string         `json:"effective_dir"`
	Branch             string         `json:"branch"`
	Mode               string         `json:"mode"`            // "oneshot" | "interactive"
	Origin             string         `json:"origin"`          // "commander" | "direct" | "scratch"
	PermissionMode     PermissionMode `json:"permission_mode"` // "full_access" | "sandboxed"
	OrphanPolicy       string         `json:"orphan_policy"`   // "fail" | "requeue"
	Prompt             string         `json:"prompt"`
	Status             string         `json:"status"` // "queued","assigned","running","waiting_input","suspended","completed","failed","canceled"
	Summary            string         `json:"summary"`
	GitSummaryJSON     string         `json:"git_summary_json,omitempty"`
	DigestJSON         string         `json:"digest_json,omitempty"`
	Followups          []TaskFollowup `json:"followups,omitempty"`
	Worktrees          []TaskWorktree `json:"worktrees,omitempty"`
	WorktreesCleanedAt *time.Time     `json:"worktrees_cleaned_at,omitzero"`
	ExitCode           *int           `json:"exit_code,omitzero"`
	// Cause attributes the last status transition.
	Cause *Cause `json:"cause,omitzero"`
	// SuspendedBy is "user" or "pause" while the task is suspended ("" otherwise).
	SuspendedBy string `json:"suspended_by"`
	// ParentChatID is the chat (commander or agent) that dispatched the task.
	ParentChatID string `json:"parent_chat_id,omitempty"`
	// Title is a short name for the task; Narration the commander's one-line "now"
	// (NarrationAt when it was set); TicketID/TicketTitle the ka ticket it works on.
	Title       string     `json:"title,omitempty"`
	Narration   string     `json:"narration,omitempty"`
	NarrationAt *time.Time `json:"narration_at,omitzero"`
	TicketID    string     `json:"ticket_id,omitempty"`
	TicketTitle string     `json:"ticket_title,omitempty"`
	// Keep: the user pinned the task, so retention never compacts it. CompactedAt is
	// when retention dropped its verbose events (milestones are kept).
	Keep        bool       `json:"keep"`
	CompactedAt *time.Time `json:"compacted_at,omitzero"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitzero"`
	CompletedAt *time.Time `json:"completed_at,omitzero"`
}

// TaskStartSpec is the persisted placement and launch specification of a task:
// the JSON-encoded worker selector and task.start payload, plus whether the
// worker was explicitly pinned (so requeue keeps it instead of clearing it).
type TaskStartSpec struct {
	SelectorJSON string `json:"selector_json,omitempty"`
	PayloadJSON  string `json:"payload_json,omitempty"`
	WorkerPinned bool   `json:"worker_pinned,omitzero"`
}

type TaskEvent struct {
	ID        int64     `json:"id"`
	TaskID    string    `json:"task_id"`
	Kind      string    `json:"kind"` // "milestone","tool_call","file_edit","control","error","exit"
	Message   string    `json:"message"`
	MetaJSON  string    `json:"meta_json,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// AuditEvent is one operator control action in the fleet-wide audit log: who
// (User: the authenticated browser/Tailscale identity, "token" for admin bearer
// callers, "mcp" for MCP clients), what (Action, Message), when, and which host
// and task it touched. MetaJSON never carries secret values.
type AuditEvent struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	User      string    `json:"user"`
	Source    string    `json:"source,omitempty"` // "rest" | "ws" | "mcp" | "commander" | "cli" | "legacy"
	Action    string    `json:"action"`           // e.g. "task.steer", "task.cancel", "task.take_control"
	WorkerID  string    `json:"worker_id,omitempty"`
	WorkerSID string    `json:"worker_sid,omitempty"`
	TaskID    string    `json:"task_id,omitempty"`
	TaskSID   string    `json:"task_sid,omitempty"`
	Message   string    `json:"message,omitempty"`
	MetaJSON  string    `json:"meta_json,omitempty"`
	// Cause attributes the action (actor kind, path, confidence, correlation).
	Cause *Cause `json:"cause,omitzero"`
}

// Machine vitals and heat retention (SYR-WY5F2): raw samples, hourly roll-ups and daily
// roll-ups, each kept for a fixed time, so a machine never has more than ~4k rows.
const (
	// VitalsBucket is the raw tier's resolution: heartbeats within one bucket merge.
	VitalsBucket = 15 * time.Second
	// VitalsRawKeep, VitalsHourlyKeep and VitalsDailyKeep are how long each tier is kept.
	VitalsRawKeep    = 12 * time.Hour
	VitalsHourlyKeep = 30 * 24 * time.Hour
	VitalsDailyKeep  = 365 * 24 * time.Hour
	// VitalsHourly and VitalsDaily are the roll-up periods.
	VitalsHourly = time.Hour
	VitalsDaily  = 24 * time.Hour
)

// VitalsSample is one machine's raw vitals over one VitalsBucket: the mean of the N
// heartbeats in it, and the highest smoothed heat with its cause.
type VitalsSample struct {
	WorkerID string
	At       time.Time // bucket start (UTC, a multiple of VitalsBucket)
	N        int
	CPU      float64 // %
	Load     float64
	Mem      float64 // MB used
	MemTotal uint64  // MB
	Swap     float64 // MB used
	// GPU is the mean GPU % of the GPUN heartbeats that carried one (nil: none did).
	GPU     *float64
	GPUN    int
	Heat    float64 // mean smoothed heat 0..1
	HeatMax float64
	Cause   string // heat cause at HeatMax
	// SpoolSeq is the highest daemon spool seq folded into the sample (0: none): storing
	// it advances the worker's applied seq in the same transaction.
	SpoolSeq uint64
}

// VitalsStat is a metric's min, sample-weighted mean and max over a roll-up period.
type VitalsStat struct {
	Min, Avg, Max float64
}

// VitalsRollup is one machine's vitals over an hour or a day.
type VitalsRollup struct {
	WorkerID string
	Period   time.Duration // VitalsHourly or VitalsDaily
	At       time.Time     // period start (UTC)
	N        int           // heartbeats folded in
	CPU      VitalsStat
	Load     VitalsStat
	Mem      VitalsStat
	MemTotal uint64
	Swap     VitalsStat
	GPU      *VitalsStat // nil when no heartbeat in the period had a GPU reading
	GPUN     int
	Heat     VitalsStat
	Cause    string // heat cause at Heat.Max
}

// VitalsRollupResult counts what one RollupVitals run did.
type VitalsRollupResult struct {
	Hourly, Daily int   // roll-up rows written (recomputed)
	Pruned        int64 // rows deleted past their tier's retention
}

// AuditFilter selects audit events. Zero values mean "no filter". Results are
// newest first; BeforeID pages toward older rows and AfterID fetches newer ones.
type AuditFilter struct {
	WorkerID string
	TaskID   string
	User     string
	Action   string
	Since    time.Time
	Until    time.Time
	BeforeID int64
	AfterID  int64
	Limit    int
}

// Chat backends: an agent CLI on a worker (subscription, the default) or the
// optional API-key commander.
const (
	ChatBackendAgent = "agent"
	ChatBackendAPI   = "api"
)

type ChatSession struct {
	ID        string    `json:"id"`
	SID       string    `json:"sid"`
	Title     string    `json:"title"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Agent chat fields (migration 012). Backend is ChatBackendAgent unless set.
	Backend        string `json:"backend"`
	WorkerID       string `json:"worker_id,omitempty"`
	Agent          string `json:"agent,omitempty"`
	AccountID      string `json:"account_id,omitempty"`
	Effort         string `json:"effort,omitempty"`
	ApprovalMode   string `json:"approval_mode,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	Status         string `json:"status,omitempty"`
	FleetTools     bool   `json:"fleet_tools"`
	LastErrorJSON  string `json:"last_error_json,omitempty"`
	// MCPTokenHash is the SHA-256 hex of the chat's MCP bearer token; never serialized.
	MCPTokenHash string `json:"-"`
	// Cause attributes the last status transition.
	Cause *Cause `json:"cause,omitzero"`
	// SuspendedBy is "user" or "pause" while the chat is suspended; SuspendedPrev is the
	// status restored on resume.
	SuspendedBy   string `json:"suspended_by"`
	SuspendedPrev string `json:"suspended_prev,omitempty"`
	// NotesCursor is the last fleet event id delivered to this chat as a note.
	NotesCursor int64 `json:"-"`
	// QuestionID marks a hidden side-conversation chat answering about that question;
	// ForkOf is the chat whose agent session it forked ("" for the commander fallback).
	QuestionID string `json:"question_id,omitempty"`
	ForkOf     string `json:"fork_of,omitempty"`
	// TaskID / TicketID link an agent chat session to the task and/or ticket it works on.
	TaskID   string `json:"task_id,omitempty"`
	TicketID string `json:"ticket_id,omitempty"`
	// Keep: the user pinned the chat, so retention never compacts it. CompactedAt is
	// when retention last dropped detailed events (tool calls, reasoning) of turns that
	// ended before CompactedBefore; the transcript itself is kept. Set by retention
	// only (UpdateChatSession leaves them alone).
	Keep            bool       `json:"keep"`
	CompactedAt     *time.Time `json:"compacted_at,omitzero"`
	CompactedBefore *time.Time `json:"compacted_before,omitzero"`
}

// ChatEvent is one stored agent chat event: JSON is the protocol.AgentChatEvent.
type ChatEvent struct {
	ChatID    string    `json:"chat_id"`
	Seq       int64     `json:"seq"`
	TurnID    string    `json:"turn_id,omitempty"`
	Kind      string    `json:"kind"`
	JSON      string    `json:"json"`
	CreatedAt time.Time `json:"created_at"`
}

// AgentAccountRecord is one worker's agent account (JSON of protocol.AgentAccount).
type AgentAccountRecord struct {
	WorkerID  string    `json:"worker_id"`
	AccountID string    `json:"account_id"`
	JSON      string    `json:"json"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ChatMessage struct {
	ID        int64     `json:"id"`
	SessionID string    `json:"session_id"`
	Role      string    `json:"role"` // "user","assistant","tool"
	Content   string    `json:"content"`
	ToolJSON  string    `json:"tool_json,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type UsageEvent struct {
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	AccountID    string    `json:"account_id,omitempty"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	CreatedAt    time.Time `json:"created_at"`
}

type UsageBucket struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Requests     int64  `json:"requests"`
}

type SubscriptionQuota struct {
	ID          string     `json:"id"`
	Provider    string     `json:"provider"`
	Label       string     `json:"label"`
	WindowLabel string     `json:"window_label,omitempty"`
	UsedPercent float64    `json:"used_percent"`
	ResetAt     *time.Time `json:"reset_at,omitzero"`
	Source      string     `json:"source,omitempty"`
	WorkerID    string     `json:"worker_id,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

type UsageSummary struct {
	TotalInputTokens  int64               `json:"total_input_tokens"`
	TotalOutputTokens int64               `json:"total_output_tokens"`
	TotalRequests     int64               `json:"total_requests"`
	ByProvider        []UsageBucket       `json:"by_provider"`
	Quotas            []SubscriptionQuota `json:"quotas"`
}

type Store interface {
	RetentionStore
	SpoolStore

	// NewSID mints a unique short ID (sid) of the given kind (see internal/ids). Create*
	// methods mint one themselves when the record's SID is empty; callers that need the sid
	// up front mint it here.
	NewSID(ctx context.Context, kind ids.Kind) (string, error)
	// ResolveID returns the id of the kind's record named by ref (its id or its sid), or
	// ErrNotFound.
	ResolveID(ctx context.Context, kind ids.Kind, ref string) (string, error)

	// UpsertWorker registers or refreshes a worker by id; its sid is minted on first sight
	// (when w.SID is empty) and never changes after.

	UpsertWorker(ctx context.Context, w Worker) error
	// EnsureWorker records how the hub launches a machine. A machine not seen yet is
	// created offline (with a minted sid) from w's identity fields, so a configured host
	// exists before its daemon ever connects; an existing row only gets w's Launch and
	// SSHTarget. Returns the stored row.
	EnsureWorker(ctx context.Context, w Worker) (Worker, error)
	// SetWorkerLaunchState records the outcome of a start attempt ("" clears it).
	SetWorkerLaunchState(ctx context.Context, id, state, errMsg string, at time.Time) error

	// Machine credentials (SYR-RK9YD). CreateMachineCredential stores c (minting its ids)
	// with the hex SHA-256 of its secret. GetMachineCredential returns a credential and
	// its secret hash, or ErrNotFound. ListMachineCredentials lists a machine's
	// credentials (every machine's when workerID is ""), newest first.
	CreateMachineCredential(ctx context.Context, c MachineCredential, secretHash string) (MachineCredential, error)
	GetMachineCredential(ctx context.Context, id string) (MachineCredential, string, error)
	ListMachineCredentials(ctx context.Context, workerID string) ([]MachineCredential, error)
	// TouchMachineCredential records a successful authentication.
	TouchMachineCredential(ctx context.Context, id string, at time.Time) error
	// RevokeMachineCredentials revokes every active credential of workerID except
	// exceptID ("" revokes all) and returns how many it revoked.
	RevokeMachineCredentials(ctx context.Context, workerID, exceptID, reason string, at time.Time) (int, error)
	// RevokeMachineCredential revokes one credential (no-op when already revoked).
	RevokeMachineCredential(ctx context.Context, id, reason string, at time.Time) error

	// Machine join codes. CreateMachineJoinCode stores c with the hex SHA-256 of the code
	// and deletes codes that expired over a day before c.CreatedAt. RedeemMachineJoinCode
	// atomically marks the unused, unexpired code with codeHash as used by workerID and
	// returns it (ErrNotFound when there is none). CountActiveMachineJoinCodes counts the
	// unused, unexpired codes.
	CreateMachineJoinCode(ctx context.Context, c MachineJoinCode, codeHash string) (MachineJoinCode, error)
	GetMachineJoinCode(ctx context.Context, id string) (MachineJoinCode, error)
	RedeemMachineJoinCode(ctx context.Context, codeHash, workerID string, at time.Time) (MachineJoinCode, error)
	CountActiveMachineJoinCodes(ctx context.Context, at time.Time) (int, error)
	UpdateWorkerFacts(ctx context.Context, id, factsJSON string, updatedAt time.Time) error
	// TouchWorker marks an existing worker online at seenAt. Unlike UpsertWorker it
	// never recreates a row, so a heartbeat racing a fleet removal can't resurrect it.
	TouchWorker(ctx context.Context, id string, seenAt time.Time) error
	GetWorker(ctx context.Context, id string) (Worker, error)
	ListWorkers(ctx context.Context) ([]Worker, error)
	DeleteWorker(ctx context.Context, id string) error
	MarkStaleWorkersOffline(ctx context.Context, cutoff time.Time) ([]string, error)
	// MarkWorkerOffline flips one online worker to offline and reports whether it changed
	// (its link died).
	MarkWorkerOffline(ctx context.Context, id string) (bool, error)

	EnsureDefaultProject(ctx context.Context) error

	CreateProject(ctx context.Context, p Project) error
	GetProject(ctx context.Context, id string) (Project, error)
	UpdateProject(ctx context.Context, p Project) error
	// SetProjectHeadline sets (or, with "", clears) a project's headline.
	SetProjectHeadline(ctx context.Context, id, headline string, at time.Time) error
	UpdateProjectDelivery(ctx context.Context, id, deliveryMode, mergeStyle string) error
	ListProjects(ctx context.Context) ([]Project, error)
	DeleteProject(ctx context.Context, id string) error

	CreateRepo(ctx context.Context, r Repo) error
	GetRepo(ctx context.Context, id string) (Repo, error)
	UpdateRepo(ctx context.Context, r Repo) error
	UpdateRepoDelivery(ctx context.Context, id, deliveryMode, mergeStyle, remoteURL string) error
	ListRepos(ctx context.Context, projectID string) ([]Repo, error)
	DeleteRepo(ctx context.Context, id string) error

	// Work model: tickets, DAG edges, repo bindings, external refs, and computed rollups.
	CreateTicket(ctx context.Context, t Ticket) (Ticket, error)
	GetTicket(ctx context.Context, id string) (Ticket, error)
	UpdateTicket(ctx context.Context, t Ticket) (Ticket, error)
	ListTickets(ctx context.Context, f TicketFilter) ([]Ticket, error)
	DeleteTicket(ctx context.Context, id string) error

	// AddTicketEdge records that ticketID is blocked by blockedByID, rejecting cycles.
	AddTicketEdge(ctx context.Context, ticketID, blockedByID string) error
	RemoveTicketEdge(ctx context.Context, ticketID, blockedByID string) error
	ListTicketEdges(ctx context.Context, ticketID string) ([]TicketEdge, error)
	// ListTicketEdgesByProject returns blocked_by edges for tickets in projectID.
	// An empty projectID returns every edge.
	ListTicketEdgesByProject(ctx context.Context, projectID string) ([]TicketEdge, error)
	// ListTicketBlocking returns edges where blockedByID blocks other tickets.
	ListTicketBlocking(ctx context.Context, blockedByID string) ([]TicketEdge, error)
	// ListLatestDigestJSON returns each ticket's newest non-empty task digest.
	ListLatestDigestJSON(ctx context.Context) ([]TicketDigestRaw, error)
	// LatestDigestJSON returns one ticket's newest non-empty task digest, or "" when it has none.
	LatestDigestJSON(ctx context.Context, ticketID string) (string, error)

	// Repo bindings per ticket.
	PutTicketRepoBinding(ctx context.Context, b TicketRepoBinding) error
	RemoveTicketRepoBinding(ctx context.Context, ticketID, repoID string) error
	ListTicketRepoBindings(ctx context.Context, ticketID string) ([]TicketRepoBinding, error)

	// External tracker links per ticket.
	PutExternalRef(ctx context.Context, ref ExternalRef) error
	RemoveExternalRef(ctx context.Context, ticketID, tracker, externalID string) error
	ListExternalRefs(ctx context.Context, ticketID string) ([]ExternalRef, error)
	// TicketByExternalRef returns the ticket linked to externalID in tracker.
	TicketByExternalRef(ctx context.Context, tracker, externalID string) (Ticket, error)
	// ListExternalRefsByTracker returns every ref of tracker.
	ListExternalRefsByTracker(ctx context.Context, tracker string) ([]ExternalRef, error)
	CreateTracker(ctx context.Context, t Tracker) error
	GetTracker(ctx context.Context, id string) (Tracker, error)
	ListTrackers(ctx context.Context, projectID string) ([]Tracker, error)
	UpdateTracker(ctx context.Context, t Tracker) error
	DeleteTracker(ctx context.Context, id string) error

	// Short comments posted on tickets (agent sessions and operators).
	CreateTicketComment(ctx context.Context, c TicketComment) (TicketComment, error)
	ListTicketComments(ctx context.Context, ticketID string) ([]TicketComment, error)

	// UpsertAttempt inserts or updates one attempt row. Attempt 0 assigns the next
	// number for the ticket (or, with no ticket, for the task). tickets.attempt is
	// raised to the written number and never lowered. A failed/lost write does not
	// replace a row that already finished.
	UpsertAttempt(ctx context.Context, a Attempt) (Attempt, error)
	// ListAttempts returns attempts for ticketID when it is set, otherwise for taskID,
	// ordered by attempt number. One of the two ids is required.
	ListAttempts(ctx context.Context, ticketID, taskID string) ([]Attempt, error)

	// Computed rollups (done/total, blocked, running) for initiative/epic containers; never stored.
	GetTicketRollup(ctx context.Context, id string) (TicketRollup, error)

	// Ticket leases & ready queue (SYR-DXMYY, docs/design/work.md).
	// ClaimTicket atomically transitions a dispatchable ready ticket to running with a lease.
	ClaimTicket(ctx context.Context, ticketID, workerID, taskID string, now time.Time, ttl time.Duration) (Ticket, bool, error)
	// ReleaseTicket clears a ticket's lease and optionally transitions it to nextStatus.
	ReleaseTicket(ctx context.Context, ticketID, nextStatus string) (Ticket, bool, error)
	// RenewLeases extends lease_expires_at and lease_heartbeat_at for active leases held by workerID.
	RenewLeases(ctx context.Context, workerID string, ticketIDs []string, now time.Time, ttl time.Duration) (int, error)
	// ExpireLeases transitions tickets whose leases expired at or before now per their orphan_policy.
	ExpireLeases(ctx context.Context, now time.Time) ([]ExpiredTicketLease, error)
	// ListDispatchableTickets returns ready, unblocked, repo-bound, unheld tickets ordered by priority then created_at.
	ListDispatchableTickets(ctx context.Context, projectID string, limit int) ([]Ticket, error)
	// ListLeasedTicketsByWorker returns tickets currently leased to workerID.
	ListLeasedTicketsByWorker(ctx context.Context, workerID string) ([]Ticket, error)

	// Create* insert a record; an empty ID gets a new UUIDv7 and an empty SID a fresh sid.
	CreateTask(ctx context.Context, t Task) error
	// UpsertExternalTask creates or updates the discovered session keyed by (WorkerID,
	// ExternalID) and returns its task id (id and sid minted on first sight; t.ID and
	// t.SID are ignored) and its previous status ("" when new). t.Cause is stored.
	UpsertExternalTask(ctx context.Context, t Task) (string, string, error)
	UpdateTaskAssignment(ctx context.Context, id, workerID, effectiveDir string) error
	// UpdateTaskWorktrees persists the worktree paths reported back by the daemon for a task.
	UpdateTaskWorktrees(ctx context.Context, id string, worktrees []TaskWorktree) error
	// UpdateTaskDigest persists the JSON-encoded post-run RunDigest reported by the daemon.
	UpdateTaskDigest(ctx context.Context, id, digestJSON string) error
	// AppendTaskFollowup appends a proposed follow-up to the task's followups_json array.
	AppendTaskFollowup(ctx context.Context, id string, f TaskFollowup) ([]TaskFollowup, error)
	// MarkTaskWorktreesCleaned records that the task's provisioned worktrees have been removed.
	MarkTaskWorktreesCleaned(ctx context.Context, id string, at time.Time) error
	// ListTasksByTicket returns tasks linked to ticketID, newest first.
	ListTasksByTicket(ctx context.Context, ticketID string) ([]Task, error)
	// ListTasksPendingWorktreeCleanup returns ticket-backed tasks with provisioned worktrees
	// that have not yet been cleaned up.
	ListTasksPendingWorktreeCleanup(ctx context.Context, limit int) ([]Task, error)
	// UpdateTaskStatus sets the status (and summary, git summary, exit code when given)
	// with the cause of the transition, and returns the previous status.
	UpdateTaskStatus(ctx context.Context, id, status, summary, gitSummaryJSON string, exitCode *int, cause Cause) (string, error)
	// SetTaskSuspendedBy records who suspended a task ("" when it is not suspended).
	SetTaskSuspendedBy(ctx context.Context, id, by string) error
	// SetTaskParentChat records the chat that dispatched a task.
	SetTaskParentChat(ctx context.Context, id, chatID string) error
	// SetTaskNarration sets (or, with "", clears) a task's one-line narration.
	SetTaskNarration(ctx context.Context, id, narration string, at time.Time) error
	// SetTaskTicket records the ka ticket a task works on (title may be filled in later).
	SetTaskTicket(ctx context.Context, id, ticketID, ticketTitle string) error
	GetTask(ctx context.Context, id string) (Task, error)
	ListTasks(ctx context.Context, status, workerID string, limit int) ([]Task, error)

	// Task placement persistence and atomic scheduling transitions.
	SetTaskStartSpec(ctx context.Context, taskID string, spec TaskStartSpec) error
	GetTaskStartSpec(ctx context.Context, taskID string) (TaskStartSpec, error)
	ClaimQueuedTask(ctx context.Context, taskID, workerID string, cause Cause) (bool, error)
	RequeueTask(ctx context.Context, taskID, summary string, keepWorker bool, cause Cause) (bool, error)

	AppendEvent(ctx context.Context, e TaskEvent) (int64, error)
	ListEvents(ctx context.Context, taskID string, afterID int64, limit int) ([]TaskEvent, error)

	// Fleet-wide operator audit log.
	AppendAudit(ctx context.Context, e AuditEvent) (int64, error)
	ListAudit(ctx context.Context, f AuditFilter) ([]AuditEvent, error)
	// PruneAudit deletes rows older than olderThan and, when maxRows > 0, all but
	// the newest maxRows rows. It returns the number of rows deleted.
	PruneAudit(ctx context.Context, olderThan time.Time, maxRows int) (int64, error)

	// Machine vitals and heat (SYR-WY5F2). UpsertVitals merges raw samples into stored
	// buckets (sample-weighted means, max heat and its cause); samples of machines that
	// no longer exist are dropped.
	UpsertVitals(ctx context.Context, samples []VitalsSample) error
	// ListVitals returns every machine's raw samples in [since, until), oldest first.
	ListVitals(ctx context.Context, since, until time.Time) ([]VitalsSample, error)
	// ListVitalsRollups returns every machine's roll-ups of period starting in
	// [since, until), oldest first.
	ListVitalsRollups(ctx context.Context, period time.Duration, since, until time.Time) ([]VitalsRollup, error)
	// RollupVitals recomputes the hourly roll-up of every complete hour still in the raw
	// tier and the daily roll-up of every complete day those hours touch, then deletes
	// rows past each tier's retention, all as of now. Recomputing from the finer tier
	// makes it idempotent: running it twice, late, or after a gap gives the same rows.
	RollupVitals(ctx context.Context, now time.Time) (VitalsRollupResult, error)

	CreateChatSession(ctx context.Context, s ChatSession) error
	GetChatSession(ctx context.Context, id string) (ChatSession, error)
	ListChatSessions(ctx context.Context, limit int) ([]ChatSession, error)
	RenameChatSession(ctx context.Context, id, title string) error
	DeleteChatSession(ctx context.Context, id string) error
	AppendChatMessage(ctx context.Context, m ChatMessage) (int64, error)
	ListChatMessages(ctx context.Context, sessionID string) ([]ChatMessage, error)
	// UpdateChatSession writes every mutable column of an existing session
	// (title, provider, model and the agent chat fields) and bumps updated_at.
	UpdateChatSession(ctx context.Context, s ChatSession) error
	// ListChatSessionsByWorker returns the agent chat sessions hosted on workerID.
	ListChatSessionsByWorker(ctx context.Context, workerID string) ([]ChatSession, error)
	// GetChatSessionByTaskID returns the agent chat session linked to taskID.
	GetChatSessionByTaskID(ctx context.Context, taskID string) (ChatSession, error)
	// ListChatSessionsByTicket returns the agent chat sessions linked to ticketID, newest first.
	ListChatSessionsByTicket(ctx context.Context, ticketID string) ([]ChatSession, error)

	// Agent chat events. AppendChatEvent ignores a duplicate (chat_id, seq) and
	// reports whether the row was inserted.
	AppendChatEvent(ctx context.Context, e ChatEvent) (bool, error)
	ListChatEvents(ctx context.Context, chatID string, afterSeq int64, limit int) ([]ChatEvent, error)
	MaxChatEventSeq(ctx context.Context, chatID string) (int64, error)
	// CoalesceChatTurn replaces a finished turn's streamed deltas with complete
	// events: message deltas are dropped when the turn has a message event (or
	// folded into one), thought deltas are folded into one thought_delta.
	CoalesceChatTurn(ctx context.Context, chatID, turnID string) error

	// Agent accounts reported by workers (agent.capabilities).
	UpsertAgentAccount(ctx context.Context, a AgentAccountRecord) error
	// ListAgentAccounts lists one worker's accounts, or every worker's when workerID is "".
	ListAgentAccounts(ctx context.Context, workerID string) ([]AgentAccountRecord, error)
	DeleteAgentAccounts(ctx context.Context, workerID string) error

	RecordUsage(ctx context.Context, u UsageEvent) error
	GetUsageSummary(ctx context.Context) (UsageSummary, error)
	UpsertSubscriptionQuota(ctx context.Context, q SubscriptionQuota) error
	ListSubscriptionQuotas(ctx context.Context) ([]SubscriptionQuota, error)

	GetAllSettings(ctx context.Context) (map[string]string, error)
	SetSettings(ctx context.Context, kv map[string]string) error

	// Human control: drain, fleet timeline, holds and open requests.
	SetWorkerDrained(ctx context.Context, id string, drained bool, at time.Time) error
	SetWorkerDefaultShell(ctx context.Context, id, shell string) error
	SetWorkerAgentsUseGitBash(ctx context.Context, id string, enabled bool) error
	AppendFleetEvent(ctx context.Context, e FleetEvent) (int64, error)
	ListFleetEvents(ctx context.Context, f FleetEventFilter) ([]FleetEvent, error)
	MaxFleetEventID(ctx context.Context) (int64, error)
	CreateHold(ctx context.Context, h Hold) (Hold, error)
	GetHold(ctx context.Context, id string) (Hold, error)
	ListHolds(ctx context.Context, f HoldFilter) ([]Hold, error)
	// ReleaseHold marks an active hold released; false when it was already released.
	ReleaseHold(ctx context.Context, id, by, note string, at time.Time) (bool, error)
	CreateUserRequest(ctx context.Context, r UserRequest) (UserRequest, error)
	GetUserRequest(ctx context.Context, id string) (UserRequest, error)
	ListUserRequests(ctx context.Context, f UserRequestFilter) ([]UserRequest, error)
	// ResolveUserRequest closes an open request; false when it was no longer open.
	ResolveUserRequest(ctx context.Context, id, status, by, resolution, commandID string, at time.Time) (bool, error)

	// Override requests (break-glass) and vetoes awaiting acknowledgement.
	CreateOverride(ctx context.Context, o Override) (Override, error)
	GetOverride(ctx context.Context, id string) (Override, error)
	ListOverrides(ctx context.Context, f OverrideFilter) ([]Override, error)
	// DecideOverride approves (expiresAt set) or denies a pending override; false when
	// it was no longer pending.
	DecideOverride(ctx context.Context, id, status, by, note string, at time.Time, expiresAt *time.Time) (bool, error)
	// ExpireOverride ends an approved override; false when it was not approved.
	ExpireOverride(ctx context.Context, id string, at time.Time) (bool, error)
	// MarkOverrideUsed counts one action the override let through.
	MarkOverrideUsed(ctx context.Context, id string, at time.Time) error
	CreateVeto(ctx context.Context, v Veto) (Veto, error)
	GetVeto(ctx context.Context, id string) (Veto, error)
	ListVetoes(ctx context.Context, f VetoFilter) ([]Veto, error)
	// MarkVetoesDelivered records that the open vetoes of these fleet events were told to
	// chatID in turn turnID; it returns the vetoes it updated.
	MarkVetoesDelivered(ctx context.Context, fleetEventIDs []int64, chatID, turnID string, at time.Time) ([]Veto, error)
	// AckVetoes acknowledges the open vetoes last delivered in chatID's turn turnID and
	// returns them.
	AckVetoes(ctx context.Context, chatID, turnID string, at time.Time) ([]Veto, error)
	// EscalateVeto marks an open, unescalated veto escalated; false otherwise.
	EscalateVeto(ctx context.Context, id string, at time.Time) (bool, error)
	// DismissVeto closes an open veto without acknowledgement; false when not open.
	DismissVeto(ctx context.Context, id, by string, at time.Time) (bool, error)

	// Policy directives (docs/design/autopilot.md).
	CreateDirective(ctx context.Context, d Directive) (Directive, error)
	GetDirective(ctx context.Context, id string) (Directive, error)
	ListDirectives(ctx context.Context, f DirectiveFilter) ([]Directive, error)
	// EndDirective stamps ended_at when the directive is still open; false when it
	// was already ended (the timer and the reaper may both pass).
	EndDirective(ctx context.Context, id, by string, at time.Time) (bool, error)

	// Policy proposals: Commander set_policy changes waiting for the user (confirm mode).
	CreatePolicyProposal(ctx context.Context, p PolicyProposal) (PolicyProposal, error)
	GetPolicyProposal(ctx context.Context, id string) (PolicyProposal, error)
	ListPolicyProposals(ctx context.Context, f PolicyProposalFilter) ([]PolicyProposal, error)
	// DecidePolicyProposal approves or denies a pending proposal. appliedID is the
	// directive created on approval. false when it was no longer pending.
	DecidePolicyProposal(ctx context.Context, id, status, by, appliedID string, at time.Time) (bool, error)

	// Questions (PRD §5.8b).
	CreateQuestion(ctx context.Context, q Question) (Question, error)
	GetQuestion(ctx context.Context, id string) (Question, error)
	// UpdateQuestion writes every mutable field of an existing question.
	UpdateQuestion(ctx context.Context, q Question) error
	ListQuestions(ctx context.Context, f QuestionFilter) ([]Question, error)
	// AppendQuestionMessage adds a side-conversation message (seq assigned).
	AppendQuestionMessage(ctx context.Context, m QuestionMessage) (QuestionMessage, error)
	ListQuestionMessages(ctx context.Context, questionID string) ([]QuestionMessage, error)
	AddQuestionReject(ctx context.Context, r QuestionReject) error
	ListQuestionRejects(ctx context.Context, since time.Time) ([]QuestionReject, error)

	// Reviews (SYR-H50YW): one per task or chat. GetReview takes a task id or a chat id
	// (the other "") and is ErrNotFound when the session has none. SaveReview inserts or
	// updates the session's review (id/sid assigned on insert, updated_at bumped).
	GetReview(ctx context.Context, taskID, chatID string) (Review, error)
	SaveReview(ctx context.Context, r Review) (Review, error)
	// ListReviews returns every review, oldest first.
	ListReviews(ctx context.Context) ([]Review, error)
	// MarkReviewCommentSeen records a PR/MR review comment as processed for taskID.
	// Returns true if it was not seen before, false if already recorded.
	MarkReviewCommentSeen(ctx context.Context, taskID, commentID string, at time.Time) (bool, error)

	// Devices (SYR-W0P8B). CreateDevice assigns id/sid when empty. GetDeviceBySecretHash
	// returns revoked and expired devices too: callers decide. ListDevices returns the
	// devices that are not revoked, most recently used first.
	CreateDevice(ctx context.Context, d Device) (Device, error)
	GetDevice(ctx context.Context, id string) (Device, error)
	GetDeviceBySecretHash(ctx context.Context, hash string) (Device, error)
	ListDevices(ctx context.Context) ([]Device, error)
	// TouchDevice records a use: last_used_at, the sliding expires_at, and where from.
	TouchDevice(ctx context.Context, id string, at, expiresAt time.Time, ip, userAgent string) error
	RenameDevice(ctx context.Context, id, name string) error
	// RevokeDevice revokes a device; false when it was not active.
	RevokeDevice(ctx context.Context, id string, at time.Time) (bool, error)

	// Pairing codes (SYR-V5JZB, SYR-A29CT). CreatePairingCode assigns id/sid when empty.
	CreatePairingCode(ctx context.Context, p PairingCode) (PairingCode, error)
	GetPairingCodeByHash(ctx context.Context, codeHash string) (PairingCode, error)
	GetPairingCodeByPollHash(ctx context.Context, pollHash string) (PairingCode, error)
	// DecidePairingCode approves or denies a pending, unexpired login code; false when it
	// was not pending.
	DecidePairingCode(ctx context.Context, id string, approve bool, by string, at time.Time) (bool, error)
	// RedeemPairingCode atomically marks a code used and creates its device. It succeeds
	// once, only before expiry, and (for login codes) only after approval; otherwise it
	// returns false and creates nothing.
	RedeemPairingCode(ctx context.Context, codeID string, d Device, at time.Time) (Device, bool, error)
	// PruneAuth deletes pairing codes that expired before cutoff and devices revoked
	// before it (expired devices stay: they are rejected and visible as expired).
	PruneAuth(ctx context.Context, cutoff time.Time) error

	UpsertMemory(ctx context.Context, m MemoryEntry) error
	ListMemory(ctx context.Context, scope string, limit int) ([]MemoryEntry, error)
	DeleteMemory(ctx context.Context, scope, key string) error

	Close() error
}

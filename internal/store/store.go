package store

import (
	"context"
	"errors"
	"time"

	"github.com/BryanSant/konsert/internal/ids"
)

var (
	ErrNotFound         = errors.New("record not found")
	ErrProtectedDefault = errors.New("cannot delete built-in default resource")
)

const (
	DefaultWorkspaceID = ids.DefaultWorkspaceID
	DefaultProjectID   = ids.DefaultProjectID
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
	// Version is the konsertd build version last reported in worker.hello.
	Version string `json:"version,omitempty"`

	// Launch is how the hub (re)starts this machine's konsertd: LaunchManual (""),
	// LaunchLocal or LaunchSSH. SSHTarget and ControllerURL are what an SSH (re)start
	// uses. The fields are set by EnsureWorker/SetWorkerLaunchState, never by
	// UpsertWorker, so a worker.hello keeps them.
	Launch        string     `json:"launch,omitempty"`
	SSHTarget     string     `json:"ssh_target,omitempty"`
	ControllerURL string     `json:"controller_url,omitempty"`
	LaunchState   string     `json:"launch_state,omitempty"` // "" | "starting" | "failed"
	LaunchError   string     `json:"launch_error,omitempty"`
	LaunchAt      *time.Time `json:"launch_at,omitzero"`
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

// MemoryEntry persists compact Orchestrator fleet/thread/project state in SQLite across context resets.
type MemoryEntry struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"` // "fleet" | "session:<id>" | "project:<id>"
	Key       string    `json:"key"`
	Summary   string    `json:"summary"`
	MetaJSON  string    `json:"meta_json,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Space struct {
	ID        string    `json:"id"`
	SID       string    `json:"sid"`
	WorkerID  string    `json:"worker_id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Branch    string    `json:"branch,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Workspace is the top-level abstract "Space" in Konsert's Space > Project > Repo > Worktree taxonomy.
type Workspace struct {
	ID        string    `json:"id"`
	SID       string    `json:"sid"`
	Name      string    `json:"name"`
	IsDefault bool      `json:"is_default"`
	CreatedAt time.Time `json:"created_at"`
}

// Project groups one or more Repos (supporting multi-repo projects across machines) inside a Workspace.
type Project struct {
	ID          string    `json:"id"`
	SID         string    `json:"sid"`
	WorkspaceID string    `json:"workspace_id"`
	Name        string    `json:"name"`
	IsDefault   bool      `json:"is_default"`
	CreatedAt   time.Time `json:"created_at"`
}

// Repo represents a repository or working folder on a specific machine belonging to a Project.
type Repo struct {
	ID        string    `json:"id"`
	SID       string    `json:"sid"`
	ProjectID string    `json:"project_id"`
	WorkerID  string    `json:"worker_id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	RemoteURL string    `json:"remote_url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Worktree represents a branch checkout or git worktree within a Repo.
type Worktree struct {
	ID        string    `json:"id"`
	SID       string    `json:"sid"`
	RepoID    string    `json:"repo_id"`
	Branch    string    `json:"branch,omitempty"`
	Path      string    `json:"path"`
	IsPrimary bool      `json:"is_primary"`
	CreatedAt time.Time `json:"created_at"`
}

// TaxonomySnapshot bundles the complete Space > Project > Repo > Worktree hierarchy.
type TaxonomySnapshot struct {
	Workspaces []Workspace `json:"workspaces"`
	Projects   []Project   `json:"projects"`
	Repos      []Repo      `json:"repos"`
	Worktrees  []Worktree  `json:"worktrees"`
}

type Task struct {
	ID             string         `json:"id"`
	SID            string         `json:"sid"`
	ExternalID     string         `json:"external_id,omitempty"` // worker's key for a discovered session
	WorkerID       string         `json:"worker_id"`
	SpaceID        string         `json:"space_id,omitempty"`
	WorkspaceID    string         `json:"workspace_id,omitempty"`
	ProjectID      string         `json:"project_id,omitempty"`
	RepoID         string         `json:"repo_id,omitempty"`
	WorktreeID     string         `json:"worktree_id,omitempty"`
	Agent          string         `json:"agent"`
	Model          string         `json:"model,omitempty"`
	SessionKind    string         `json:"session_kind,omitempty"` // "chat" | "terminal"
	Workdir        string         `json:"workdir"`
	EffectiveDir   string         `json:"effective_dir"`
	Branch         string         `json:"branch"`
	Mode           string         `json:"mode"`            // "oneshot" | "interactive"
	Origin         string         `json:"origin"`          // "orchestrator" | "direct" | "scratch"
	PermissionMode PermissionMode `json:"permission_mode"` // "full_access" | "sandboxed"
	OrphanPolicy   string         `json:"orphan_policy"`   // "fail" | "requeue"
	Prompt         string         `json:"prompt"`
	Status         string         `json:"status"` // "queued","assigned","running","waiting_input","completed","failed","canceled"
	Summary        string         `json:"summary"`
	GitSummaryJSON string         `json:"git_summary_json,omitempty"`
	ExitCode       *int           `json:"exit_code,omitzero"`
	CreatedAt      time.Time      `json:"created_at"`
	StartedAt      *time.Time     `json:"started_at,omitzero"`
	CompletedAt    *time.Time     `json:"completed_at,omitzero"`
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
	Source    string    `json:"source,omitempty"` // "rest" | "ws" | "mcp" | "orchestrator" | "cli" | "legacy"
	Action    string    `json:"action"`           // e.g. "task.steer", "task.cancel", "task.take_control"
	WorkerID  string    `json:"worker_id,omitempty"`
	WorkerSID string    `json:"worker_sid,omitempty"`
	TaskID    string    `json:"task_id,omitempty"`
	TaskSID   string    `json:"task_sid,omitempty"`
	Message   string    `json:"message,omitempty"`
	MetaJSON  string    `json:"meta_json,omitempty"`
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
// optional API-key orchestrator.
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
	// exists before its daemon ever connects; an existing row only gets w's Launch,
	// SSHTarget and ControllerURL. Returns the stored row.
	EnsureWorker(ctx context.Context, w Worker) (Worker, error)
	// SetWorkerLaunchState records the outcome of a start attempt ("" clears it).
	SetWorkerLaunchState(ctx context.Context, id, state, errMsg string, at time.Time) error
	UpdateWorkerFacts(ctx context.Context, id, factsJSON string, updatedAt time.Time) error
	// TouchWorker marks an existing worker online at seenAt. Unlike UpsertWorker it
	// never recreates a row, so a heartbeat racing a fleet removal can't resurrect it.
	TouchWorker(ctx context.Context, id string, seenAt time.Time) error
	GetWorker(ctx context.Context, id string) (Worker, error)
	ListWorkers(ctx context.Context) ([]Worker, error)
	DeleteWorker(ctx context.Context, id string) error
	MarkStaleWorkersOffline(ctx context.Context, cutoff time.Time) ([]string, error)

	CreateSpace(ctx context.Context, sp Space) error
	GetSpace(ctx context.Context, id string) (Space, error)
	ListSpaces(ctx context.Context, workerID string) ([]Space, error)
	DeleteSpace(ctx context.Context, id string) error

	EnsureTaxonomyDefaults(ctx context.Context) error
	GetTaxonomy(ctx context.Context) (TaxonomySnapshot, error)

	CreateWorkspace(ctx context.Context, ws Workspace) error
	GetWorkspace(ctx context.Context, id string) (Workspace, error)
	ListWorkspaces(ctx context.Context) ([]Workspace, error)
	DeleteWorkspace(ctx context.Context, id string) error

	CreateProject(ctx context.Context, p Project) error
	GetProject(ctx context.Context, id string) (Project, error)
	ListProjects(ctx context.Context, workspaceID string) ([]Project, error)
	DeleteProject(ctx context.Context, id string) error

	CreateRepo(ctx context.Context, r Repo) error
	GetRepo(ctx context.Context, id string) (Repo, error)
	ListRepos(ctx context.Context, projectID, workerID string) ([]Repo, error)
	DeleteRepo(ctx context.Context, id string) error

	CreateWorktree(ctx context.Context, wt Worktree) error
	GetWorktree(ctx context.Context, id string) (Worktree, error)
	ListWorktrees(ctx context.Context, repoID string) ([]Worktree, error)
	DeleteWorktree(ctx context.Context, id string) error

	// Create* insert a record; an empty ID gets a new UUIDv7 and an empty SID a fresh sid.
	CreateTask(ctx context.Context, t Task) error
	// UpsertExternalTask creates or updates the discovered session keyed by (WorkerID,
	// ExternalID) and returns its task id (id and sid minted on first sight; t.ID and
	// t.SID are ignored).
	UpsertExternalTask(ctx context.Context, t Task) (string, error)
	UpdateTaskAssignment(ctx context.Context, id, workerID, effectiveDir string) error
	UpdateTaskStatus(ctx context.Context, id, status, summary, gitSummaryJSON string, exitCode *int) error
	GetTask(ctx context.Context, id string) (Task, error)
	ListTasks(ctx context.Context, status, workerID string, limit int) ([]Task, error)

	// Task placement persistence and atomic scheduling transitions.
	SetTaskStartSpec(ctx context.Context, taskID string, spec TaskStartSpec) error
	GetTaskStartSpec(ctx context.Context, taskID string) (TaskStartSpec, error)
	ClaimQueuedTask(ctx context.Context, taskID, workerID string) (bool, error)
	RequeueTask(ctx context.Context, taskID, summary string, keepWorker bool) (bool, error)

	AppendEvent(ctx context.Context, e TaskEvent) (int64, error)
	ListEvents(ctx context.Context, taskID string, afterID int64, limit int) ([]TaskEvent, error)

	// Fleet-wide operator audit log.
	AppendAudit(ctx context.Context, e AuditEvent) (int64, error)
	ListAudit(ctx context.Context, f AuditFilter) ([]AuditEvent, error)
	// PruneAudit deletes rows older than olderThan and, when maxRows > 0, all but
	// the newest maxRows rows. It returns the number of rows deleted.
	PruneAudit(ctx context.Context, olderThan time.Time, maxRows int) (int64, error)

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

	UpsertMemory(ctx context.Context, m MemoryEntry) error
	ListMemory(ctx context.Context, scope string, limit int) ([]MemoryEntry, error)
	DeleteMemory(ctx context.Context, scope, key string) error

	Close() error
}

package store

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound         = errors.New("record not found")
	ErrProtectedDefault = errors.New("cannot delete built-in default resource")
)

const (
	DefaultWorkspaceID = "space-default"
	DefaultProjectID   = "project-default"
)

type PermissionMode string

const (
	PermissionFullAccess PermissionMode = "full_access" // Default ("YOLO")
	PermissionSandboxed  PermissionMode = "sandboxed"   // Opt-in restriction
)

type Worker struct {
	ID             string            `json:"id"`
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
	FactsUpdatedAt *time.Time        `json:"facts_updated_at,omitempty"`
}

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
	WorkerID  string    `json:"worker_id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Branch    string    `json:"branch,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Workspace is the top-level abstract "Space" in Konsert's Space > Project > Repo > Worktree taxonomy.
type Workspace struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	IsDefault bool      `json:"is_default"`
	CreatedAt time.Time `json:"created_at"`
}

// Project groups one or more Repos (supporting multi-repo projects across machines) inside a Workspace.
type Project struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Name        string    `json:"name"`
	IsDefault   bool      `json:"is_default"`
	CreatedAt   time.Time `json:"created_at"`
}

// Repo represents a repository or working folder on a specific machine belonging to a Project.
type Repo struct {
	ID        string    `json:"id"`
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
	ExitCode       *int           `json:"exit_code,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	StartedAt      *time.Time     `json:"started_at,omitempty"`
	CompletedAt    *time.Time     `json:"completed_at,omitempty"`
}

// TaskStartSpec is the persisted placement and launch specification of a task:
// the JSON-encoded worker selector and task.start payload, plus whether the
// worker was explicitly pinned (so requeue keeps it instead of clearing it).
type TaskStartSpec struct {
	SelectorJSON string `json:"selector_json,omitempty"`
	PayloadJSON  string `json:"payload_json,omitempty"`
	WorkerPinned bool   `json:"worker_pinned,omitempty"`
}

type TaskEvent struct {
	ID        int64     `json:"id"`
	TaskID    string    `json:"task_id"`
	Kind      string    `json:"kind"` // "milestone","tool_call","file_edit","control","error","exit"
	Message   string    `json:"message"`
	MetaJSON  string    `json:"meta_json,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type ChatSession struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
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
	ResetAt     *time.Time `json:"reset_at,omitempty"`
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
	UpsertWorker(ctx context.Context, w Worker) error
	UpdateWorkerFacts(ctx context.Context, id, factsJSON string, updatedAt time.Time) error
	// TouchWorker marks an existing worker online at seenAt. Unlike UpsertWorker it
	// never recreates a row, so a heartbeat racing a fleet removal can't resurrect it.
	TouchWorker(ctx context.Context, id string, seenAt time.Time) error
	GetWorker(ctx context.Context, id string) (Worker, error)
	ListWorkers(ctx context.Context) ([]Worker, error)
	DeleteWorker(ctx context.Context, id string) error
	DeduplicateWorkers(ctx context.Context) ([]string, error)
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

	CreateTask(ctx context.Context, t Task) error
	UpsertExternalTask(ctx context.Context, t Task) error
	UpdateTaskAssignment(ctx context.Context, id, workerID, effectiveDir string) error
	UpdateTaskStatus(ctx context.Context, id, status, summary, gitSummaryJSON string, exitCode *int) error
	GetTask(ctx context.Context, id string) (Task, error)
	ListTasks(ctx context.Context, status, workerID string, limit int) ([]Task, error)

	// Task placement persistence and atomic scheduling transitions.
	SetTaskStartSpec(ctx context.Context, taskID string, spec TaskStartSpec) error
	GetTaskStartSpec(ctx context.Context, taskID string) (TaskStartSpec, error)
	ClaimQueuedTask(ctx context.Context, taskID, workerID string) (bool, error)
	RequeueTask(ctx context.Context, taskID, summary string, keepWorker bool) (bool, error)

	// Stable konsertd machine identity (WorkerInfo.MachineID).
	SetWorkerMachineID(ctx context.Context, workerID, machineID string) error
	ListWorkerMachineIDs(ctx context.Context) (map[string]string, error)

	AppendEvent(ctx context.Context, e TaskEvent) (int64, error)
	ListEvents(ctx context.Context, taskID string, afterID int64, limit int) ([]TaskEvent, error)

	CreateChatSession(ctx context.Context, s ChatSession) error
	GetChatSession(ctx context.Context, id string) (ChatSession, error)
	ListChatSessions(ctx context.Context, limit int) ([]ChatSession, error)
	RenameChatSession(ctx context.Context, id, title string) error
	DeleteChatSession(ctx context.Context, id string) error
	AppendChatMessage(ctx context.Context, m ChatMessage) (int64, error)
	ListChatMessages(ctx context.Context, sessionID string) ([]ChatMessage, error)

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

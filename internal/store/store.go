package store

import (
	"context"
	"errors"
	"time"
)

var ErrNotFound = errors.New("record not found")

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
}

type Space struct {
	ID        string    `json:"id"`
	WorkerID  string    `json:"worker_id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Branch    string    `json:"branch,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Task struct {
	ID             string         `json:"id"`
	WorkerID       string         `json:"worker_id"`
	SpaceID        string         `json:"space_id,omitempty"`
	Agent          string         `json:"agent"`
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

type UsageSummary struct {
	TotalInputTokens  int64         `json:"total_input_tokens"`
	TotalOutputTokens int64         `json:"total_output_tokens"`
	TotalRequests     int64         `json:"total_requests"`
	ByProvider        []UsageBucket `json:"by_provider"`
}

type Store interface {
	UpsertWorker(ctx context.Context, w Worker) error
	GetWorker(ctx context.Context, id string) (Worker, error)
	ListWorkers(ctx context.Context) ([]Worker, error)
	DeleteWorker(ctx context.Context, id string) error
	MarkStaleWorkersOffline(ctx context.Context, cutoff time.Time) ([]string, error)

	CreateSpace(ctx context.Context, sp Space) error
	GetSpace(ctx context.Context, id string) (Space, error)
	ListSpaces(ctx context.Context, workerID string) ([]Space, error)
	DeleteSpace(ctx context.Context, id string) error

	CreateTask(ctx context.Context, t Task) error
	UpdateTaskAssignment(ctx context.Context, id, workerID, effectiveDir string) error
	UpdateTaskStatus(ctx context.Context, id, status, summary, gitSummaryJSON string, exitCode *int) error
	GetTask(ctx context.Context, id string) (Task, error)
	ListTasks(ctx context.Context, status, workerID string, limit int) ([]Task, error)

	AppendEvent(ctx context.Context, e TaskEvent) (int64, error)
	ListEvents(ctx context.Context, taskID string, afterID int64, limit int) ([]TaskEvent, error)

	CreateChatSession(ctx context.Context, s ChatSession) error
	GetChatSession(ctx context.Context, id string) (ChatSession, error)
	ListChatSessions(ctx context.Context, limit int) ([]ChatSession, error)
	AppendChatMessage(ctx context.Context, m ChatMessage) (int64, error)
	ListChatMessages(ctx context.Context, sessionID string) ([]ChatMessage, error)

	RecordUsage(ctx context.Context, u UsageEvent) error
	GetUsageSummary(ctx context.Context) (UsageSummary, error)

	GetAllSettings(ctx context.Context) (map[string]string, error)
	SetSettings(ctx context.Context, kv map[string]string) error

	Close() error
}

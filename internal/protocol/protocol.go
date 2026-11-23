package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// WebSocket JSON control plane message types.
const (
	MsgWorkerHello     = "worker.hello"
	MsgWorkerHeartbeat = "worker.heartbeat"
	MsgWorkerFacts     = "worker.facts"
	MsgWorkerQuotas    = "worker.quotas"
	MsgExternalSession = "worker.external_session"

	MsgTaskStart  = "task.start"
	MsgTaskCancel = "task.cancel"
	MsgTaskSteer  = "task.steer"
	MsgTaskStatus = "task.status"
	MsgTaskEvent  = "task.event"

	MsgPtySubscribe   = "pty.subscribe"
	MsgPtyUnsubscribe = "pty.unsubscribe"
	MsgPtyResize      = "pty.resize"

	MsgChatSend  = "chat.send"
	MsgChatDelta = "chat.delta"
	MsgChatTool  = "chat.tool"
	MsgChatDone  = "chat.done"
	MsgChatError = "chat.error"

	MsgFleetUpdate = "fleet.update"
	MsgUsageUpdate = "usage.update"

	MsgGitInspect               = "git.inspect"
	MsgGitInspectResult         = "git.inspect.result"
	MsgGitCommit                = "git.commit"
	MsgGitCommitResult          = "git.commit.result"
	MsgGitWorktreeCleanup       = "git.worktree.cleanup"
	MsgGitWorktreeCleanupResult = "git.worktree.cleanup.result"

	MsgFSList             = "fs.list"
	MsgFSListResult       = "fs.list.result"
	MsgWorktreeScan       = "git.worktree.scan"
	MsgWorktreeScanResult = "git.worktree.scan.result"
	MsgFileUpload         = "file.upload"
	MsgFileUploadResult   = "file.upload.result"

	// WSCloseWorkerRemoved is sent by the controller when an operator removes a machine
	// from the fleet so konsertd exits cleanly instead of auto-reconnecting.
	WSCloseWorkerRemoved = 4001

	// WSCloseTryAgainLater (RFC 6455 1013) is sent by the controller to a new worker
	// connection while another live connection holds the same identity. konsertd
	// reconnects with backoff; only WSCloseWorkerRemoved makes it exit.
	WSCloseTryAgainLater = 1013
)

// Task origins distinguish Direct Mode sessions, Orchestrated jobs, and Floating Scratch sessions.
const (
	TaskOriginOrchestrator = "orchestrator"
	TaskOriginDirect       = "direct"
	TaskOriginScratch      = "scratch"
	TaskOriginExternal     = "external"
)

// Task statuses.
const (
	TaskStatusQueued       = "queued"
	TaskStatusAssigned     = "assigned"
	TaskStatusRunning      = "running"
	TaskStatusIdle         = "idle"
	TaskStatusWaitingInput = "waiting_input"
	TaskStatusCompleted    = "completed"
	TaskStatusFailed       = "failed"
	TaskStatusCanceled     = "canceled"
)

// Task execution modes.
const (
	TaskModeOneshot     = "oneshot"
	TaskModeInteractive = "interactive"
)

// Permission modes.
const (
	PermissionFullAccess = "full_access"
	PermissionSandboxed  = "sandboxed"
)

// Orphan policies.
const (
	OrphanPolicyFail    = "fail"
	OrphanPolicyRequeue = "requeue"
)

// Envelope is the JSON text frame wrapper used on both /ws/worker and /ws/client.
type Envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	TS      time.Time       `json:"ts"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// NewEnvelope marshals a typed control-plane message into JSON bytes.
func NewEnvelope(msgType, id string, payload any) ([]byte, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal payload for %s: %w", msgType, err)
		}
		raw = b
	}
	env := Envelope{
		Type:    msgType,
		ID:      id,
		TS:      time.Now().UTC(),
		Payload: raw,
	}
	return json.Marshal(env)
}

// DecodeEnvelope unmarshals a JSON text frame into an Envelope.
func DecodeEnvelope(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	if env.Type == "" {
		return Envelope{}, fmt.Errorf("envelope missing type")
	}
	return env, nil
}

// DecodePayload unmarshals an envelope's payload into T.
func DecodePayload[T any](env Envelope) (T, error) {
	var out T
	if len(env.Payload) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(env.Payload, &out); err != nil {
		return out, fmt.Errorf("decode payload for %s: %w", env.Type, err)
	}
	return out, nil
}

// MachineFacts captures cached OS, hardware, virtualization, filesystem, and safe tool/auth readiness facts.
type MachineFacts struct {
	OSName         string            `json:"os_name,omitempty"`
	OSVersion      string            `json:"os_version,omitempty"`
	KernelVersion  string            `json:"kernel_version,omitempty"`
	CPUModel       string            `json:"cpu_model,omitempty"`
	CPUCores       int               `json:"cpu_cores"`
	RAMTotalMB     uint64            `json:"ram_total_mb"`
	SwapTotalMB    uint64            `json:"swap_total_mb"`
	SwapUsedMB     uint64            `json:"swap_used_mb"`
	StorageType    string            `json:"storage_type,omitempty"` // "nvme" | "ssd" | "hdd" | "virtual" | "unknown"
	DiskTotalGB    uint64            `json:"disk_total_gb"`
	DiskFreeGB     uint64            `json:"disk_free_gb"`
	TmpIsTmpfs     bool              `json:"tmp_is_tmpfs"`
	IsWSL          bool              `json:"is_wsl"`
	IsContainer    bool              `json:"is_container"`
	IsMacContainer bool              `json:"is_mac_container"`
	IsVM           bool              `json:"is_vm"`
	Virtualization string            `json:"virtualization,omitempty"` // "bare_metal" | "wsl" | "container" | "mac_container" | "vm" | "unknown"
	Tools          map[string]string `json:"tools,omitempty"`
	AuthReady      map[string]bool   `json:"auth_ready,omitempty"`
	CollectedAt    time.Time         `json:"collected_at"`
}

type WorkerFactsPayload struct {
	Facts MachineFacts `json:"facts"`
}

// WorkerInfo describes a worker node during registration.
type WorkerInfo struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	HostGroup      string            `json:"host_group"`
	OS             string            `json:"os"`
	Arch           string            `json:"arch"`
	Isolation      string            `json:"isolation"`
	PermissionMode string            `json:"permission_mode"`
	Agents         []string          `json:"agents"`
	Tags           map[string]string `json:"tags"`
	MaxConcurrent  int               `json:"max_concurrent"`
	Facts          *MachineFacts     `json:"facts,omitempty"`
	// MachineID is a stable per-installation UUID persisted by konsertd; the hub
	// prefers it over the name/host tuple for identity matching.
	MachineID string `json:"machine_id,omitempty"`
}

// ActiveTaskInfo is sent in worker.hello so the controller can re-adopt in-flight tasks on restart.
type ActiveTaskInfo struct {
	TaskID       string    `json:"task_id"`
	Status       string    `json:"status"`
	EffectiveDir string    `json:"effective_dir,omitempty"`
	StartedAt    time.Time `json:"started_at"`
}

type WorkerHelloPayload struct {
	Worker      WorkerInfo       `json:"worker"`
	ActiveTasks []ActiveTaskInfo `json:"active_tasks,omitempty"`
}

type ListeningPort struct {
	Port    int    `json:"port"`
	Process string `json:"process,omitempty"`
	Address string `json:"address,omitempty"`
}

type WorkerMetrics struct {
	CPUPercent     float64         `json:"cpu_percent"`
	MemUsedMB      uint64          `json:"mem_used_mb"`
	MemTotalMB     uint64          `json:"mem_total_mb"`
	SwapUsedMB     uint64          `json:"swap_used_mb,omitempty"`
	SwapTotalMB    uint64          `json:"swap_total_mb,omitempty"`
	DiskFreeGB     uint64          `json:"disk_free_gb,omitempty"`
	DiskTotalGB    uint64          `json:"disk_total_gb,omitempty"`
	LoadAvg        float64         `json:"load_avg"`
	ListeningPorts []ListeningPort `json:"listening_ports,omitempty"`
}

type WorkerHeartbeatPayload struct {
	ActiveTaskIDs []string       `json:"active_task_ids"`
	LoadAvg       float64        `json:"load_avg,omitempty"`
	Metrics       *WorkerMetrics `json:"metrics,omitempty"`
}

type SubscriptionQuota struct {
	Provider    string     `json:"provider"`               // "claude" | "agy" | "codex" | "grok" | "cursor"
	Label       string     `json:"label"`                  // "Claude", "Antigravity", "Codex", "Grok", "Cursor Models"
	WindowLabel string     `json:"window_label,omitempty"` // "5h", "8h", "7d", ""
	UsedPercent float64    `json:"used_percent"`           // 0..100
	ResetAt     *time.Time `json:"reset_at,omitempty"`
	Source      string     `json:"source,omitempty"` // "probe" | "hook" | "manual"
	WorkerID    string     `json:"worker_id,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

type WorkerQuotasPayload struct {
	Quotas []SubscriptionQuota `json:"quotas"`
}

// ExternalSessionPayload reports a coding agent session launched outside Konsert
// (discovered via agent hooks or lightweight passive process/transcript inspection).
type ExternalSessionPayload struct {
	ExternalID   string   `json:"external_id"` // stable per-host session key (e.g. "pid-1234" or hook session id)
	Agent        string   `json:"agent"`       // "claude" | "agy" | "codex" | "grok"
	Model        string   `json:"model,omitempty"`
	Cwd          string   `json:"cwd"`
	Branch       string   `json:"branch,omitempty"`
	Status       string   `json:"status"` // "working" | "needs_you" | "done" | "fail"
	Summary      string   `json:"summary,omitempty"`
	ChangedFiles []string `json:"changed_files,omitempty"`
	Source       string   `json:"source"` // "hook" | "passive"
}

type UsageBucket struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Requests     int64  `json:"requests"`
}

type UsageSummary struct {
	TotalInputTokens  int64               `json:"total_input_tokens"`
	TotalOutputTokens int64               `json:"total_output_tokens"`
	TotalRequests     int64               `json:"total_requests"`
	ByProvider        []UsageBucket       `json:"by_provider"`
	Quotas            []SubscriptionQuota `json:"quotas,omitempty"`
}

type TaskStartPayload struct {
	TaskID         string `json:"task_id"`
	Agent          string `json:"agent"`
	Model          string `json:"model,omitempty"`
	SessionKind    string `json:"session_kind,omitempty"`
	Workdir        string `json:"workdir"`
	Branch         string `json:"branch,omitempty"`
	UseWorktree    bool   `json:"use_worktree"`
	Prompt         string `json:"prompt"`
	Mode           string `json:"mode"`
	PermissionMode string `json:"permission_mode"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Cols           uint16 `json:"cols,omitempty"`
	Rows           uint16 `json:"rows,omitempty"`
}

type TaskCancelPayload struct {
	TaskID string `json:"task_id"`
}

type TaskSteerPayload struct {
	TaskID        string `json:"task_id"`
	InputText     string `json:"input_text"`
	AppendNewline bool   `json:"append_newline"`
}

type GitSummary struct {
	BaseSHA    string   `json:"base_sha,omitempty"`
	HeadSHA    string   `json:"head_sha,omitempty"`
	Commits    []string `json:"commits,omitempty"`
	DiffStat   string   `json:"diff_stat,omitempty"`
	DirtyFiles []string `json:"dirty_files,omitempty"`
}

type TaskStatusPayload struct {
	TaskID       string      `json:"task_id"`
	Status       string      `json:"status"`
	EffectiveDir string      `json:"effective_dir,omitempty"`
	ExitCode     *int        `json:"exit_code,omitempty"`
	Summary      string      `json:"summary,omitempty"`
	GitSummary   *GitSummary `json:"git_summary,omitempty"`
}

type TaskEventPayload struct {
	TaskID  string         `json:"task_id"`
	Kind    string         `json:"kind"`
	Message string         `json:"message"`
	Meta    map[string]any `json:"meta,omitempty"`
}

type PtySubscribePayload struct {
	TaskID string `json:"task_id"`
}

type PtyUnsubscribePayload struct {
	TaskID string `json:"task_id"`
}

type PtyResizePayload struct {
	TaskID string `json:"task_id"`
	Cols   uint16 `json:"cols"`
	Rows   uint16 `json:"rows"`
}

type ChatSendPayload struct {
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
}

type ChatDeltaPayload struct {
	SessionID string `json:"session_id"`
	Delta     string `json:"delta"`
}

type ChatToolPayload struct {
	SessionID string `json:"session_id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Args      any    `json:"args,omitempty"`
	Result    any    `json:"result,omitempty"`
	Status    string `json:"status"` // "running" | "completed" | "error"
}

type ChatDonePayload struct {
	SessionID string `json:"session_id"`
	MessageID int64  `json:"message_id,omitempty"`
}

type ChatErrorPayload struct {
	SessionID string `json:"session_id"`
	Error     string `json:"error"`
}

type FleetUpdatePayload struct {
	Kind      string `json:"kind"` // "worker" | "task" | "event" | "space" | "workspace" | "project" | "repo" | "worktree" | "*_deleted"
	Worker    any    `json:"worker,omitempty"`
	Task      any    `json:"task,omitempty"`
	Event     any    `json:"event,omitempty"`
	Space     any    `json:"space,omitempty"`
	Workspace any    `json:"workspace,omitempty"`
	Project   any    `json:"project,omitempty"`
	Repo      any    `json:"repo,omitempty"`
	Worktree  any    `json:"worktree,omitempty"`
	DeletedID string `json:"deleted_id,omitempty"`
}

type GitChangedFile struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // "M", "A", "D", "??", etc.
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Diff    string `json:"diff,omitempty"`
}

type GitInspectRequest struct {
	Workdir string `json:"workdir"`
}

type GitInspectResult struct {
	IsGitRepo    bool             `json:"is_git_repo"`
	Branch       string           `json:"branch,omitempty"`
	EffectiveDir string           `json:"effective_dir,omitempty"`
	IsWorktree   bool             `json:"is_worktree"`
	Added        int              `json:"added"`
	Deleted      int              `json:"deleted"`
	Files        []GitChangedFile `json:"files"`
	Commits      []string         `json:"commits,omitempty"`
	Error        string           `json:"error,omitempty"`
}

type GitCommitRequest struct {
	Workdir  string `json:"workdir"`
	Message  string `json:"message"`
	StageAll bool   `json:"stage_all"`
}

type GitCommitResult struct {
	OK        bool   `json:"ok"`
	CommitSHA string `json:"commit_sha,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Error     string `json:"error,omitempty"`
}

type GitWorktreeCleanupRequest struct {
	Workdir string `json:"workdir"`
}

type GitWorktreeCleanupResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type FSEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	IsDir     bool   `json:"is_dir"`
	IsGitRepo bool   `json:"is_git_repo,omitempty"`
}

type FSListRequest struct {
	Path     string `json:"path"`
	Prefix   string `json:"prefix,omitempty"`
	DirsOnly bool   `json:"dirs_only"`
}

type FSListResult struct {
	CurrentDir string    `json:"current_dir"`
	ParentDir  string    `json:"parent_dir,omitempty"`
	Entries    []FSEntry `json:"entries"`
	Error      string    `json:"error,omitempty"`
}

type WorktreeScanRequest struct {
	RootPath string `json:"root_path,omitempty"`
	Query    string `json:"query,omitempty"` // optional substring filter (e.g. "epic-123" or "123")
}

type WorktreeScanMatch struct {
	Path      string `json:"path"`
	RepoRoot  string `json:"repo_root,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Kind      string `json:"kind"` // "konsert_worktree" | "git_worktree" | "temp_dir"
	Removable bool   `json:"removable"`
}

type WorktreeScanResult struct {
	RootPath string              `json:"root_path"`
	Matches  []WorktreeScanMatch `json:"matches"`
	Error    string              `json:"error,omitempty"`
}

type FileUploadRequest struct {
	SessionID  string `json:"session_id,omitempty"`
	Filename   string `json:"filename"`
	DataBase64 string `json:"data_base64"`
}

type FileUploadResult struct {
	OK         bool   `json:"ok"`
	RemotePath string `json:"remote_path,omitempty"`
	Filename   string `json:"filename,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Error      string `json:"error,omitempty"`
}

// IsTerminalTaskStatus reports whether a task status is final.
func IsTerminalTaskStatus(status string) bool {
	switch status {
	case TaskStatusCompleted, TaskStatusFailed, TaskStatusCanceled:
		return true
	default:
		return false
	}
}

// Host wake lock (Keep Awake) control plane. Both messages are additive: hubs that never
// send host.wakelock and workers that never report host.wakelock.state are unaffected,
// and older peers ignore message types they do not know.
const (
	// MsgHostWakeLock (hub → worker) asks konsertd to hold or release an OS-level
	// idle-sleep inhibitor. konsertd answers with MsgHostWakeLockState.
	MsgHostWakeLock = "host.wakelock"
	// MsgHostWakeLockState (worker → hub) reports the current wake lock state. konsertd
	// sends it right after every worker.hello (so the hub learns the state after a
	// reconnect) and whenever the state changes.
	MsgHostWakeLockState = "host.wakelock.state"
)

// HostWakeLockPayload is the hub → worker request carried by MsgHostWakeLock.
type HostWakeLockPayload struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
}

// HostWakeLockState is the worker → hub report carried by MsgHostWakeLockState.
type HostWakeLockState struct {
	// Active reports whether an inhibitor is currently held.
	Active bool `json:"active"`
	// Method names the mechanism: "caffeinate", "systemd-inhibit" or
	// "SetThreadExecutionState" (empty when the platform has none).
	Method string `json:"method,omitempty"`
	// Error explains why the last enable request failed or why a held lock was lost.
	Error string `json:"error,omitempty"`
	// Reason echoes the reason the current lock was taken with.
	Reason string `json:"reason,omitempty"`
	// Since is when the current lock was acquired (nil while inactive).
	Since *time.Time `json:"since,omitempty"`
}

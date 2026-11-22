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
)

// Task origins distinguish Direct Mode sessions, Orchestrated jobs, and Floating Scratch sessions.
const (
	TaskOriginOrchestrator = "orchestrator"
	TaskOriginDirect       = "direct"
	TaskOriginScratch      = "scratch"
)

// Task statuses.
const (
	TaskStatusQueued       = "queued"
	TaskStatusAssigned     = "assigned"
	TaskStatusRunning      = "running"
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
	LoadAvg        float64         `json:"load_avg"`
	ListeningPorts []ListeningPort `json:"listening_ports,omitempty"`
}

type WorkerHeartbeatPayload struct {
	ActiveTaskIDs []string       `json:"active_task_ids"`
	LoadAvg       float64        `json:"load_avg,omitempty"`
	Metrics       *WorkerMetrics `json:"metrics,omitempty"`
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

type TaskStartPayload struct {
	TaskID         string `json:"task_id"`
	Agent          string `json:"agent"`
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
	Kind      string `json:"kind"` // "worker" | "task" | "event" | "space" | "space_deleted"
	Worker    any    `json:"worker,omitempty"`
	Task      any    `json:"task,omitempty"`
	Event     any    `json:"event,omitempty"`
	Space     any    `json:"space,omitempty"`
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

// IsTerminalTaskStatus reports whether a task status is final.
func IsTerminalTaskStatus(status string) bool {
	switch status {
	case TaskStatusCompleted, TaskStatusFailed, TaskStatusCanceled:
		return true
	default:
		return false
	}
}

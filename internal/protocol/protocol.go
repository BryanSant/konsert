package protocol

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
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
	// MsgWorkerPorts carries the listening ports (WorkerPortsPayload): sent every few
	// minutes and as the reply (same envelope id) to MsgWorkerPortsRequest.
	MsgWorkerPorts        = "worker.ports"
	MsgWorkerPortsRequest = "worker.ports.request"

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

	// WSCloseCredentialRevoked is sent by the controller when an operator revokes the
	// machine's credential. konsertd keeps retrying slowly (re-reading its saved
	// credential), so `konsertd join` with a new code brings it back.
	WSCloseCredentialRevoked = 4003

	// WSCloseTryAgainLater (RFC 6455 1013) is sent by the controller to a new worker
	// connection while another live connection holds the same identity. konsertd
	// reconnects with backoff; only WSCloseWorkerRemoved makes it exit.
	WSCloseTryAgainLater = 1013
)

// The local-socket transport (KON-K1B60): the hub (or `konsertd attach` on its behalf,
// over SSH) opens a WebSocket on konsertd's local socket at AttachPath and speaks the
// same worker protocol as on /ws/worker, with the roles of who dials reversed.
const (
	// AttachPath is the HTTP path of the WebSocket upgrade on the local socket.
	AttachPath = "/konsertd/attach"
	// HubIDHeader carries the attaching hub's id, so konsertd knows which
	// authorized_keys line to drop when that hub removes the machine.
	HubIDHeader = "X-Konsert-Hub"
	// TransportHeader names the transport the hub attaches over ("ssh" or "local"), for
	// `konsertd status`.
	TransportHeader = "X-Konsert-Transport"
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
	// Seq is the daemon's spool sequence number of a spooled worker → hub message (0:
	// not spooled). See spool.go.
	Seq     uint64         `json:"seq,omitzero"`
	Type    string         `json:"type"`
	ID      string         `json:"id,omitempty"`
	TS      time.Time      `json:"ts"`
	Payload jsontext.Value `json:"payload,omitzero"`
}

// JSONOptions are the options for JSON Konsert exchanges over its wire
// protocols and REST API. Strings that come from the outside world (file
// names, command and agent output) are not guaranteed to be valid UTF-8; v2
// rejects invalid UTF-8 by default, which would drop a whole frame or
// response, so invalid bytes are replaced with U+FFFD (as v1 did).
var JSONOptions = jsontext.AllowInvalidUTF8(true)

// outEnvelope is Envelope with the payload marshaled in place (one encoding
// pass instead of marshaling the payload and then re-validating it).
type outEnvelope struct {
	Type    string    `json:"type"`
	ID      string    `json:"id,omitempty"`
	TS      time.Time `json:"ts"`
	Payload any       `json:"payload,omitzero"`
}

// NewEnvelope marshals a typed control-plane message into JSON bytes. A nil
// payload is omitted.
func NewEnvelope(msgType, id string, payload any) ([]byte, error) {
	b, err := json.Marshal(outEnvelope{Type: msgType, ID: id, TS: time.Now().UTC(), Payload: payload}, JSONOptions)
	if err != nil {
		return nil, fmt.Errorf("marshal payload for %s: %w", msgType, err)
	}
	return b, nil
}

// DecodeEnvelope unmarshals a JSON text frame into an Envelope.
func DecodeEnvelope(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env, JSONOptions); err != nil {
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
	if err := json.Unmarshal(env.Payload, &out, JSONOptions); err != nil {
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
	// StartMode (Windows) is how konsertd was started: "service", "background_task" (an
	// S4U task: runs whether or not anyone is signed in) or "user_session" (a logon task:
	// only runs while the user is signed in); "" elsewhere or when unknown.
	StartMode   string    `json:"start_mode,omitempty"`
	CollectedAt time.Time `json:"collected_at"`
}

type WorkerFactsPayload struct {
	Facts MachineFacts `json:"facts"`
}

// WorkerInfo describes a worker node during registration.
type WorkerInfo struct {
	// ID is konsertd's persistent UUIDv7 identity: the hub's workers.id.
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
	Facts          *MachineFacts     `json:"facts,omitzero"`
	// Version is konsertd's build version (ldflags main.version). Empty or "dev"
	// means unknown and never counts as a mismatch with the hub.
	Version string `json:"version,omitempty"`
	// Hostname is the OS hostname of the daemon's machine (its Name may differ). The hub
	// uses it, with a loopback peer address, to recognize a daemon on its own host.
	Hostname string `json:"hostname,omitempty"`
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
	// ActiveChats are the agent chats open on the daemon.
	ActiveChats []ActiveChatInfo `json:"active_chats,omitempty"`
	// Spool describes the daemon's durable outbound spool; the hub answers with
	// worker.spool.resume (nil: the daemon does not spool).
	Spool *SpoolHello `json:"spool,omitzero"`
}

type ListeningPort struct {
	Port    int    `json:"port"`
	Process string `json:"process,omitempty"`
	Address string `json:"address,omitempty"`
}

// WorkerMetrics are the vitals in every heartbeat, read without a subprocess (see
// internal/worker/metrics.go). Disk free is a machine fact; listening ports come in
// worker.ports.
type WorkerMetrics struct {
	// CPUPercent is the busy share since the previous sample (Linux, Windows) or the
	// load average over the core count (macOS).
	CPUPercent  float64 `json:"cpu_percent"`
	MemUsedMB   uint64  `json:"mem_used_mb"`
	MemTotalMB  uint64  `json:"mem_total_mb"`
	SwapUsedMB  uint64  `json:"swap_used_mb,omitzero"`
	SwapTotalMB uint64  `json:"swap_total_mb,omitzero"`
	// LoadAvg is the 1-minute load average (0 on Windows, which has none).
	LoadAvg float64 `json:"load_avg"`
	// GPUPercent is GPU utilization 0-100 when the platform reports it (macOS
	// IOAccelerator statistics, nvidia-smi on Linux); nil when unknown. Read on every
	// heartbeat while sessions run, otherwise every 5 minutes.
	GPUPercent *float64 `json:"gpu_percent,omitzero"`
}

// WorkerPortsPayload is a machine's listening TCP ports (worker.ports).
type WorkerPortsPayload struct {
	Ports       []ListeningPort `json:"ports"`
	CollectedAt time.Time       `json:"collected_at"`
}

type WorkerHeartbeatPayload struct {
	ActiveTaskIDs []string       `json:"active_task_ids"`
	LoadAvg       float64        `json:"load_avg,omitzero"`
	Metrics       *WorkerMetrics `json:"metrics,omitzero"`
	// At is when the vitals were sampled: a heartbeat replayed from the spool after an
	// outage is stored at its original time.
	At time.Time `json:"at,omitzero"`
	// Samples is how many heartbeats Metrics averages (0 or 1: one). The spool compacts
	// old vitals into 1-minute and 1-hour means when it runs out of room.
	Samples int `json:"samples,omitzero"`
}

type SubscriptionQuota struct {
	Provider    string     `json:"provider"`               // "claude" | "agy" | "codex" | "grok" | "cursor"
	Label       string     `json:"label"`                  // "Claude", "Antigravity", "Codex", "Grok", "Cursor Models"
	WindowLabel string     `json:"window_label,omitempty"` // "5h", "8h", "7d", ""
	UsedPercent float64    `json:"used_percent"`           // 0..100
	ResetAt     *time.Time `json:"reset_at,omitzero"`
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
	// Tool is the tool the agent is running (a PreToolUse hook), "" when unknown or
	// finished (PostToolUse); with Status "working" it gives the coarse activity.
	Tool string `json:"tool,omitempty"`
	// Event is the raw hook event (PreToolUse, PostToolUse, Notification, Stop, ...).
	Event string `json:"event,omitempty"`
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
	TaskID string `json:"task_id"`
	// SID is the task's short user-facing ID (t-…); the worker names the task's git
	// worktree directory after it (.konsert-worktrees/<sid>). The protocol keys on TaskID.
	SID            string `json:"sid,omitempty"`
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
	Cols           uint16 `json:"cols,omitzero"`
	Rows           uint16 `json:"rows,omitzero"`
	// AccountID selects a worker-local agent account (AgentAccount.ID); "" = the
	// daemon's own environment (the agent's default login).
	AccountID string `json:"account_id,omitempty"`
	// ApprovalMode is an AgentApprovalMode.ID of the agent; when set it replaces the
	// PermissionMode-derived approval flags (PermissionMode still controls sandboxing).
	ApprovalMode string `json:"approval_mode,omitempty"`
	// Effort is an AgentEffort.ID of the model.
	Effort string `json:"effort,omitempty"`
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
	ExitCode     *int        `json:"exit_code,omitzero"`
	Summary      string      `json:"summary,omitempty"`
	GitSummary   *GitSummary `json:"git_summary,omitzero"`
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
	Args      any    `json:"args,omitzero"`
	Result    any    `json:"result,omitzero"`
	Status    string `json:"status"` // "running" | "completed" | "error"
}

type ChatDonePayload struct {
	SessionID string `json:"session_id"`
	MessageID int64  `json:"message_id,omitzero"`
}

type ChatErrorPayload struct {
	SessionID string `json:"session_id"`
	Error     string `json:"error"`
}

type FleetUpdatePayload struct {
	Kind      string `json:"kind"` // "worker" | "task" | "event" | "space" | "workspace" | "project" | "repo" | "worktree" | "*_deleted"
	Worker    any    `json:"worker,omitzero"`
	Task      any    `json:"task,omitzero"`
	Event     any    `json:"event,omitzero"`
	Space     any    `json:"space,omitzero"`
	Workspace any    `json:"workspace,omitzero"`
	Project   any    `json:"project,omitzero"`
	Repo      any    `json:"repo,omitzero"`
	Worktree  any    `json:"worktree,omitzero"`
	// Audit carries a new fleet audit log entry when Kind is "audit" (additive).
	Audit any `json:"audit,omitzero"`
	// Accounts carries a worker's agent accounts when Kind is "agent_accounts" (additive).
	Accounts any `json:"accounts,omitzero"`
	// ChatSession carries a created/updated chat (status, title, settings) when Kind is
	// "chat_session"; "chat_session_deleted" uses DeletedID (additive).
	ChatSession any    `json:"chat_session,omitzero"`
	DeletedID   string `json:"deleted_id,omitempty"`
	// WorkerID names the worker for worker-scoped kinds such as "agent_accounts" (additive).
	WorkerID string `json:"worker_id,omitempty"`
	// Human control kinds: "fleet_event" (FleetEvent), "hold" (Hold), "request"
	// (Request) and "pause" (Pause, the fleet-wide Pause everything state).
	FleetEvent any `json:"fleet_event,omitzero"`
	Hold       any `json:"hold,omitzero"`
	Request    any `json:"request,omitzero"`
	Pause      any `json:"pause,omitzero"`
	// Override carries an override request (break-glass) when Kind is "override"; Veto a
	// veto with its acknowledgement state when Kind is "veto".
	Override any `json:"override,omitzero"`
	Veto     any `json:"veto,omitzero"`
	// Question carries a question (with its side conversation) when Kind is "question".
	Question any `json:"question,omitzero"`
	// Activity carries a SessionActivity when Kind is "activity".
	Activity any `json:"activity,omitzero"`
	// Review carries a store.Review (decision, Expedite) when Kind is "review".
	Review any `json:"review,omitzero"`
	// Device carries a signed-in device (Settings → Devices) when Kind is "device";
	// "device_revoked" uses DeletedID.
	Device any `json:"device,omitzero"`
	// ShellReminder carries a "shells left running" Needs-you item when Kind is
	// "shell_reminder" (resolved: true when it is gone).
	ShellReminder any `json:"shell_reminder,omitzero"`
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

// MsgGitPRReview (hub->worker) asks konsertd to review the pull request of a checkout's
// branch with gh on the machine that owns the checkout; the reply is MsgGitPRReviewResult.
const (
	MsgGitPRReview       = "git.pr.review"
	MsgGitPRReviewResult = "git.pr.review.result"
)

// PR review actions.
const (
	PRReviewApprove        = "approve"
	PRReviewRequestChanges = "request_changes"
)

// PR review outcomes (GitPRReviewResult.Status).
const (
	PRReviewSent = "sent" // gh submitted the review
	// PRReviewCommented: the PR is the user's own, which GitHub does not let them
	// approve or request changes on, so the decision was posted as a comment.
	PRReviewCommented = "commented"
	PRReviewFailed    = "failed"    // gh (or GitHub) refused; Message says why
	PRReviewNoGitHub  = "no_github" // the checkout has no GitHub remote
	PRReviewNoGH      = "no_gh"     // gh is not installed on the machine
	PRReviewNoPR      = "no_pr"     // no pull request for the branch
	PRReviewOffline   = "offline"   // set by the hub: the machine is not connected
)

type GitPRReviewRequest struct {
	Workdir string `json:"workdir"`
	// Branch whose PR is reviewed ("" = the checkout's current branch).
	Branch string `json:"branch,omitempty"`
	Action string `json:"action"` // approve | request_changes
	Body   string `json:"body,omitempty"`
	// By names who decided (device or user label), for the comment posted instead of a
	// review on the user's own PR.
	By string `json:"by,omitempty"`
}

type GitPRReviewResult struct {
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	PRNumber int    `json:"pr_number,omitzero"`
	PRURL    string `json:"pr_url,omitempty"`
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
	IsGitRepo bool   `json:"is_git_repo,omitzero"`
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
	Size       int64  `json:"size,omitzero"`
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
	Since *time.Time `json:"since,omitzero"`
}

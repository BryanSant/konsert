package protocol

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"time"
)

// WebSocket JSON control plane message types.
const (
	MsgWorkerHello = "worker.hello"
	// MsgWorkerDirectives (hub → worker) replaces syred's active policy directives
	// for this machine. Sent on hello and whenever that machine's directives change.
	MsgWorkerDirectives = "worker.directives"
	MsgWorkerHeartbeat  = "worker.heartbeat"
	MsgWorkerFacts      = "worker.facts"
	MsgWorkerQuotas     = "worker.quotas"
	MsgExternalSession  = "worker.external_session"
	// MsgWorkerPorts carries the listening ports (WorkerPortsPayload): sent every few
	// minutes and as the reply (same envelope id) to MsgWorkerPortsRequest.
	MsgWorkerPorts = "worker.ports"
	// Process-level attribution (SYR-AT7T1): a watched daemon process exited (with
	// who could have stopped it), and a user's request to kill one process.
	MsgProcessExit        = "worker.process_exit"
	MsgProcessKill        = "worker.process_kill"
	MsgProcessKillResult  = "worker.process_kill_result"
	MsgWorkerPortsRequest = "worker.ports.request"
	// MsgWorkerCheckoutsRefresh asks syred to re-scan local git checkouts and report
	// updated machine facts immediately (on-demand refresh).
	MsgWorkerCheckoutsRefresh = "worker.checkouts.refresh"

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

	// MsgArtifactFetch asks the daemon that ran a task for one capped artifact
	// (diff, log, stderr, file). The hub serves digest and transcript itself.
	MsgArtifactFetch       = "artifact.fetch"
	MsgArtifactFetchResult = "artifact.fetch.result"

	MsgFSList             = "fs.list"
	MsgFSListResult       = "fs.list.result"
	MsgWorktreeScan       = "git.worktree.scan"
	MsgWorktreeScanResult = "git.worktree.scan.result"
	MsgFileUpload         = "file.upload"
	MsgFileUploadResult   = "file.upload.result"

	// WSCloseWorkerRemoved is sent by the controller when an operator removes a machine
	// from the fleet so syred exits cleanly instead of auto-reconnecting.
	WSCloseWorkerRemoved = 4001

	// WSCloseCredentialRevoked is sent by the controller when an operator revokes the
	// machine's credential. syred keeps retrying slowly (re-reading its saved
	// credential), so `syred join` with a new code brings it back.
	WSCloseCredentialRevoked = 4003

	// WSCloseTryAgainLater (RFC 6455 1013) is sent by the controller to a new worker
	// connection while another live connection holds the same identity. syred
	// reconnects with backoff; only WSCloseWorkerRemoved makes it exit.
	WSCloseTryAgainLater = 1013
)

// The local-socket transport (SYR-K1B60): the hub (or `syred attach` on its behalf,
// over SSH) opens a WebSocket on syred's local socket at AttachPath and speaks the
// same worker protocol as on /ws/worker, with the roles of who dials reversed.
const (
	// AttachPath is the HTTP path of the WebSocket upgrade on the local socket.
	AttachPath = "/syred/attach"
	// HubIDHeader carries the attaching hub's id, so syred knows which
	// authorized_keys line to drop when that hub removes the machine.
	HubIDHeader = "X-Syre-Hub"
	// TransportHeader names the transport the hub attaches over ("ssh" or "local"), for
	// `syred status`.
	TransportHeader = "X-Syre-Transport"
)

// Task origins distinguish Direct Mode sessions, Commander-dispatched jobs, and Floating Scratch sessions.
const (
	TaskOriginCommander = "commander"
	TaskOriginDirect    = "direct"
	TaskOriginScratch   = "scratch"
	TaskOriginExternal  = "external"
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

// JSONOptions are the options for JSON Syre exchanges over its wire
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

// TerminalShell describes one terminal shell available on a worker machine.
type TerminalShell struct {
	ID    string `json:"id"`             // "git-bash" | "pwsh" | "powershell" | "zsh" | "bash" | "fish"
	Label string `json:"label"`          // "Git Bash" | "PowerShell 7" | "PowerShell 5.1" | "zsh" | "bash" | "fish"
	Path  string `json:"path,omitempty"` // resolved executable path on the worker
}

// Known terminal shell IDs.
const (
	ShellGitBash    = "git-bash"
	ShellPwsh       = "pwsh"
	ShellPowerShell = "powershell"
	ShellZsh        = "zsh"
	ShellBash       = "bash"
	ShellFish       = "fish"
)

// ValidTerminalShellID reports whether id is a known terminal shell identifier.
func ValidTerminalShellID(id string) bool {
	switch id {
	case ShellGitBash, ShellPwsh, ShellPowerShell, ShellZsh, ShellBash, ShellFish:
		return true
	}
	return false
}

// ReportedCheckout describes a local git checkout discovered on a worker machine.
type ReportedCheckout struct {
	Path      string     `json:"path"`
	RemoteURL string     `json:"remote_url"` // normalized remote URL (e.g. github.com/org/repo)
	Branch    string     `json:"branch,omitempty"`
	Head      string     `json:"head,omitempty"`
	LastFetch *time.Time `json:"last_fetch,omitzero"`
}

// GPU is one graphics processor.
type GPU struct {
	// Vendor is nvidia, amd, intel or apple.
	Vendor string `json:"vendor"`
	Model  string `json:"model,omitempty"`
	// VRAMMB is its dedicated memory; for a unified-memory GPU (Apple silicon) the
	// machine's RAM, shared with the CPU.
	VRAMMB  uint64 `json:"vram_mb,omitzero"`
	Unified bool   `json:"unified,omitzero"`
	// Integrated marks a small integrated GPU (an iGPU next to a discrete one, or a
	// laptop's): not one to place compute work on.
	Integrated bool `json:"integrated,omitzero"`
	Cores      int  `json:"cores,omitzero"`
}

// MachineFacts captures cached OS, hardware, virtualization, filesystem, and safe tool/auth readiness facts.
// HomeFolder is a folder under the user's home: Path is home-relative with "/" between
// parts ("src", "source/repos"), on every OS.
type HomeFolder struct {
	Path  string `json:"path"`
	Repos int    `json:"repos"`
}

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
	// GPUs are the machine's graphics processors (SYR-M7F13).
	GPUs []GPU `json:"gpus,omitempty"`
	// Shells lists the supported terminal shells detected on this machine.
	Shells []TerminalShell `json:"shells,omitempty"`
	// Checkouts lists local git checkouts discovered under the daemon's configured roots.
	Checkouts []ReportedCheckout `json:"checkouts,omitempty"`
	// HomeFolders is the home survey (SYR-W3SVZ): which common code folders exist under
	// the user's home, and the checkout roots there, with how many git repos sit directly
	// in each. The hub suggests a fleet workspace folder from them. Home itself is not listed.
	HomeFolders []HomeFolder `json:"home_folders,omitempty"`
	// WorkdirRoot is the resolved base directory under which per-ticket worktrees
	// (<workdir_root>/.syre/tickets/<ticket sid>/<repo name>) are provisioned on this worker.
	WorkdirRoot string `json:"workdir_root,omitempty"`
	// StartMode (Windows) is how syred was started: "service", "background_task" (an
	// S4U task: runs whether or not anyone is signed in) or "user_session" (a logon task:
	// only runs while the user is signed in); "" elsewhere or when unknown.
	StartMode string `json:"start_mode,omitempty"`

	// Guardrails (docs/design/guardrails.md). IsDesktop picks the reserve cap.
	IsDesktop         bool   `json:"is_desktop,omitzero"`
	SurvivalReserveMB uint64 `json:"survival_reserve_mb,omitzero"`
	// PolicyReserveMB is what the machine policy keeps free on top of it (min free RAM
	// plus the user's reservation): the guardrails trip at the sum.
	PolicyReserveMB uint64 `json:"policy_reserve_mb,omitzero"`
	// GuardrailMode is the effective mode (passthrough | adaptive | adaptive_strict)
	// and GuardrailModeSource where it came from (auto | config | hub).
	GuardrailMode       string `json:"guardrail_mode,omitempty"`
	GuardrailModeSource string `json:"guardrail_mode_source,omitempty"`
	// GuardrailBaseMode is the mode without hub directives (config or auto): what a
	// machine.guardrails directive tightens or loosens against.
	GuardrailBaseMode string `json:"guardrail_base_mode,omitempty"`
	// GuardrailMechanism is how sessions are contained: systemd-scope,
	// process-group or job-object.
	GuardrailMechanism string `json:"guardrail_mechanism,omitempty"`

	CollectedAt time.Time `json:"collected_at"`
}

type WorkerFactsPayload struct {
	Facts MachineFacts `json:"facts"`
}

// WorkerInfo describes a worker node during registration.
type WorkerInfo struct {
	// ID is syred's persistent UUIDv7 identity: the hub's workers.id.
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
	// Version is syred's build version (ldflags main.version). Empty or "dev"
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
	// OpenChats are the chats whose engine is open (or whose turn runs) on the machine,
	// set by the hub from the heartbeat: an idle chat that is still open is a live
	// session, one that closed is not.
	OpenChats []string `json:"open_chats,omitempty"`
	// Load splits the machine's processes by session (from the process sampler,
	// docs/plan.md Phase 6); nil until the first sample.
	Load *MachineLoad `json:"load,omitzero"`
}

// MachineLoad is the machine's processes split by who runs them.
type MachineLoad struct {
	At time.Time `json:"at"`
	// Sessions are the managed and observed sessions with processes, each with its
	// whole process tree: helpers, builds, and processes that outlived their parent.
	Sessions []SessionLoad `json:"sessions,omitempty"`
	// Agent load is the sessions' processes plus syred and its helpers; other load is
	// everything else (the user's own work, the OS). CPU is 100 per fully used core.
	AgentCPUPercent float64 `json:"agent_cpu_percent"`
	OtherCPUPercent float64 `json:"other_cpu_percent"`
	AgentRSSMB      uint64  `json:"agent_rss_mb"`
	OtherRSSMB      uint64  `json:"other_rss_mb"`
	// TopOther are the busiest processes outside agent work.
	TopOther []ProcessLoad `json:"top_other,omitempty"`
	// Unreported counts processes the OS gave no CPU or memory for (macOS: other
	// users' processes unless syred runs as root); they are in neither total.
	Unreported int `json:"unreported,omitzero"`
	// UserIdleS is how long since someone used the keyboard or mouse here (nil: no
	// user session to read, e.g. a server reached over SSH). UserIdleSource says how
	// it was read: "hid" (macOS), "last_input" (Windows), "logind" (Linux; coarse, 0
	// until the desktop's own idle timeout passes).
	UserIdleS      *int64 `json:"user_idle_s,omitzero"`
	UserIdleSource string `json:"user_idle_source,omitempty"`
}

// ProcessLoad is one process in a load report.
type ProcessLoad struct {
	PID        int     `json:"pid"`
	Name       string  `json:"name"`
	CPUPercent float64 `json:"cpu_percent"`
	RSSMB      uint64  `json:"rss_mb"`
	// StartedAt identifies the process with PID (PIDs are reused): a kill names both.
	StartedAt time.Time `json:"started_at,omitzero"`
}

// ProcessExitPayload reports that a watched process exited: a long-lived daemon
// (its parent is init, systemd or launchd) that belonged to no session. It exited
// between After and Before (two samples). SSHShells are the interactive shells under
// sshd, outside Syre, at that time: who might have stopped it.
type ProcessExitPayload struct {
	Name      string     `json:"name"`
	PID       int        `json:"pid"`
	StartedAt time.Time  `json:"started_at,omitzero"`
	After     time.Time  `json:"after"`
	Before    time.Time  `json:"before"`
	SSHShells []SSHShell `json:"ssh_shells,omitempty"`
}

// SSHShell is an interactive shell under sshd that Syre did not start.
type SSHShell struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
	TTY  string `json:"tty,omitempty"` // "/dev/pts/3"
	// LastInput is when its terminal last had input (the tty's access time); zero
	// when unknown.
	LastInput time.Time `json:"last_input,omitzero"`
}

// ProcessKillPayload asks the daemon to stop one process (the user, from the UI). The
// start time must match, so a reused PID is never killed.
type ProcessKillPayload struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

// ProcessKillResult is the daemon's answer to a ProcessKillPayload.
type ProcessKillResult struct {
	Name   string `json:"name,omitempty"`
	Killed bool   `json:"killed"`
	// Forced: it ignored the polite signal and was killed outright.
	Forced bool   `json:"forced,omitzero"`
	Error  string `json:"error,omitempty"`
}

// SessionLoad is one session's processes in a sample. Exactly one of TaskID, ChatID
// and ExternalID (an observed session) is set.
type SessionLoad struct {
	TaskID     string  `json:"task_id,omitempty"`
	ChatID     string  `json:"chat_id,omitempty"`
	ExternalID string  `json:"external_id,omitempty"`
	CPUPercent float64 `json:"cpu_percent"` // 100 per fully used core
	RSSMB      uint64  `json:"rss_mb"`
	PeakRSSMB  uint64  `json:"peak_rss_mb"`
	Procs      int     `json:"procs"`
	// Orphans are processes kept by lineage: no longer under the session's tree (a
	// build daemon, a dev server), or still running after the session ended.
	Orphans int `json:"orphans,omitzero"`
}

// WorkerPortsPayload is a machine's listening TCP ports (worker.ports).
type WorkerPortsPayload struct {
	Ports       []ListeningPort `json:"ports"`
	CollectedAt time.Time       `json:"collected_at"`
}

type WorkerHeartbeatPayload struct {
	ActiveTaskIDs []string       `json:"active_task_ids"`
	ActiveChatIDs []string       `json:"active_chat_ids,omitempty"`
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

// ExternalSessionPayload reports a coding agent session launched outside Syre
// (discovered via agent hooks or lightweight passive process/transcript inspection).
type ExternalSessionPayload struct {
	ExternalID   string   `json:"external_id"` // stable per-host session key (e.g. "pid-1234" or hook session id)
	Agent        string   `json:"agent"`       // "claude" | "agy" | "codex" | "grok" | "copilot" | "cursor"
	Model        string   `json:"model,omitempty"`
	Cwd          string   `json:"cwd"`
	Branch       string   `json:"branch,omitempty"`
	Status       string   `json:"status"` // "working" | "idle" | "needs_you" | "done" | "fail"
	Summary      string   `json:"summary,omitempty"`
	ChangedFiles []string `json:"changed_files,omitempty"`
	Source       string   `json:"source"` // "hook" | "passive" | "transcript"
	// Account is the agent account whose session store holds a transcript-observed
	// session ("claude2"); "" when unknown.
	Account string `json:"account,omitempty"`
	// Tool is the tool the agent is running (a PreToolUse hook), "" when unknown or
	// finished (PostToolUse); with Status "working" it gives the coarse activity.
	Tool string `json:"tool,omitempty"`
	// Event is the raw hook event (PreToolUse, PostToolUse, Notification, Stop, ...).
	Event string `json:"event,omitempty"`
	// Waiting says what a needs_you session waits for ("approve Bash"), when the agent
	// tells.
	Waiting string `json:"waiting,omitempty"`
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

// TaskRepoBinding describes one repository bound to a ticket-backed task.
type TaskRepoBinding struct {
	RepoName     string `json:"repo_name"`
	RemoteURL    string `json:"remote_url,omitempty"`
	CheckoutPath string `json:"checkout_path,omitempty"`
	BaseBranch   string `json:"base_branch,omitempty"`
	TargetBranch string `json:"target_branch,omitempty"`
	// StackOn lists unmerged blocker branches (in dependency order) that this binding's
	// worktree must stack on top of instead of branching directly from BaseBranch.
	StackOn []string `json:"stack_on,omitempty"`
}

// TaskWorktree records a provisioned git worktree for one repo binding of a task.
type TaskWorktree struct {
	RepoName     string `json:"repo_name"`
	CheckoutPath string `json:"checkout_path"`
	WorktreePath string `json:"worktree_path"`
	Branch       string `json:"branch,omitempty"`
	// BaseSHA is the commit the worktree started from (after stacking, if any).
	BaseSHA string `json:"base_sha,omitempty"`
}

type TaskStartPayload struct {
	TaskID string `json:"task_id"`
	// SID is the task's short user-facing ID (t-…); the worker names the task's git
	// worktree directory after it (.syre-worktrees/<sid>). The protocol keys on TaskID.
	SID string `json:"sid,omitempty"`
	// TicketSID is the owning ticket's short ID (SYR-… / k-…) when this task works a
	// ticket with repo bindings; worktrees are provisioned at <workdir root>/.syre/tickets/<ticket_sid>/<repo_name>.
	TicketSID   string            `json:"ticket_sid,omitempty"`
	Bindings    []TaskRepoBinding `json:"bindings,omitempty"`
	Agent       string            `json:"agent"`
	Model       string            `json:"model,omitempty"`
	SessionKind string            `json:"session_kind,omitempty"`
	Workdir     string            `json:"workdir"`
	Branch      string            `json:"branch,omitempty"`
	UseWorktree bool              `json:"use_worktree"`
	// CreateWorkdir asks the worker to create a missing home-relative Workdir ("~/src")
	// first: the fleet workspace folder is made only where work lands (SYR-W3SVZ).
	CreateWorkdir  bool   `json:"create_workdir,omitzero"`
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
	// Shell selects the terminal shell ID (e.g. "git-bash", "pwsh", "powershell",
	// "zsh", "bash", "fish") for shell tasks; "" = worker default.
	Shell string `json:"shell,omitempty"`
	// UseGitBash asks Windows agent drivers to configure their shell tool to use
	// Git Bash when Git Bash is installed on the worker.
	UseGitBash bool `json:"use_git_bash,omitzero"`
}

type TaskCancelPayload struct {
	TaskID string `json:"task_id"`
	GraceS int    `json:"grace_s,omitzero"`
	// Hangup closes a user's terminal the way a terminal emulator does when its window
	// closes: SIGHUP to the shell and its foreground job, a short wait, then SIGKILL.
	// Without it a cancel interrupts, which an interactive shell ignores.
	Hangup bool `json:"hangup,omitzero"`
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
	TaskID       string         `json:"task_id"`
	Status       string         `json:"status"`
	EffectiveDir string         `json:"effective_dir,omitempty"`
	Worktrees    []TaskWorktree `json:"worktrees,omitempty"`
	ExitCode     *int           `json:"exit_code,omitzero"`
	Summary      string         `json:"summary,omitempty"`
	GitSummary   *GitSummary    `json:"git_summary,omitzero"`
	Digest       *RunDigest     `json:"digest,omitzero"`
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
	Kind    string `json:"kind"` // "worker" | "task" | "event" | "project" | "repo" | "ticket" | "ticket_edge" | "*_deleted"
	Worker  any    `json:"worker,omitzero"`
	Task    any    `json:"task,omitzero"`
	Event   any    `json:"event,omitzero"`
	Project any    `json:"project,omitzero"`
	Repo    any    `json:"repo,omitzero"`
	Ticket  any    `json:"ticket,omitzero"`
	// TicketEdge carries a blocked_by edge when Kind is "ticket_edge" or "ticket_edge_deleted".
	TicketEdge any `json:"ticket_edge,omitzero"`
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
	// Autopilot carries the fleet/project autopilot snapshot when Kind is "autopilot".
	Autopilot any `json:"autopilot,omitzero"`
	// Directive carries a policy directive when Kind is "directive".
	Directive any `json:"directive,omitzero"`
	// PolicyProposal carries a Commander set_policy approval card when Kind is "policy_proposal".
	PolicyProposal any `json:"policy_proposal,omitzero"`
	// Labels carries one machine's operator labels (WorkerID) when Kind is "machine_labels";
	// MachineGroups every saved group when Kind is "machine_groups"; ChatScope a Commander
	// chat's machines when Kind is "chat_scope" (SYR-FJA2T), or a ticket's Run on when Kind
	// is "ticket_scope" (SYR-0M4QQ).
	Labels        any `json:"labels,omitzero"`
	MachineGroups any `json:"machine_groups,omitzero"`
	ChatScope     any `json:"chat_scope,omitzero"`
}

// Policy directive kinds.
const (
	DirectiveKindMachinePause    = "machine.pause"
	DirectiveKindMachineThrottle = "machine.throttle"
	DirectiveKindMachineDrain    = "machine.drain"
	DirectiveKindAutopilotPause  = "autopilot.pause"
	DirectiveKindProviderDisable = "provider.disable"
	// DirectiveKindMachineGuardrails sets a machine's guardrail mode
	// (docs/design/guardrails.md). The newest active one wins.
	DirectiveKindMachineGuardrails = "machine.guardrails"
)

// WorkerDirectiveParams is the machine-facing copy of a directive's params.
// syred enforces pause, drain, the concurrency cap, the guardrail mode, and
// throttle CPU priority / cpu_ceiling_pct through its guardrails; heavy_ok is
// forwarded only.
type WorkerDirectiveParams struct {
	MaxConcurrency *int  `json:"max_concurrency,omitempty"`
	CPUCeilingPct  *int  `json:"cpu_ceiling_pct,omitempty"`
	HeavyOK        *bool `json:"heavy_ok,omitempty"`
	SuspendRunning bool  `json:"suspend_running,omitzero"`
	// Guardrails is machine.guardrails' mode: passthrough, adaptive or adaptive_strict.
	Guardrails string `json:"guardrails,omitempty"`
}

// WorkerDirective is one active directive for the machine that receives it.
type WorkerDirective struct {
	ID     string                `json:"id"`
	SID    string                `json:"sid"`
	Kind   string                `json:"kind"`
	Until  *time.Time            `json:"until,omitzero"`
	Params WorkerDirectiveParams `json:"params"`
}

// WorkerDirectivesPayload replaces the daemon's directive set (empty clears it) and
// its machine policy (nil clears it).
type WorkerDirectivesPayload struct {
	Directives []WorkerDirective `json:"directives"`
	Policy     *MachinePolicy    `json:"policy,omitzero"`
	// PolicyLifts are policy rules approved break-glass overrides suspend until Until.
	PolicyLifts []PolicyLift `json:"policy_lifts,omitempty"`
}

// PolicyLift suspends machine policy rules until Until (an approved override).
type PolicyLift struct {
	OverrideSID string    `json:"override_sid"`
	Rules       []string  `json:"rules"`
	Until       time.Time `json:"until"`
}

// Machine roles (MachinePolicy.Role).
const (
	MachineRoleDedicated   = "dedicated"    // runs any work (the default)
	MachineRoleInteractive = "interactive"  // someone works here: the user-active signal will gate heavy work (SYR-PSDFB)
	MachineRoleControlOnly = "control_only" // runs no work: hub, phone, laptop on the go
)

// Work classes (MachinePolicy.AllowedWork).
const (
	WorkChat   = "chat"   // an agent chat
	WorkTask   = "task"   // a dispatched task or terminal
	WorkTicket = "ticket" // a ticket run (task or chat working a ticket)
)

// MachinePolicy is a machine's standing rules for new work, set by the user in the UI
// or CLI (audited) and enforced twice: the hub admits no work that breaks it, and the
// daemon refuses such work even if the hub is wrong. MCP reads it but cannot change
// it; set_policy tightens through timed directives on top (docs/plan.md, Phase 8).
// Zero values mean no rule.
type MachinePolicy struct {
	Role string `json:"role,omitempty"`
	// MaxSessions caps the managed sessions hosted at once.
	MaxSessions int `json:"max_sessions,omitempty"`
	// MaxLoad refuses new work while the 1-minute load average is above it (Linux,
	// macOS; Windows has no load average).
	MaxLoad float64 `json:"max_load,omitempty"`
	// ReserveCPU keeps cores for the user: new work waits while the load average is
	// above the core count minus ReserveCPU.
	ReserveCPU float64 `json:"reserve_cpu,omitempty"`
	// MinFreeRAMMB and ReserveRAMMB are kept free on top of the guardrails' survival
	// reserve, never instead of it: new work waits while available memory is below
	// survival reserve + both, and the guardrails' watchdog trips at that floor too.
	MinFreeRAMMB int `json:"min_free_ram_mb,omitempty"`
	ReserveRAMMB int `json:"reserve_ram_mb,omitempty"`
	// QuietHours are windows when the machine accepts no new work, in TimeZone (an IANA
	// name, required with quiet hours).
	QuietHours []QuietWindow `json:"quiet_hours,omitempty"`
	TimeZone   string        `json:"time_zone,omitempty"`
	// AllowedWork limits the work classes the machine runs (empty: all).
	AllowedWork []string `json:"allowed_work,omitempty"`
}

// QuietWindow is a daily window, "22:00" to "07:00" (an end before the start runs past
// midnight). Days, when set, are the days the window starts on ("mon" … "sun").
type QuietWindow struct {
	Start string   `json:"start"`
	End   string   `json:"end"`
	Days  []string `json:"days,omitempty"`
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

// Plain-git delivery operations (SYR-H50YW) run on the daemon that owns the checkout,
// using that machine's git credentials.
const (
	MsgGitInfo        = "git.info"
	MsgGitInfoResult  = "git.info.result"
	MsgGitPush        = "git.push"
	MsgGitPushResult  = "git.push.result"
	MsgGitMerge       = "git.merge"
	MsgGitMergeResult = "git.merge.result"
)

type GitInfoRequest struct {
	Workdir string `json:"workdir"`
}

type GitInfoResult struct {
	OK            bool   `json:"ok"`
	IsGitRepo     bool   `json:"is_git_repo"`
	Branch        string `json:"branch,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	RemoteName    string `json:"remote_name,omitempty"`
	RemoteURL     string `json:"remote_url,omitempty"`
	HasRemote     bool   `json:"has_remote"`
	Dirty         bool   `json:"dirty"`
	Error         string `json:"error,omitempty"`
}

type GitPushRequest struct {
	Workdir string `json:"workdir"`
	// Branch to push ("" = current branch of Workdir).
	Branch string `json:"branch,omitempty"`
	// Remote name (default "origin"). Ignored when RemoteURL is set.
	Remote string `json:"remote,omitempty"`
	// RemoteURL pushes directly to a URL (e.g. a user's fork when upstream is read-only).
	RemoteURL string `json:"remote_url,omitempty"`
	// CommitMessage: when non-empty and the working tree has uncommitted changes, stage
	// and commit them before pushing.
	CommitMessage string `json:"commit_message,omitempty"`
	// RebaseOnto and StackBaseSHA, when both set, rebase the branch onto RebaseOnto
	// (replaying commits after StackBaseSHA) before pushing with --force-with-lease.
	RebaseOnto   string `json:"rebase_onto,omitempty"`
	StackBaseSHA string `json:"stack_base_sha,omitempty"`
}

type GitPushResult struct {
	OK        bool   `json:"ok"`
	Branch    string `json:"branch,omitempty"`
	RemoteURL string `json:"remote_url,omitempty"`
	HeadSHA   string `json:"head_sha,omitempty"`
	Error     string `json:"error,omitempty"`
}

type GitMergeRequest struct {
	Workdir string `json:"workdir"`
	// SourceBranch is the task branch to merge ("" = current branch of Workdir).
	SourceBranch string `json:"source_branch,omitempty"`
	// TargetBranch is the branch to merge into ("" = repo default branch).
	TargetBranch string `json:"target_branch,omitempty"`
	// Style is "squash" (default), "merge", or "rebase".
	Style string `json:"style,omitempty"`
	// Push pushes TargetBranch to Remote (default "origin") after merging when a remote exists.
	Push   bool   `json:"push"`
	Remote string `json:"remote,omitempty"`
	// CommitMessage is used for squash/merge commits (and auto-committing dirty worktree changes).
	CommitMessage string `json:"commit_message,omitempty"`
	// TaskSID is the short task ID used to name syre/merged/<sid> if the local target
	// branch is checked out with uncommitted changes.
	TaskSID string `json:"task_sid,omitempty"`
	// StackBaseSHA, when set on a stacked ticket whose blockers have already merged into
	// TargetBranch, rebases commits after StackBaseSHA onto TargetBranch before merging.
	StackBaseSHA string `json:"stack_base_sha,omitempty"`
}

type GitMergeResult struct {
	OK            bool     `json:"ok"`
	Merged        bool     `json:"merged"`
	Pushed        bool     `json:"pushed"`
	Conflict      bool     `json:"conflict,omitzero"`
	ConflictFiles []string `json:"conflict_files,omitempty"`
	TargetBranch  string   `json:"target_branch,omitempty"`
	MergeSHA      string   `json:"merge_sha,omitempty"`
	// MergedBranch is set when the local target branch was checked out dirty and the
	// merge result was left on syre/merged/<sid> instead of touching the checkout.
	MergedBranch string `json:"merged_branch,omitempty"`
	// LocalBehind is true when the merge was pushed to the remote while the local target
	// branch was left untouched because it is checked out in a worktree.
	LocalBehind bool `json:"local_behind,omitzero"`
	// Message carries a human-readable outcome notice (e.g. local behind or merged branch).
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

type GitWorktreeCleanupRequest struct {
	Workdir string `json:"workdir"`
	// RepoRoot, when set, identifies the primary git repository that owns Workdir
	// (used for per-ticket worktrees at <workdir root>/.syre/tickets/<ticket sid>/<repo name>).
	RepoRoot string `json:"repo_root,omitempty"`
	// PruneEmptyParent removes Workdir's parent directory if empty after removal.
	PruneEmptyParent bool `json:"prune_empty_parent,omitzero"`
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
	Kind      string `json:"kind"` // "syre_worktree" | "git_worktree" | "temp_dir"
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
	// MsgHostWakeLock (hub → worker) asks syred to hold or release an OS-level
	// idle-sleep inhibitor. syred answers with MsgHostWakeLockState.
	MsgHostWakeLock = "host.wakelock"
	// MsgHostWakeLockState (worker → hub) reports the current wake lock state. syred
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

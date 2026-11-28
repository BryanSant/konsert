import type { Device } from './device.models';
import type { SessionActivity, WorkerHeat } from './activity.models';
import { Cause, FleetPause } from './control.models';
import type { DeliveryMode, MergeStyle, Review } from './review.models';

export type PermissionMode = 'full_access' | 'sandboxed';
export type TaskOrigin = 'commander' | 'direct' | 'scratch';
export type SessionKind = 'chat' | 'terminal';

export type TaskStatus =
  | 'queued'
  | 'assigned'
  | 'running'
  | 'idle'
  | 'waiting_input'
  | 'suspended'
  | 'completed'
  | 'failed'
  | 'canceled';

export interface ListeningPort {
  port: number;
  process?: string;
  address?: string;
}

export interface WorkerMetrics {
  cpu_percent: number;
  mem_used_mb: number;
  mem_total_mb: number;
  load_avg: number;
  swap_used_mb?: number;
  swap_total_mb?: number;
  /** GPU utilization 0-100 where the platform reports it. */
  gpu_percent?: number;
}

/** GET /api/workers/{id}/ports: listening ports, fetched on demand (lsof / ss on the machine). */
export interface WorkerPorts {
  worker_id: string;
  ports: ListeningPort[];
  collected_at: string;
  /** False when the machine did not answer and this is the last list it sent. */
  fresh: boolean;
}

export interface TerminalShell {
  id: string;
  label: string;
  path?: string;
}

export interface ReportedCheckout {
  path: string;
  remote_url: string;
  branch?: string;
  head?: string;
  last_fetch?: string;
}

export interface MachineFacts {
  os_name?: string;
  os_version?: string;
  kernel_version?: string;
  cpu_model?: string;
  cpu_cores: number;
  ram_total_mb: number;
  swap_total_mb: number;
  swap_used_mb: number;
  storage_type?: string;
  disk_total_gb: number;
  disk_free_gb: number;
  tmp_is_tmpfs: boolean;
  is_wsl: boolean;
  is_container: boolean;
  is_mac_container: boolean;
  is_vm: boolean;
  virtualization?: string;
  /** Guardrails (docs/design/guardrails.md). */
  is_desktop?: boolean;
  survival_reserve_mb?: number;
  /** Effective mode: passthrough | adaptive | adaptive_strict. */
  guardrail_mode?: string;
  /** auto | config | hub */
  guardrail_mode_source?: string;
  guardrail_base_mode?: string;
  /** systemd-scope | process-group | job-object */
  guardrail_mechanism?: string;
  tools?: Record<string, string>;
  auth_ready?: Record<string, boolean>;
  /** Supported terminal shells detected on this machine. */
  shells?: TerminalShell[];
  /** Local git checkouts discovered on this machine. */
  checkouts?: ReportedCheckout[];
  /**
   * Windows: how syred was started. 'background_task' (an S4U task: starts at boot,
   * runs whether or not anyone is signed in), 'service', or 'user_session' (a logon task:
   * only runs while the user is signed in).
   */
  start_mode?: 'background_task' | 'service' | 'user_session';
  collected_at?: string;
}

export interface Worker {
  id: string;
  /** Short user-facing ID (shown in the UI); `id` (UUIDv7) keys all state. */
  sid: string;
  name: string;
  host_group: string;
  os: string;
  arch: string;
  isolation: 'host' | 'container' | 'vm' | string;
  permission_mode: PermissionMode;
  agents: string[];
  tags: Record<string, string>;
  max_concurrent: number;
  status: 'online' | 'offline' | string;
  active_tasks?: number;
  last_seen_at: string;
  /** Host Keep Awake state; absent until the worker reports it or one is requested. */
  wake_lock?: WorkerWakeLock;
  metrics?: WorkerMetrics;
  facts?: MachineFacts;
  /** syred build version from worker.hello; empty when unknown. */
  version?: string;
  /** How the hub (re)starts this machine's syred; absent when it was started by hand. */
  launch?: WorkerLaunch;
  /** SSH target the hub installs/starts syred through (launch 'ssh'). */
  ssh_target?: string;
  /**
   * How the machine reaches the hub: 'ssh' (the hub attaches over SSH with its own key),
   * 'local' (the hub's own machine, over syred's socket) or 'outbound' (syred dials
   * the hub with its machine credential).
   */
  transport?: WorkerTransport;
  /** Last start attempt by the hub; absent once the daemon is connected. */
  launch_state?: WorkerLaunchState;
  launch_error?: string;
  launch_at?: string;
  /** Outage of a hub-started daemon the hub cannot attach to; absent while connected. */
  restart?: WorkerRestart;
  /** Drained: the machine accepts no new work and finishes what it runs. */
  drained?: boolean;
  drained_at?: string;
  /** Operator's preferred default terminal shell ID for this machine ('' = automatic). */
  default_shell?: string;
  /** Windows: configure supported agent CLIs to use Git Bash for their shell tool (default true). */
  agents_use_git_bash?: boolean;
  /** Smoothed load score; absent while offline or before the first heartbeat. */
  heat?: WorkerHeat;
  /** The machine's outbound spool as it last reported it; absent before the first report. */
  spool?: WorkerSpool;
  /** The hub just woke from sleep and waits for this machine to re-attach. */
  reconnecting_after_sleep?: boolean;
}

/**
 * syred keeps everything that matters for the hub on disk until the hub applied it
 * (docs/design/spool.md): its size, oldest unsent record and the replay after an outage.
 */
export interface WorkerSpool {
  bytes: number;
  budget_bytes: number;
  /** Records not yet acknowledged by the hub. */
  records: number;
  oldest_unsent_at?: string;
  last_seq: number;
  acked_seq: number;
  replaying?: boolean;
  replay_sent?: number;
  replay_total?: number;
  /** Over budget after compaction: vitals and activity are no longer kept. */
  full?: boolean;
  full_reason?: string;
  dropped?: number;
  error?: string;
}

export type WorkerLaunch = 'local' | 'ssh';
export type WorkerTransport = 'ssh' | 'local' | 'outbound';
export type WorkerLaunchState = 'starting' | 'failed';

/**
 * The hub cannot attach to a daemon it starts: 'unreachable' (ssh cannot reach the host;
 * the hub keeps re-dialing) or 'down' (the host answers, syred is not running; the hub
 * restarts it with backoff).
 */
export interface WorkerRestart {
  state: 'unreachable' | 'down';
  /** Restarts so far in this outage. */
  attempt?: number;
  /** Earliest next restart. */
  next_at?: string;
  /** Last attach error. */
  error?: string;
  since: string;
}

/** The hub's own machine, as GET /api/setup reports it. */
export interface LocalHostInfo {
  hostname: string;
  syred_path?: string;
  syred_found: boolean;
  /** How to make syred available when it was not found. */
  fix?: string;
  /** Identity of the local daemon (running, or the one `syred install` will start). */
  worker_id?: string;
  worker_sid?: string;
  /** A syred on this machine is connected: adding it only attaches. */
  connected: boolean;
  /** The hub already starts this machine's syred. */
  added: boolean;
  launch_state?: WorkerLaunchState;
  launch_error?: string;
  /** `syred id` failed. */
  error?: string;
}

export interface SetupStatus {
  /** First run: no machines and the Add Machine dialog was never completed or dismissed. */
  needs_setup: boolean;
  local: LocalHostInfo;
}

/** POST /api/machines/join-codes: a short-lived code for `syred join` (SYR-RK9YD). */
export interface JoinCode {
  id: string;
  code: string;
  expires_at: string;
  hub_url: string;
  command: string;
}

/** GET /api/machines/join-codes/{id}. */
export interface JoinCodeStatus {
  id: string;
  status: 'pending' | 'used' | 'expired';
  expires_at: string;
  used_at?: string;
  worker_id?: string;
  worker_sid?: string;
  worker_name?: string;
}

export interface MachineCredential {
  id: string;
  sid: string;
  worker_id: string;
  source: 'join' | 'local' | 'ssh' | string;
  created_at: string;
  last_used_at?: string;
}

/** GET /api/workers/{id}/credential. */
export interface MachineCredentialInfo {
  worker_id: string;
  launch?: string;
  active: MachineCredential[];
  revoked: number;
}

export interface CredentialRotation {
  worker_id: string;
  worker_sid?: string;
  name: string;
  action: 'revoked' | 'failed';
  needs_join?: boolean;
  error?: string;
}

/** POST /api/machines/credentials/rotate. */
export interface RotationResponse {
  results: CredentialRotation[];
  needs_join: number;
  failed: number;
}

export interface AddLocalWorkerResult {
  worker_id: string;
  worker_sid?: string;
  state: 'connected' | 'starting';
}

/** Per-host wake lock (caffeinate / systemd-inhibit / SetThreadExecutionState). */
export interface WorkerWakeLock {
  /** The worker's syred reports wake lock state (older builds never do). */
  supported: boolean;
  /** Hub-side wish for this host; absent when nobody asked. */
  desired?: boolean;
  active: boolean;
  method?: string;
  error?: string;
  reason?: string;
  since?: string;
}

export interface WakeLockDesire {
  enabled: boolean;
  reason?: string;
}

export interface WakeLockStatus {
  fleet: WakeLockDesire | null;
  workers: Array<Partial<WorkerWakeLock> & { worker_id: string; online: boolean }>;
}

export interface GitSummary {
  base_sha?: string;
  head_sha?: string;
  commits?: string[];
  diff_stat?: string;
  dirty_files?: string[];
}

export interface AutonomyPolicy {
  retry_limit?: number;
  cross_review?: boolean;
  escalate_on_blocker?: boolean;
  expand_scope?: boolean;
}

export interface Project {
  id: string;
  /** Short user-facing ID (shown in the UI); `id` (UUIDv7) keys all state. */
  sid: string;
  name: string;
  is_default: boolean;
  /** The project's current goal as the commander states it (set_narration). */
  headline?: string;
  headline_at?: string;
  verify_cmd?: string;
  autonomy?: AutonomyPolicy;
  autopilot?: boolean;
  autopilot_paused?: boolean;
  delivery_mode?: DeliveryMode;
  merge_style?: MergeStyle;
  created_at: string;
}

export interface Repo {
  id: string;
  /** Short user-facing ID (shown in the UI); `id` (UUIDv7) keys all state. */
  sid: string;
  project_id: string;
  name: string;
  remote_url: string;
  default_branch?: string;
  verify_cmd?: string;
  delivery_mode?: DeliveryMode;
  merge_style?: MergeStyle;
  created_at: string;
}

export type TicketKind = 'initiative' | 'epic' | 'feature' | 'bug' | 'chore' | 'spike';
export type TicketStatus = 'backlog' | 'ready' | 'running' | 'needs_you' | 'review' | 'done' | 'failed' | 'canceled';
export type TicketPriority = 'P0' | 'P1' | 'P2' | 'P3';

export interface AcceptanceItem {
  text: string;
  done: boolean;
}

export interface TicketHints {
  machine?: string;
  agent?: string;
  model?: string;
  effort?: string;
  os?: string;
  arch?: string;
  gpu?: boolean;
  min_ram_gb?: number;
  tools?: string[];
}

export interface TicketLease {
  worker_id: string;
  task_id?: string;
  expires_at: string;
  heartbeat_at?: string;
}

export interface Ticket {
  id: string;
  sid: string;
  project_id: string;
  parent_id?: string;
  kind: TicketKind;
  title: string;
  body?: string;
  acceptance: AcceptanceItem[];
  labels: string[];
  priority: TicketPriority;
  status: TicketStatus;
  hints?: TicketHints;
  verify_cmd?: string;
  autonomy?: AutonomyPolicy;
  /** How many times Autopilot has attempted this ticket. */
  attempt?: number;
  lease?: TicketLease;
  orphan_policy?: string;
  created_at: string;
  updated_at: string;
}

export interface TicketComment {
  id: string;
  ticket_id: string;
  author: string;
  body: string;
  created_at: string;
}

/** One loop iteration of a ticket (GET /api/tickets/{id}/attempts). */
export interface Attempt {
  id: string;
  ticket_id?: string;
  task_id: string;
  chat_id?: string;
  attempt: number;
  worker_id?: string;
  account_id?: string;
  agent?: string;
  model?: string;
  machine?: string;
  queued_at?: string;
  started_at?: string;
  ended_at?: string;
  queue_wait_s?: number;
  duration_s?: number;
  exit_code?: number;
  status?: string;
  failure_category?: string;
  verify_cmd?: string;
  verify_exit?: number;
  verify_passed?: number;
  verify_failed?: number;
  tokens_in?: number;
  tokens_out?: number;
  cost_usd?: number;
  git_json?: string;
}

/** One-line view of a ticket's latest run digest (board cards). */
export interface TicketDigestLine {
  ticket_id: string;
  summary?: string;
  agent?: string;
  machine?: string;
  attempt?: number;
}

/** Compact post-run digest (protocol.RunDigest). */
export interface RunDigest {
  ticket_sid?: string;
  attempt?: number;
  status: string;
  machine?: string;
  agent?: string;
  model?: string;
  duration_s?: number;
  summary?: string;
  interface?: string;
  followups?: string[];
  git?: Record<string, string>;
  verify?: { cmd?: string; exit?: number; passed?: number; failed?: number };
  failure?: { category?: string; tests?: string[]; tail?: string[] };
  question?: { text?: string; context?: string[] };
}

export interface TicketEdge {
  ticket_id: string;
  blocked_by_id: string;
  created_at: string;
}

export interface TicketRepoBinding {
  ticket_id: string;
  repo_id: string;
  base_branch?: string;
  target_branch?: string;
}

export interface ExternalRef {
  ticket_id: string;
  tracker: string;
  external_id: string;
  url?: string;
  version?: string;
}

export interface TicketRollup {
  ticket_id: string;
  total: number;
  done: number;
  running: number;
  blocked: number;
}

export interface SessionDefaults {
  machine_id: string;
  machine_source: 'explicit' | 'localhost' | 'last_used' | 'guess' | string;
  agent: string;
  agent_source: 'explicit' | 'default' | 'last_used' | 'guess' | string;
  model: string;
  model_source: 'explicit' | 'default' | 'last_used' | 'guess' | string;
  available_agents: string[];
}

export interface GitChangedFile {
  path: string;
  status: string;
  added: number;
  deleted: number;
  diff?: string;
}

export interface GitInspectResult {
  is_git_repo: boolean;
  branch?: string;
  effective_dir?: string;
  is_worktree: boolean;
  added: number;
  deleted: number;
  files: GitChangedFile[];
  commits?: string[];
  error?: string;
}

export interface GitCommitResult {
  ok: boolean;
  commit_sha?: string;
  summary?: string;
  error?: string;
}

export interface FSEntry {
  name: string;
  path: string;
  is_dir: boolean;
  is_git_repo?: boolean;
}

export interface FSListResult {
  current_dir: string;
  parent_dir?: string;
  entries: FSEntry[];
  error?: string;
}

export interface SSHHost {
  alias: string;
  hostname: string;
  user?: string;
  port?: string;
  display: string;
  added: boolean;
}

export interface SSHBootstrapStage {
  stage: string;
  label: string;
  status: 'ok' | 'skipped' | 'error' | string;
  detail?: string;
}

export interface SSHBootstrapResult {
  ok: boolean;
  target: string;
  remote_os?: string;
  remote_arch?: string;
  stage?: string;
  stages?: SSHBootstrapStage[];
  hooks_installed?: boolean;
  service_mode?: string;
  message?: string;
  error?: string;
  /** The remote daemon's identity (workers.id) and the --name it was installed with. */
  worker_id?: string;
  name?: string;
  /** Installed as a Windows logon task that starts at the user's next login. */
  start_deferred?: boolean;
  /** Set (with host_keys) when the host key is unknown; resubmit with accept_host_key. */
  host_key_confirmation_required?: boolean;
  host_keys?: SSHHostKey[];
}

/** An SSH host key offered by a target, in OpenSSH SHA256 fingerprint form. */
export interface SSHHostKey {
  type: string;
  fingerprint: string;
}

/** Bootstrap stage reported when an unknown host key needs operator confirmation. */
export const SSH_STAGE_HOST_KEY_CONFIRMATION = 'host_key_confirmation';

export interface AppSettings {
  llm_provider: string;
  llm_model: string;
  llm_base_url: string;
  llm_api_key_set: boolean;
  default_agent?: string;
  /** Default agent account id (worker-local, e.g. "claude2"). */
  default_account_id?: string;
  default_model?: string;
  default_effort?: string;
  last_used_machine?: string;
  last_used_agent?: string;
  last_used_model?: string;
  default_permission_mode: PermissionMode;
  terminal_font_size: number;
  notify_sound: boolean;
  notify_desktop: boolean;
  /** Devices stay signed in while used at least this often (sliding expiry, days). */
  device_idle_days?: number;
  /** Global default delivery mode ('' = auto: pr_merge with remote, direct without). */
  default_delivery_mode?: DeliveryMode;
  /** Global default merge style ('squash' | 'merge' | 'rebase'). */
  default_merge_style?: MergeStyle;
  /** Global Autopilot Play switch: every project plays unless explicitly paused. */
  autopilot_global?: boolean;
  /** Commander set_policy: tighten (default), confirm (approval card), or none (read-only). */
  commander_policy_writes?: 'tighten' | 'confirm' | 'none' | string;
}

export type AutopilotDisplayState = 'playing' | 'paused' | 'finished';

export interface AutopilotCounts {
  dispatchable: number;
  running: number;
  needs_you: number;
  review: number;
}

export interface AutopilotProjectState {
  project_id: string;
  project_sid: string;
  project_name: string;
  play: boolean;
  autopilot: boolean;
  autopilot_paused: boolean;
  global: boolean;
  fleet_paused: boolean;
  state: AutopilotDisplayState;
  counts: AutopilotCounts;
}

export interface AutopilotState {
  global: boolean;
  fleet_paused: boolean;
  state: AutopilotDisplayState;
  counts: AutopilotCounts;
  projects: AutopilotProjectState[];
}

export interface Task {
  id: string;
  /** Short user-facing ID (shown in the UI); `id` (UUIDv7) keys all state. */
  sid: string;
  worker_id: string;
  project_id?: string;
  repo_id?: string;
  agent: string;
  model?: string;
  session_kind?: SessionKind | string;
  workdir: string;
  effective_dir: string;
  branch: string;
  mode: 'oneshot' | 'interactive' | string;
  origin?: TaskOrigin | string;
  permission_mode: PermissionMode;
  orphan_policy: string;
  prompt: string;
  status: TaskStatus;
  summary: string;
  git_summary_json?: string;
  /** Latest run digest, when the task worked a ticket. */
  digest_json?: string;
  exit_code?: number | null;
  created_at: string;
  started_at?: string | null;
  completed_at?: string | null;
  /** Attribution of the last status transition. */
  cause?: Cause;
  /** Who suspended it ("user" or "pause"); empty unless suspended. */
  suspended_by?: string;
  /** The commander chat that dispatched it. */
  parent_chat_id?: string;
  /** Short name (dispatch_task title). */
  title?: string;
  /** The commander's one-line "now" (set_narration). */
  narration?: string;
  narration_at?: string;
  /** The ka ticket it works on; the title is resolved by the hub. */
  ticket_id?: string;
  ticket_title?: string;
  /** Pinned by the user ("Keep"): retention never compacts it. */
  keep?: boolean;
  /** When retention dropped its verbose events (milestones are kept). */
  compacted_at?: string | null;
}

export interface TaskEvent {
  id: number;
  task_id: string;
  kind: 'milestone' | 'tool_call' | 'file_edit' | 'control' | 'error' | 'exit' | 'waiting_input' | 'summary' | string;
  message: string;
  meta_json?: string;
  created_at: string;
}

/** One operator control action from the fleet-wide audit log (GET /api/audit). */
export interface AuditEvent {
  id: number;
  created_at: string;
  /** Authenticated identity: session user, Tailscale login, "token", "mcp", "commander". */
  user: string;
  /** Path the action came through: "rest" | "cli" | "mcp" | "commander" | "legacy". */
  source?: string;
  action: string;
  worker_id?: string;
  worker_sid?: string;
  task_id?: string;
  task_sid?: string;
  message?: string;
  meta_json?: string;
}

export interface AuditListResponse {
  events: AuditEvent[];
  /** before_id for the next (older) page; absent on the last page. */
  next_cursor?: number;
}

export interface AuditQuery {
  worker_id?: string;
  task_id?: string;
  user?: string;
  action?: string;
  since?: string;
  until?: string;
  before_id?: number;
  limit?: number;
}

export interface UsageBucket {
  provider: string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  requests: number;
}

export interface SubscriptionQuota {
  id?: string;
  provider: string;
  label: string;
  window_label?: string;
  used_percent: number;
  reset_at?: string | null;
  source?: string;
  worker_id?: string;
  updated_at?: string;
}

export interface UsageSummary {
  total_input_tokens: number;
  total_output_tokens: number;
  total_requests: number;
  by_provider: UsageBucket[];
  quotas?: SubscriptionQuota[];
}

export interface ChatSession {
  id: string;
  /** Short user-facing ID (shown in the UI); `id` (UUIDv7) keys all state. */
  sid: string;
  title: string;
  provider: string;
  model: string;
  created_at: string;
  updated_at: string;
  /** agent: a subscription agent CLI on a worker; api: the optional API-key commander. */
  backend?: 'agent' | 'api' | string;
  worker_id?: string;
  agent?: string;
  account_id?: string;
  effort?: string;
  approval_mode?: string;
  cwd?: string;
  agent_session_id?: string;
  /** ChatStatus of the last turn (starting, idle, working, needs_you, done, failed, cancelled). */
  status?: string;
  fleet_tools?: boolean;
  /** Last seq the hub persisted for this chat (for catch-up). */
  last_seq?: number;
  /** Attribution of the last status transition. */
  cause?: Cause;
  /** Who suspended it ("user" or "pause"); empty unless suspended. */
  suspended_by?: string;
  /** Pinned by the user ("Keep"): retention never compacts it. */
  keep?: boolean;
  /** When retention last dropped detailed events (tool calls, reasoning) of turns that
   *  ended before compacted_before; the transcript is kept. */
  compacted_at?: string | null;
  compacted_before?: string | null;
}

export interface ChatMessage {
  id: number;
  session_id: string;
  role: 'user' | 'assistant' | 'tool';
  content: string;
  tool_json?: string;
  created_at: string;
}

export interface ChatToolCard {
  session_id: string;
  call_id: string;
  name: string;
  args?: unknown;
  result?: unknown;
  status: 'running' | 'completed' | 'error';
}

export interface AuthMe {
  authenticated: boolean;
  auth_required: boolean;
  user: string;
  /** How this browser authenticated: device | recovery | tailscale | none. */
  method?: string;
  /** This browser's own device credential, when it signed in by pairing. */
  device?: Device;
  provider: string;
  model: string;
}

/** Kind-specific policy. Throttle uses max_concurrency; pause uses suspend_running. */
export interface DirectiveParams {
  max_concurrency?: number;
  cpu_ceiling_pct?: number;
  heavy_ok?: boolean;
  suspend_running?: boolean;
  /** provider.disable: agent to prefer when the target is skipped. */
  fallback?: string;
  /** provider.disable: "agent" (a whole provider) or "account" (one account id). */
  scope?: 'agent' | 'account' | string;
  /** machine.guardrails: passthrough | adaptive | adaptive_strict. */
  guardrails?: string;
}

/**
 * A typed, timed policy directive. machine.* targets a worker; autopilot.pause
 * targets a project; provider.disable targets an agent name or an account id.
 */
export interface PolicyDirective {
  id: string;
  sid: string;
  kind: string;
  target: string;
  params?: DirectiveParams;
  /** RFC3339. Absent when the directive lasts until someone ends it. */
  until?: string;
  provenance?: string;
  actor?: string;
  reason?: string;
  created_at: string;
  ended_at?: string;
  ended_by?: string;
}

export interface CreateDirectiveRequest {
  kind: string;
  target: string;
  until?: string;
  reason?: string;
  params?: DirectiveParams;
}

/**
 * A Commander set_policy change held for the user when commander_policy_writes
 * is confirm. Approval creates the directive; denial drops it.
 */
export interface PolicyProposal {
  id: string;
  sid: string;
  chat_id?: string;
  kind: string;
  target: string;
  params?: DirectiveParams;
  until?: string;
  reason?: string;
  summary: string;
  status: 'pending' | 'approved' | 'denied' | string;
  actor_id?: string;
  created_at: string;
  decided_at?: string;
  decided_by?: string;
  applied_id?: string;
}

export interface FleetSnapshotResponse {
  workers: Worker[];
  active_tasks_count: number;
  tasks: Task[];
  projects?: Project[];
  repos?: Repo[];
  usage?: UsageSummary;
  /** The hub's own build version ("dev" for unstamped local builds). */
  controller_version?: string;
  /** Fleet-wide Pause everything state. */
  pause?: FleetPause;
  /** Fleet-wide and per-project Autopilot Play/Pause state and ticket counts. */
  autopilot?: AutopilotState;
  /** Latest activity of every known session. */
  activity?: SessionActivity[];
  /** Review decisions and Expedite flags (SYR-BM8JR). */
  reviews?: Review[];
  /** Active policy directives (machine pause, throttle, drain, autopilot pause). */
  directives?: PolicyDirective[];
}

export interface DispatchTaskRequest {
  worker_selector?: {
    worker_id?: string;
    host_group?: string;
    os?: string;
    tags?: Record<string, string>;
  };
  project_id?: string;
  repo_id?: string;
  ticket_id?: string;
  session_kind?: SessionKind;
  agent?: string;
  /** Worker-local agent account (its env profile is applied to the session). */
  account_id?: string;
  model?: string;
  /** The model's effort id (AgentEffort.id); empty = the model default. */
  effort?: string;
  /** The agent's approval mode id (AgentApprovalMode.id); empty = the account default. */
  approval_mode?: string;
  /** Terminal shell ID (e.g. 'git-bash', 'pwsh', 'powershell', 'zsh', 'bash', 'fish'); empty = machine default. */
  shell?: string;
  repo_path?: string;
  branch?: string;
  use_worktree?: boolean;
  prompt?: string;
  mode?: 'oneshot' | 'interactive';
  origin?: TaskOrigin;
  permission_mode?: PermissionMode;
  timeout_seconds?: number;
}

export interface WorkspaceTab {
  id: string; // 'commander' or taskId
  kind: 'commander' | 'terminal';
  title: string;
  pinned: boolean;
  taskId?: string;
  workerId?: string;
  agent?: string;
  model?: string;
  sessionKind?: SessionKind | string;
  origin?: TaskOrigin | string;
  interactiveMode: boolean;
}

export interface Envelope<T = unknown> {
  type: string;
  id?: string;
  ts: string;
  payload?: T;
}

export interface PtyFrameEvent {
  frameType: number; // 0x01 snapshot, 0x02 data
  taskId: string;
  data: Uint8Array;
}

export const FRAME_PTY_SNAPSHOT = 0x01;
export const FRAME_PTY_DATA = 0x02;
export const FRAME_PTY_STDIN = 0x03;
export const FRAME_TASK_ID_SIZE = 36; // a UUIDv7 task id
export const FRAME_HEADER_SIZE = 1 + FRAME_TASK_ID_SIZE;

/** ID of the built-in Default project (Go: ids.DefaultProjectID; its sid is p-00000). */
export const DEFAULT_PROJECT_ID = '00000000-0000-7000-8000-000000000001';

/** Settings → Storage (GET /api/storage). */
export interface StorageKind {
  kind: string;
  label: string;
  bytes: number;
  rows: number;
}

export interface StorageStats {
  path: string;
  file_bytes: number;
  wal_bytes: number;
  free_bytes: number;
  page_size: number;
  method: 'dbstat' | 'estimate' | string;
  breakdown: StorageKind[];
  auto_vacuum: string;
  measured_at: string;
}

export interface RetentionDays {
  detail_days: number;
  fleet_days: number;
  fleet_summary_days: number;
  usage_days: number;
  usage_daily_days: number;
  // Per-session and agent/other load history: per minute, then hourly.
  metrics_days: number;
  metrics_hourly_days: number;
}

export interface RetentionBound {
  Min: number;
  Max: number;
}

export interface CompactionStep {
  step: string;
  rows: number;
  items: number;
  done: boolean;
}

export interface CompactionRun {
  trigger?: string;
  running: boolean;
  step?: string;
  steps?: CompactionStep[] | null;
  batches?: number;
  rows?: number;
  freed_bytes?: number;
  longest_batch_ms?: number;
  started_at?: string;
  finished_at?: string;
  duration_ms?: number;
  complete?: boolean;
  error?: string;
}

export interface StorageResponse {
  stats: StorageStats;
  retention: RetentionDays;
  defaults: RetentionDays;
  bounds: Record<keyof RetentionDays, RetentionBound>;
  retention_text: string[];
  compaction: CompactionRun;
}

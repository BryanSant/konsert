export type PermissionMode = 'full_access' | 'sandboxed';
export type TaskOrigin = 'orchestrator' | 'direct' | 'scratch';
export type SessionKind = 'chat' | 'terminal';

export type TaskStatus =
  | 'queued'
  | 'assigned'
  | 'running'
  | 'idle'
  | 'waiting_input'
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
  listening_ports?: ListeningPort[];
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
  tools?: Record<string, string>;
  auth_ready?: Record<string, boolean>;
  collected_at?: string;
}

export interface Worker {
  id: string;
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
  /** konsertd build version from worker.hello; empty when unknown. */
  version?: string;
}

/** Per-host wake lock (caffeinate / systemd-inhibit / SetThreadExecutionState). */
export interface WorkerWakeLock {
  /** The worker's konsertd reports wake lock state (older builds never do). */
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

export interface Space {
  id: string;
  worker_id: string;
  name: string;
  path: string;
  branch?: string;
  created_at: string;
}

export interface Workspace {
  id: string;
  name: string;
  is_default: boolean;
  created_at: string;
}

export interface Project {
  id: string;
  workspace_id: string;
  name: string;
  is_default: boolean;
  created_at: string;
}

export interface Repo {
  id: string;
  project_id: string;
  worker_id: string;
  name: string;
  path: string;
  remote_url?: string;
  created_at: string;
}

export interface Worktree {
  id: string;
  repo_id: string;
  branch?: string;
  path: string;
  is_primary: boolean;
  created_at: string;
}

export interface TaxonomySnapshot {
  workspaces: Workspace[];
  projects: Project[];
  repos: Repo[];
  worktrees: Worktree[];
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
}

export interface Task {
  id: string;
  worker_id: string;
  space_id?: string;
  workspace_id?: string;
  project_id?: string;
  repo_id?: string;
  worktree_id?: string;
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
  exit_code?: number | null;
  created_at: string;
  started_at?: string | null;
  completed_at?: string | null;
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
  /** Authenticated identity: session user, Tailscale login, "token", "mcp", "orchestrator". */
  user: string;
  /** Path the action came through: "rest" | "cli" | "mcp" | "orchestrator" | "legacy". */
  source?: string;
  action: string;
  worker_id?: string;
  task_id?: string;
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
  title: string;
  provider: string;
  model: string;
  created_at: string;
  updated_at: string;
  /** agent: a subscription agent CLI on a worker; api: the optional API-key orchestrator. */
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
  fleet_token_required?: boolean;
  user: string;
  provider: string;
  model: string;
}

export interface FleetSnapshotResponse {
  workers: Worker[];
  active_tasks_count: number;
  tasks: Task[];
  spaces?: Space[];
  taxonomy?: TaxonomySnapshot;
  usage?: UsageSummary;
  /** The hub's own build version ("dev" for unstamped local builds). */
  controller_version?: string;
}

export interface DispatchTaskRequest {
  worker_selector?: {
    worker_id?: string;
    host_group?: string;
    os?: string;
    tags?: Record<string, string>;
  };
  space_id?: string;
  workspace_id?: string;
  project_id?: string;
  repo_id?: string;
  worktree_id?: string;
  session_kind?: SessionKind;
  agent?: string;
  /** Worker-local agent account (its env profile is applied to the session). */
  account_id?: string;
  model?: string;
  /** The model's effort id (AgentEffort.id); empty = the model default. */
  effort?: string;
  /** The agent's approval mode id (AgentApprovalMode.id); empty = the account default. */
  approval_mode?: string;
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
  id: string; // 'orchestrator' or taskId
  kind: 'orchestrator' | 'terminal';
  title: string;
  pinned: boolean;
  taskId?: string;
  workerId?: string;
  spaceId?: string;
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
export const FRAME_TASK_ID_SIZE = 24;
export const FRAME_HEADER_SIZE = 1 + FRAME_TASK_ID_SIZE;

/** ID of the built-in Default project (Go: ids.DefaultProject). */
export const DEFAULT_PROJECT_ID = 'PRJ-00000';

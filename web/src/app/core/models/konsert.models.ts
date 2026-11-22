export type PermissionMode = 'full_access' | 'sandboxed';
export type TaskOrigin = 'orchestrator' | 'direct' | 'scratch';

export type TaskStatus =
  | 'queued'
  | 'assigned'
  | 'running'
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
  metrics?: WorkerMetrics;
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

export interface AppSettings {
  llm_provider: string;
  llm_model: string;
  llm_base_url: string;
  llm_api_key_set: boolean;
  default_permission_mode: PermissionMode;
  terminal_font_size: number;
  notify_sound: boolean;
  notify_desktop: boolean;
}

export interface Task {
  id: string;
  worker_id: string;
  space_id?: string;
  agent: string;
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

export interface UsageBucket {
  provider: string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  requests: number;
}

export interface UsageSummary {
  total_input_tokens: number;
  total_output_tokens: number;
  total_requests: number;
  by_provider: UsageBucket[];
}

export interface ChatSession {
  id: string;
  title: string;
  provider: string;
  model: string;
  created_at: string;
  updated_at: string;
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
  usage?: UsageSummary;
}

export interface DispatchTaskRequest {
  worker_selector?: {
    worker_id?: string;
    host_group?: string;
    os?: string;
    tags?: Record<string, string>;
  };
  space_id?: string;
  agent: string;
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
  origin?: TaskOrigin | string;
  interactiveMode: boolean; // Direct/Scratch = true by default; Orchestrated = false (Read-Only)
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
export const FRAME_TASK_ID_SIZE = 36;
export const FRAME_HEADER_SIZE = 37;

import { Injectable } from '@angular/core';
import {
  AppSettings,
  AuthMe,
  ChatMessage,
  ChatSession,
  DispatchTaskRequest,
  FSListResult,
  FleetSnapshotResponse,
  GitCommitResult,
  GitInspectResult,
  Project,
  Repo,
  SSHBootstrapResult,
  SSHHost,
  SessionDefaults,
  Space,
  TaskEvent,
  TaxonomySnapshot,
  Workspace,
  Worktree,
} from '../models/konsert.models';

/** Error thrown for non-2xx responses; carries the HTTP status. */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

/**
 * Custom header sent on every PWA request. Browsers never attach custom headers to
 * cross-site form posts / simple requests, so the hub can treat its presence as a
 * CSRF signal alongside Origin / Sec-Fetch-Site checks.
 */
export const KONSERT_REQUEST_HEADER = 'X-Konsert-Request';

/** Paths whose 401 means "bad credentials" rather than "session expired". */
const AUTH_EXEMPT_PATHS = ['/api/auth/login'];

@Injectable({ providedIn: 'root' })
export class ApiClientService {
  private unauthorizedHandler: (() => void) | null = null;

  /** Registers the callback invoked when any request reports an expired/invalid session. */
  setUnauthorizedHandler(fn: (() => void) | null): void {
    this.unauthorizedHandler = fn;
  }

  /**
   * Low-level fetch wrapper: same-origin credentials, the CSRF marker header, a JSON
   * Content-Type for every state-changing request (bodiless ones get `{}`), 401
   * session-expiry detection and ApiError mapping for non-2xx responses.
   */
  private async send(path: string, init: RequestInit = {}, opts: { json?: boolean } = {}): Promise<Response> {
    const method = (init.method ?? 'GET').toUpperCase();
    const headers = new Headers(init.headers);
    headers.set(KONSERT_REQUEST_HEADER, '1');
    let body = init.body;
    if (opts.json !== false) {
      if (!headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
      if (body == null && (method === 'POST' || method === 'PUT' || method === 'PATCH')) {
        body = '{}';
      }
    }
    const res = await fetch(path, { ...init, method, headers, body, credentials: 'same-origin' });
    if (!res.ok) {
      if (res.status === 401 && !AUTH_EXEMPT_PATHS.includes(path.split('?')[0])) {
        this.unauthorizedHandler?.();
      }
      throw new ApiError(await readErrorMessage(res), res.status);
    }
    return res;
  }

  private async request<T>(path: string, init?: RequestInit): Promise<T> {
    const res = await this.send(path, init);
    return (await res.json()) as T;
  }

  getMe(): Promise<AuthMe> {
    return this.request<AuthMe>('/api/auth/me');
  }

  login(token: string): Promise<{ authenticated: boolean; user: string }> {
    return this.request('/api/auth/login', {
      method: 'POST',
      body: JSON.stringify({ token }),
    });
  }

  logout(): Promise<{ ok: boolean }> {
    return this.request('/api/auth/logout', { method: 'POST' });
  }

  getFleet(): Promise<FleetSnapshotResponse> {
    return this.request<FleetSnapshotResponse>('/api/fleet');
  }

  dispatchTask(req: DispatchTaskRequest): Promise<{ task_id: string; worker_id?: string; status: string }> {
    return this.request('/api/tasks', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  steerTask(taskId: string, inputText: string, appendNewline = true): Promise<{ ok: boolean }> {
    return this.request(`/api/tasks/${encodeURIComponent(taskId)}/steer`, {
      method: 'POST',
      body: JSON.stringify({ input_text: inputText, append_newline: appendNewline }),
    });
  }

  cancelTask(taskId: string): Promise<{ ok: boolean }> {
    return this.request(`/api/tasks/${encodeURIComponent(taskId)}/cancel`, {
      method: 'POST',
    });
  }

  getTaskEvents(taskId: string, afterId = 0): Promise<{ events: TaskEvent[] }> {
    return this.request(`/api/tasks/${encodeURIComponent(taskId)}/events?after_id=${afterId}`);
  }

  listChatSessions(): Promise<{ sessions: ChatSession[] }> {
    return this.request('/api/chat/sessions');
  }

  createChatSession(title = 'Orchestrator Session'): Promise<ChatSession> {
    return this.request('/api/chat/sessions', {
      method: 'POST',
      body: JSON.stringify({ title }),
    });
  }

  listChatMessages(sessionId: string): Promise<{ messages: ChatMessage[] }> {
    return this.request(`/api/chat/sessions/${encodeURIComponent(sessionId)}/messages`);
  }

  renameChatSession(sessionId: string, title: string): Promise<ChatSession> {
    return this.request(`/api/chat/sessions/${encodeURIComponent(sessionId)}`, {
      method: 'PATCH',
      body: JSON.stringify({ title }),
    });
  }

  deleteChatSession(sessionId: string): Promise<{ ok: boolean; id: string }> {
    return this.request(`/api/chat/sessions/${encodeURIComponent(sessionId)}`, {
      method: 'DELETE',
    });
  }

  async uploadAttachment(
    file: File,
    opts?: { workerId?: string; sessionId?: string }
  ): Promise<{ ok: boolean; worker_id: string; remote_path: string; filename: string; size: number }> {
    const fd = new FormData();
    fd.append('file', file, file.name);
    if (opts?.workerId) fd.append('worker_id', opts.workerId);
    if (opts?.sessionId) fd.append('session_id', opts.sessionId);

    // No explicit Content-Type: the browser sets multipart/form-data with the boundary.
    const res = await this.send('/api/attachments', { method: 'POST', body: fd }, { json: false });
    return (await res.json()) as { ok: boolean; worker_id: string; remote_path: string; filename: string; size: number };
  }

  listSpaces(workerId = ''): Promise<{ spaces: Space[] }> {
    const q = workerId ? `?worker_id=${encodeURIComponent(workerId)}` : '';
    return this.request(`/api/spaces${q}`);
  }

  createSpace(req: { worker_id: string; name?: string; path: string; branch?: string }): Promise<Space> {
    return this.request('/api/spaces', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  deleteSpace(spaceId: string): Promise<{ ok: boolean; id: string }> {
    return this.request(`/api/spaces/${encodeURIComponent(spaceId)}`, {
      method: 'DELETE',
    });
  }

  getSettings(): Promise<AppSettings> {
    return this.request('/api/settings');
  }

  updateSettings(payload: Partial<AppSettings> & { llm_api_key?: string; clear_api_key?: boolean }): Promise<AppSettings> {
    return this.request('/api/settings', {
      method: 'PUT',
      body: JSON.stringify(payload),
    });
  }

  inspectGit(params: { taskId?: string; spaceId?: string; workerId?: string; workdir?: string }): Promise<GitInspectResult> {
    const sp = new URLSearchParams();
    if (params.taskId) sp.set('task_id', params.taskId);
    if (params.spaceId) sp.set('space_id', params.spaceId);
    if (params.workerId) sp.set('worker_id', params.workerId);
    if (params.workdir) sp.set('workdir', params.workdir);
    return this.request(`/api/git/inspect?${sp.toString()}`);
  }

  commitGit(req: {
    task_id?: string;
    space_id?: string;
    worker_id?: string;
    workdir?: string;
    message: string;
    stage_all?: boolean;
  }): Promise<GitCommitResult> {
    return this.request('/api/git/commit', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  cleanupWorktree(req: { task_id?: string; worker_id?: string; workdir?: string }): Promise<{ ok: boolean; error?: string }> {
    return this.request('/api/git/worktree/cleanup', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  deleteWorker(workerId: string): Promise<{ ok: boolean; id: string }> {
    return this.request(`/api/workers/${encodeURIComponent(workerId)}`, {
      method: 'DELETE',
    });
  }

  listRemoteFS(params: { workerId?: string; path: string; prefix?: string; dirsOnly?: boolean }): Promise<FSListResult> {
    const sp = new URLSearchParams();
    if (params.workerId) sp.set('worker_id', params.workerId);
    sp.set('path', params.path || '~');
    if (params.prefix) sp.set('prefix', params.prefix);
    if (params.dirsOnly === false) sp.set('dirs_only', 'false');
    return this.request(`/api/fs/list?${sp.toString()}`);
  }

  listSSHHosts(): Promise<{ hosts: SSHHost[] }> {
    return this.request('/api/ssh/hosts');
  }

  bootstrapSSH(req: {
    target: string;
    controller_url?: string;
    sandbox?: boolean;
    enable_mock?: boolean;
  }): Promise<SSHBootstrapResult> {
    return this.request('/api/ssh/bootstrap', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  createSession(req: DispatchTaskRequest): Promise<{
    task_id: string;
    worker_id?: string;
    agent?: string;
    model?: string;
    session_kind?: string;
    status: string;
  }> {
    return this.request('/api/sessions', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  getSessionDefaults(params?: { workerId?: string; agent?: string; model?: string }): Promise<SessionDefaults> {
    const sp = new URLSearchParams();
    if (params?.workerId) sp.set('worker_id', params.workerId);
    if (params?.agent) sp.set('agent', params.agent);
    if (params?.model) sp.set('model', params.model);
    const qs = sp.toString();
    return this.request(`/api/sessions/defaults${qs ? '?' + qs : ''}`);
  }

  getTaxonomy(): Promise<TaxonomySnapshot> {
    return this.request('/api/taxonomy');
  }

  createWorkspace(name: string): Promise<Workspace> {
    return this.request('/api/workspaces', {
      method: 'POST',
      body: JSON.stringify({ name }),
    });
  }

  deleteWorkspace(id: string): Promise<{ ok: boolean; id: string }> {
    return this.request(`/api/workspaces/${encodeURIComponent(id)}`, { method: 'DELETE' });
  }

  createProject(req: { workspace_id?: string; name: string }): Promise<Project> {
    return this.request('/api/projects', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  deleteProject(id: string): Promise<{ ok: boolean; id: string }> {
    return this.request(`/api/projects/${encodeURIComponent(id)}`, { method: 'DELETE' });
  }

  createRepo(req: { project_id?: string; worker_id: string; name?: string; path: string; remote_url?: string; branch?: string }): Promise<Repo> {
    return this.request('/api/repos', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  deleteRepo(id: string): Promise<{ ok: boolean; id: string }> {
    return this.request(`/api/repos/${encodeURIComponent(id)}`, { method: 'DELETE' });
  }

  createWorktree(req: { repo_id: string; branch?: string; path?: string; is_primary?: boolean }): Promise<Worktree> {
    return this.request('/api/worktrees', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  deleteWorktree(id: string): Promise<{ ok: boolean; id: string }> {
    return this.request(`/api/worktrees/${encodeURIComponent(id)}`, { method: 'DELETE' });
  }
}

async function readErrorMessage(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { error?: unknown } | null;
    if (body && typeof body.error === 'string' && body.error) return body.error;
  } catch {
    // non-JSON error body
  }
  return `HTTP ${res.status}`;
}

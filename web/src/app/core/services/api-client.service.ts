import { Injectable } from '@angular/core';
import {
  AppSettings,
  AuthMe,
  ChatMessage,
  ChatSession,
  DispatchTaskRequest,
  FleetSnapshotResponse,
  GitCommitResult,
  GitInspectResult,
  Space,
  TaskEvent,
} from '../models/konsert.models';

@Injectable({ providedIn: 'root' })
export class ApiClientService {
  private async request<T>(path: string, init?: RequestInit): Promise<T> {
    const res = await fetch(path, {
      credentials: 'same-origin',
      headers: {
        'Content-Type': 'application/json',
        ...(init?.headers ?? {}),
      },
      ...init,
    });
    if (!res.ok) {
      let errMsg = `HTTP ${res.status}`;
      try {
        const body = await res.json();
        if (body?.error) errMsg = body.error;
      } catch {
        // ignore parse errors
      }
      throw new Error(errMsg);
    }
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
}

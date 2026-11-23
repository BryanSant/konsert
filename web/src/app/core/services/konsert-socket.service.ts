import { Injectable, inject, signal } from '@angular/core';
import {
  ChatToolCard,
  Envelope,
  FRAME_HEADER_SIZE,
  FRAME_PTY_DATA,
  FRAME_PTY_SNAPSHOT,
  FRAME_PTY_STDIN,
  FRAME_TASK_ID_SIZE,
  Project,
  PtyFrameEvent,
  Repo,
  Space,
  Task,
  TaskEvent,
  UsageSummary,
  Worker,
  Workspace,
  Worktree,
} from '../models/konsert.models';
import { FleetStore } from '../state/fleet.store';
import { OrchestratorStore } from '../state/orchestrator.store';
import { TelemetryStore } from '../state/telemetry.store';
import { NotificationService } from './notification.service';

export type ConnectionState = 'connected' | 'connecting' | 'disconnected';
type PtyListener = (ev: PtyFrameEvent) => void;

@Injectable({ providedIn: 'root' })
export class KonsertSocketService {
  private readonly fleetStore = inject(FleetStore);
  private readonly telemetryStore = inject(TelemetryStore);
  private readonly orchestratorStore = inject(OrchestratorStore);
  private readonly notifications = inject(NotificationService);

  readonly state = signal<ConnectionState>('disconnected');

  private ws: WebSocket | null = null;
  private shouldReconnect = false;
  private reconnectAttempt = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;

  private readonly ptyListeners = new Map<string, Set<PtyListener>>();
  private readonly encoder = new TextEncoder();
  private readonly decoder = new TextDecoder();

  connect(): void {
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) {
      return;
    }
    this.shouldReconnect = true;
    this.openSocket();
  }

  disconnect(): void {
    this.shouldReconnect = false;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.ws) {
      this.ws.close(1000, 'client disconnect');
      this.ws = null;
    }
    this.state.set('disconnected');
  }

  private openSocket(): void {
    this.state.set('connecting');
    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const url = `${proto}//${window.location.host}/ws/client`;
    const ws = new WebSocket(url);
    ws.binaryType = 'arraybuffer';
    this.ws = ws;

    ws.onopen = () => {
      this.reconnectAttempt = 0;
      this.state.set('connected');
      void this.fleetStore.refresh();
      // Re-subscribe any open terminal panes after reconnect.
      for (const taskId of this.ptyListeners.keys()) {
        this.sendJSON('pty.subscribe', { task_id: taskId });
      }
    };

    ws.onmessage = (ev) => {
      if (ev.data instanceof ArrayBuffer) {
        this.handleBinaryFrame(new Uint8Array(ev.data));
      } else if (typeof ev.data === 'string') {
        this.handleTextFrame(ev.data);
      }
    };

    ws.onclose = () => {
      if (this.ws === ws) {
        this.ws = null;
      }
      this.state.set('disconnected');
      if (this.shouldReconnect) {
        const delay = Math.min(1000 * Math.pow(2, this.reconnectAttempt), 15000);
        this.reconnectAttempt++;
        this.reconnectTimer = setTimeout(() => this.openSocket(), delay);
      }
    };

    ws.onerror = () => {
      // onclose handles reconnect
    };
  }

  private handleTextFrame(raw: string): void {
    let env: Envelope;
    try {
      env = JSON.parse(raw) as Envelope;
    } catch {
      return;
    }

    switch (env.type) {
      case 'fleet.update': {
        const p = env.payload as {
          kind: string;
          worker?: Worker;
          task?: Task;
          event?: TaskEvent;
          space?: Space;
          workspace?: Workspace;
          project?: Project;
          repo?: Repo;
          worktree?: Worktree;
          deleted_id?: string;
        } | undefined;
        if (!p) return;
        if (p.kind === 'worker' && p.worker) {
          this.fleetStore.upsertWorker(p.worker);
        } else if (p.kind === 'task' && p.task) {
          const prev = this.fleetStore.tasksById()[p.task.id];
          const workerName = this.fleetStore.workersById()[p.task.worker_id]?.name || p.task.worker_id;
          this.fleetStore.upsertTask(p.task);
          this.notifications.notifyTaskTransition(prev?.status, p.task, workerName);
        } else if (p.kind === 'event' && p.event) {
          this.telemetryStore.appendEvent(p.event);
        } else if (p.kind === 'space' && p.space) {
          this.fleetStore.upsertSpace(p.space);
        } else if (p.kind === 'space_deleted' && p.deleted_id) {
          this.fleetStore.removeSpace(p.deleted_id);
        } else if (p.kind === 'worker_deleted' && p.deleted_id) {
          this.fleetStore.removeWorker(p.deleted_id);
        } else if (p.kind === 'workspace' && p.workspace) {
          this.fleetStore.upsertWorkspace(p.workspace);
        } else if (p.kind === 'workspace_deleted' && p.deleted_id) {
          this.fleetStore.removeWorkspace(p.deleted_id);
        } else if (p.kind === 'project' && p.project) {
          this.fleetStore.upsertProject(p.project);
        } else if (p.kind === 'project_deleted' && p.deleted_id) {
          this.fleetStore.removeProject(p.deleted_id);
        } else if (p.kind === 'repo' && p.repo) {
          this.fleetStore.upsertRepo(p.repo);
        } else if (p.kind === 'repo_deleted' && p.deleted_id) {
          this.fleetStore.removeRepo(p.deleted_id);
        } else if (p.kind === 'worktree' && p.worktree) {
          this.fleetStore.upsertWorktree(p.worktree);
        } else if (p.kind === 'worktree_deleted' && p.deleted_id) {
          this.fleetStore.removeWorktree(p.deleted_id);
        }
        break;
      }
      case 'chat.delta': {
        const p = env.payload as { session_id: string; delta: string };
        this.orchestratorStore.onChatDelta(p.session_id, p.delta);
        break;
      }
      case 'chat.tool': {
        const p = env.payload as ChatToolCard;
        this.orchestratorStore.onChatTool(p);
        break;
      }
      case 'chat.done': {
        const p = env.payload as { session_id: string };
        void this.orchestratorStore.onChatDone(p.session_id);
        break;
      }
      case 'chat.error': {
        const p = env.payload as { session_id: string; error: string };
        this.orchestratorStore.onChatError(p.session_id, p.error);
        break;
      }
      case 'usage.update': {
        const p = env.payload as UsageSummary;
        if (p) this.fleetStore.setUsage(p);
        break;
      }
    }
  }

  private handleBinaryFrame(raw: Uint8Array): void {
    if (raw.byteLength < FRAME_HEADER_SIZE) return;
    const frameType = raw[0];
    if (frameType !== FRAME_PTY_SNAPSHOT && frameType !== FRAME_PTY_DATA) return;

    const idSlice = raw.subarray(1, FRAME_HEADER_SIZE);
    let end = idSlice.length;
    while (end > 0 && idSlice[end - 1] === 0) {
      end--;
    }
    const taskId = this.decoder.decode(idSlice.subarray(0, end));
    const data = raw.subarray(FRAME_HEADER_SIZE);

    const listeners = this.ptyListeners.get(taskId);
    if (!listeners) return;
    const ev: PtyFrameEvent = { frameType, taskId, data };
    for (const fn of listeners) {
      fn(ev);
    }
  }

  subscribePty(taskId: string, listener: PtyListener): () => void {
    let set = this.ptyListeners.get(taskId);
    const isFirst = !set || set.size === 0;
    if (!set) {
      set = new Set<PtyListener>();
      this.ptyListeners.set(taskId, set);
    }
    set.add(listener);

    if (isFirst) {
      this.sendJSON('pty.subscribe', { task_id: taskId });
    }

    return () => {
      const current = this.ptyListeners.get(taskId);
      if (!current) return;
      current.delete(listener);
      if (current.size === 0) {
        this.ptyListeners.delete(taskId);
        this.sendJSON('pty.unsubscribe', { task_id: taskId });
      }
    };
  }

  sendPtyStdin(taskId: string, input: string): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    const idBytes = this.encoder.encode(taskId);
    if (idBytes.byteLength === 0 || idBytes.byteLength > FRAME_TASK_ID_SIZE) return;
    const dataBytes = this.encoder.encode(input);
    const frame = new Uint8Array(FRAME_HEADER_SIZE + dataBytes.byteLength);
    frame[0] = FRAME_PTY_STDIN;
    frame.set(idBytes, 1);
    frame.set(dataBytes, FRAME_HEADER_SIZE);
    this.ws.send(frame.buffer);
  }

  sendPtyResize(taskId: string, cols: number, rows: number): void {
    this.sendJSON('pty.resize', { task_id: taskId, cols, rows });
  }

  sendChatMessage(sessionId: string, content: string): void {
    this.orchestratorStore.appendLocalUserMessage(content);
    this.sendJSON('chat.send', { session_id: sessionId, content });
  }

  private sendJSON(type: string, payload: unknown): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    const env: Envelope = {
      type,
      ts: new Date().toISOString(),
      payload,
    };
    this.ws.send(JSON.stringify(env));
  }
}

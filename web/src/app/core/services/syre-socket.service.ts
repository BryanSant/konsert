import { PeopleStore, Person } from '../state/people.store';
import type { ChatScopeView, MachineGroup } from '../../shared/machines/machine-target';
import { Injectable, effect, inject, signal, untracked } from '@angular/core';
import {
  AuditEvent,
  AutopilotState,
  ChatSession,
  ChatToolCard,
  Envelope,
  FRAME_HEADER_SIZE,
  FRAME_PTY_DATA,
  FRAME_PTY_SNAPSHOT,
  FRAME_PTY_STDIN,
  FRAME_TASK_ID_SIZE,
  PolicyDirective,
  PolicyProposal,
  Project,
  PtyFrameEvent,
  Repo,
  Task,
  TaskEvent,
  Ticket,
  TicketEdge,
  UsageSummary,
  Worker,
} from '../models/syre.models';
import { AgentAccount } from '../models/agent-chat.models';
import { SessionActivity, ShellReminder } from '../models/activity.models';
import { Review } from '../models/review.models';
import { Device } from '../models/device.models';
import { DevicesStore } from '../state/devices.store';
import { ActivityStore } from '../state/activity.store';
import { AgentAccountsStore } from '../state/agent-accounts.store';
import { AgentChatStore } from '../state/agent-chat.store';
import { AuditStore } from '../state/audit.store';
import { QuestionView } from '../models/question.models';
import { QuestionsStore } from '../state/questions.store';
import { ControlStore } from '../state/control.store';
import { FleetEvent, FleetPause, Hold, OverrideRequest, UserRequest, Veto } from '../models/control.models';
import { CHAT_PROTOCOL } from './chat-protocol';
import { FleetStore } from '../state/fleet.store';
import { TicketStore } from '../state/ticket.store';
import { CommanderStore } from '../state/commander.store';
import { AuthService } from './auth.service';
import { NotificationService } from './notification.service';

export type ConnectionState = 'connected' | 'connecting' | 'disconnected';
type PtyListener = (ev: PtyFrameEvent) => void;

/** After this many consecutive failed handshakes, re-check the session via /api/auth/me. */
export const HANDSHAKE_FAILURES_BEFORE_AUTH_CHECK = 3;

/** Close code the hub uses when it signs a device out (revoked): 1008 policy violation. */
const SIGNED_OUT_CLOSE_CODE = 1008;

import { ObservedEventsPush, ObservedTranscriptStore } from '../state/observed-transcript.store';
@Injectable({ providedIn: 'root' })
export class SyreSocketService {
  private readonly fleetStore = inject(FleetStore);
  private readonly people = inject(PeopleStore);
  private readonly ticketStore = inject(TicketStore);
  private readonly commanderStore = inject(CommanderStore);
  private readonly auditStore = inject(AuditStore);
  private readonly controlStore = inject(ControlStore);
  private readonly questionsStore = inject(QuestionsStore);
  private readonly agentChat = inject(AgentChatStore);
  private readonly observed = inject(ObservedTranscriptStore);
  private readonly agentAccounts = inject(AgentAccountsStore);
  private readonly activity = inject(ActivityStore);
  private readonly notifications = inject(NotificationService);
  private readonly auth = inject(AuthService);
  private readonly devices = inject(DevicesStore);

  readonly state = signal<ConnectionState>('disconnected');

  private ws: WebSocket | null = null;
  private shouldReconnect = false;
  private reconnectAttempt = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  /** True once any socket in this connect() lifetime has opened (distinguishes reconnects). */
  private hasConnected = false;
  /** Consecutive sockets that closed without ever opening (e.g. 401 on upgrade). */
  private failedHandshakes = 0;

  /** Chat session whose events this client wants (the active commander session). */
  private desiredChatSession = '';
  /** Other chats shown somewhere (Command tabs, Wall tiles), reference-counted. */
  private readonly chatWatchers = new Map<string, number>();
  /** Chat sessions subscribed on the *current* socket. */
  private readonly subscribedChats = new Set<string>();

  constructor() {
    // The hub only fans chat.* events out to clients subscribed to that session, so
    // follow the active session: unsubscribe from the old one, subscribe to the new.
    effect(() => {
      const sid = this.commanderStore.activeSessionId();
      untracked(() => this.setChatSubscription(sid));
    });
  }

  /** Subscribes this socket to chat events for `sessionId` ('' to unsubscribe). */
  setChatSubscription(sessionId: string): void {
    this.desiredChatSession = sessionId;
    this.syncChatSubscriptions();
  }

  /**
   * Receives a chat's events while it is shown outside the main chat view (a tab, a wall
   * tile). Returns the function that stops watching; the last watcher unsubscribes.
   */
  watchChat(sessionId: string): () => void {
    if (!sessionId) return () => undefined;
    this.chatWatchers.set(sessionId, (this.chatWatchers.get(sessionId) ?? 0) + 1);
    this.syncChatSubscriptions();
    let done = false;
    return () => {
      if (done) return;
      done = true;
      const n = (this.chatWatchers.get(sessionId) ?? 1) - 1;
      if (n <= 0) this.chatWatchers.delete(sessionId);
      else this.chatWatchers.set(sessionId, n);
      this.syncChatSubscriptions();
    };
  }

  private syncChatSubscriptions(): void {
    const want = new Set<string>(this.chatWatchers.keys());
    if (this.desiredChatSession) want.add(this.desiredChatSession);
    for (const id of [...this.subscribedChats]) {
      if (!want.has(id)) {
        this.sendJSON('chat.unsubscribe', { session_id: id });
        this.subscribedChats.delete(id);
      }
    }
    // The active session first, so the main view's frames go out before the watchers'.
    const order = [this.desiredChatSession, ...this.chatWatchers.keys()].filter((id) => id && want.has(id));
    for (const id of new Set(order)) {
      if (!this.subscribedChats.has(id) && this.sendJSON('chat.subscribe', { session_id: id })) {
        this.subscribedChats.add(id);
      }
    }
  }

  private readonly ptyListeners = new Map<string, Set<PtyListener>>();
  private readonly encoder = new TextEncoder();
  private readonly decoder = new TextDecoder();

  connect(): void {
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) {
      return;
    }
    this.shouldReconnect = true;
    this.hasConnected = false;
    this.failedHandshakes = 0;
    this.openSocket();
  }

  disconnect(): void {
    this.shouldReconnect = false;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.ws) {
      const ws = this.ws;
      this.ws = null;
      ws.close(1000, 'client disconnect');
    }
    this.state.set('disconnected');
    this.commanderStore.onConnectionLost();
  }

  private openSocket(): void {
    this.state.set('connecting');
    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const url = `${proto}//${window.location.host}/ws/client`;
    const ws = new WebSocket(url);
    ws.binaryType = 'arraybuffer';
    this.ws = ws;
    let opened = false;

    ws.onopen = () => {
      opened = true;
      this.failedHandshakes = 0;
      const isReconnect = this.hasConnected;
      this.hasConnected = true;
      this.reconnectAttempt = 0;
      this.state.set('connected');
      void this.fleetStore.refresh();
      void this.agentAccounts.load();
      if (isReconnect) {
        void this.commanderStore.onReconnected();
        void this.agentChat.onReconnected();
        for (const id of this.chatWatchers.keys()) void this.agentChat.catchUp(id);
        void this.controlStore.load();
        void this.questionsStore.load();
      }
      this.reportPresence();
      // A new socket starts with no subscriptions: re-subscribe the active chat session.
      this.subscribedChats.clear();
      this.syncChatSubscriptions();
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

    ws.onclose = (ev?: CloseEvent) => {
      if (this.ws !== ws) return; // superseded by disconnect()/a newer socket
      this.ws = null;
      this.state.set('disconnected');
      this.commanderStore.onConnectionLost();
      if (opened && ev?.code === SIGNED_OUT_CLOSE_CODE) {
        // The hub closed the socket because this device was revoked: confirm and sign out now.
        void this.auth.verifySession().then((valid) => {
          if (!valid) this.disconnect();
        });
      } else if (!opened) {
        this.failedHandshakes++;
        // The browser hides the upgrade's HTTP status, so a 401 looks like any other
        // failure. Periodically ask the REST API whether the session is still valid.
        if (this.failedHandshakes % HANDSHAKE_FAILURES_BEFORE_AUTH_CHECK === 0) {
          void this.auth.verifySession().then((valid) => {
            if (!valid) this.disconnect();
          });
        }
      }
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
          project?: Project;
          repo?: Repo;
          audit?: AuditEvent;
          deleted_id?: string;
          worker_id?: string;
          accounts?: AgentAccount[];
          chat_session?: ChatSession;
          fleet_event?: FleetEvent;
          hold?: Hold;
          request?: UserRequest;
          override?: OverrideRequest;
          veto?: Veto;
          pause?: FleetPause;
          question?: QuestionView;
          activity?: SessionActivity;
          review?: Review;
          device?: Device;
          shell_reminder?: ShellReminder;
          autopilot?: AutopilotState;
          directive?: PolicyDirective;
          policy_proposal?: PolicyProposal;
          ticket?: Ticket;
          ticket_edge?: TicketEdge;
          labels?: string[];
          machine_groups?: MachineGroup[];
          chat_scope?: ChatScopeView;
          person?: Person;
        } | undefined;
        if (!p) return;
        if (p.kind === 'worker' && p.worker) {
          this.fleetStore.upsertWorker(p.worker);
        } else if (p.kind === 'task' && p.task) {
          const prev = this.fleetStore.tasksById()[p.task.id];
          const worker = this.fleetStore.workersById()[p.task.worker_id];
          const workerName = worker?.name || worker?.sid || p.task.worker_id;
          this.fleetStore.upsertTask(p.task);
          this.ticketStore.applyTaskDigest(p.task);
          this.notifications.notifyTaskTransition(prev?.status, p.task, workerName);
        } else if (p.kind === CHAT_PROTOCOL.fleetAccountsKind && p.worker_id) {
          this.agentAccounts.applyWorkerAccounts(p.worker_id, p.accounts ?? []);
        } else if (p.kind === CHAT_PROTOCOL.fleetChatSessionKind && p.chat_session?.id) {
          this.agentChat.onSessionUpsert(p.chat_session);
        } else if (p.kind === CHAT_PROTOCOL.fleetChatSessionDeletedKind && p.deleted_id) {
          void this.agentChat.onSessionDeleted(p.deleted_id);
          this.activity.forget(`chat:${p.deleted_id}`);
        } else if (p.kind === 'activity' && p.activity) {
          this.activity.apply(p.activity);
        } else if (p.kind === 'review' && p.review) {
          this.fleetStore.upsertReview(p.review);
        } else if (p.kind === 'device' && p.device) {
          this.devices.apply(p.device);
        } else if (p.kind === 'device_revoked' && p.deleted_id) {
          this.devices.remove(p.deleted_id);
        } else if (p.kind === 'audit' && p.audit) {
          this.auditStore.publish(p.audit);
        } else if (p.kind === 'fleet_event' && p.fleet_event) {
          this.controlStore.applyEvent(p.fleet_event);
        } else if (p.kind === 'hold' && p.hold) {
          this.controlStore.applyHold(p.hold);
        } else if (p.kind === 'request' && p.request) {
          this.controlStore.applyRequest(p.request);
        } else if (p.kind === 'override' && p.override) {
          this.controlStore.applyOverride(p.override);
        } else if (p.kind === 'shell_reminder' && p.shell_reminder) {
          this.controlStore.applyShellReminder(p.shell_reminder);
        } else if (p.kind === 'veto' && p.veto) {
          this.controlStore.applyVeto(p.veto);
        } else if (p.kind === 'pause' && p.pause) {
          this.controlStore.applyPause(p.pause);
        } else if (p.kind === 'autopilot' && p.autopilot) {
          this.fleetStore.applyAutopilot(p.autopilot);
        } else if (p.kind === 'directive' && p.directive) {
          this.fleetStore.applyDirective(p.directive);
        } else if (p.kind === 'machine_labels' && p.worker_id) {
          this.fleetStore.applyMachineLabels(p.worker_id, p.labels ?? []);
        } else if (p.kind === 'person' && p.person) {
          this.people.apply(p.person);
        } else if (p.kind === 'machine_groups') {
          this.fleetStore.applyMachineGroups(p.machine_groups ?? []);
        } else if (p.kind === 'chat_scope' && p.chat_scope) {
          this.agentChat.applyScope(p.chat_scope);
        } else if (p.kind === 'policy_proposal' && p.policy_proposal) {
          this.controlStore.applyProposal(p.policy_proposal);
        } else if (p.kind === 'question' && p.question) {
          this.questionsStore.apply(p.question);
        } else if (p.kind === 'worker_deleted' && p.deleted_id) {
          this.fleetStore.removeWorker(p.deleted_id);
          this.agentAccounts.removeWorker(p.deleted_id);
        } else if (p.kind === 'project' && p.project) {
          this.fleetStore.upsertProject(p.project);
        } else if (p.kind === 'project_deleted' && p.deleted_id) {
          this.fleetStore.removeProject(p.deleted_id);
        } else if (p.kind === 'repo' && p.repo) {
          this.fleetStore.upsertRepo(p.repo);
        } else if (p.kind === 'repo_deleted' && p.deleted_id) {
          this.fleetStore.removeRepo(p.deleted_id);
        } else if (p.kind === 'ticket' && p.ticket) {
          this.ticketStore.upsert(p.ticket);
        } else if (p.kind === 'ticket_deleted' && p.deleted_id) {
          this.ticketStore.remove(p.deleted_id);
        } else if (p.kind === 'ticket_edge' && p.ticket_edge) {
          this.ticketStore.upsertEdge(p.ticket_edge);
        } else if (p.kind === 'ticket_edge_deleted' && p.ticket_edge) {
          this.ticketStore.dropEdge(p.ticket_edge.ticket_id, p.ticket_edge.blocked_by_id);
        }
        break;
      }
      case 'chat.delta': {
        const p = env.payload as { session_id: string; delta: string };
        this.commanderStore.onChatDelta(p.session_id, p.delta);
        break;
      }
      case 'chat.tool': {
        const p = env.payload as ChatToolCard;
        this.commanderStore.onChatTool(p);
        break;
      }
      case 'chat.done': {
        const p = env.payload as { session_id: string };
        void this.commanderStore.onChatDone(p.session_id);
        break;
      }
      case 'chat.error': {
        const p = env.payload as { session_id: string; error: string };
        this.commanderStore.onChatError(p.session_id, p.error);
        break;
      }
      case CHAT_PROTOCOL.wsEvent: {
        this.agentChat.onEvent(env.payload);
        break;
      }
      case 'observed.events': {
        this.observed.onEvents(env.payload as ObservedEventsPush);
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

  get isOpen(): boolean {
    return !!this.ws && this.ws.readyState === WebSocket.OPEN;
  }

  /**
   * Sends a chat message. Returns false (and records an error on the store) when the
   * socket is not open, so the caller can keep the draft instead of silently losing it.
   */
  sendChatMessage(sessionId: string, content: string): boolean {
    if (!this.isOpen) {
      this.commanderStore.rejectSend();
      return false;
    }
    this.commanderStore.beginSend(sessionId, content);
    if (!this.sendJSON('chat.send', { session_id: sessionId, content })) {
      this.commanderStore.abortSend(sessionId);
      return false;
    }
    return true;
  }

  /** Writes one JSON frame (used by ChatTransportService); false when not delivered. */
  private presenceHooked = false;

  /**
   * Tells the hub whether this page is visible and focused: such a device shows alerts
   * itself and gets no Web Push (SYR-Z15J6). Sent on connect and when either changes.
   */
  private reportPresence(): void {
    if (typeof document === 'undefined') return;
    const send = () => this.sendFrame('presence', { visible: !document.hidden, focused: document.hasFocus() });
    if (!this.presenceHooked) {
      this.presenceHooked = true;
      document.addEventListener('visibilitychange', send);
      window.addEventListener('focus', send);
      window.addEventListener('blur', send);
    }
    send();
  }

  sendFrame(type: string, payload: unknown): boolean {
    return this.sendJSON(type, payload);
  }

  /** Writes a JSON envelope; returns false if the socket is not open or the write threw. */
  private sendJSON(type: string, payload: unknown): boolean {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return false;
    const env: Envelope = {
      type,
      ts: new Date().toISOString(),
      payload,
    };
    try {
      this.ws.send(JSON.stringify(env));
      return true;
    } catch {
      return false;
    }
  }
}

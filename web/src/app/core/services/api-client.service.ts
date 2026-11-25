import { Injectable } from '@angular/core';
import {
  AppSettings,
  AuditListResponse,
  AuditQuery,
  AuthMe,
  AutopilotProjectState,
  AutopilotState,
  ChatMessage,
  ChatSession,
  CompactionRun,
  RetentionDays,
  StorageResponse,
  Task,
  DispatchTaskRequest,
  FSListResult,
  FleetSnapshotResponse,
  GitCommitResult,
  GitInspectResult,
  Project,
  Repo,
  SSHBootstrapResult,
  SetupStatus,
  AddLocalWorkerResult,
  JoinCode,
  JoinCodeStatus,
  MachineCredentialInfo,
  WorkerPorts,
  RotationResponse,
  SSHHost,
  SessionDefaults,
  TaskEvent,
  Ticket,
  TicketEdge,
  TicketRepoBinding,
  TicketRollup,
  WakeLockStatus,
  Worker,
  AutonomyPolicy,
} from '../models/syre.models';
import { HeatHistory, ShellPolicySettings, ShellReminder } from '../models/activity.models';
import { DevicesResponse, Device, LoginRequest, McpCredential, PairResponse, PairingCode } from '../models/device.models';
import { Acceptance, DeliveryPlan, Review, ReviewDecision, ReviewResult } from '../models/review.models';
import {
  ForgeDetectResult,
  ForgeDevicePoll,
  ForgeDeviceStart,
  ForgeHost,
  ForgeKind,
  ForgeTokenSaveResult,
} from '../models/forge.models';
import { AgentChatCreateRequest, AgentChatEvent, AgentChatPatch } from '../models/agent-chat.models';
import {
  ChatActionResult,
  FleetEventQuery,
  FleetEventsResponse,
  FleetPause,
  FleetPauseResult,
  Hold,
  OverrideRequest,
  TaskActionResult,
  UserRequest,
  Veto,
  VetoReason,
  WorkerControlResult,
} from '../models/control.models';
import {
  QuestionAnswerRequest,
  QuestionMetrics,
  QuestionSettings,
  QuestionView,
} from '../models/question.models';
import { CHAT_PROTOCOL } from './chat-protocol';

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
export const SYRE_REQUEST_HEADER = 'X-Syre-Request';

/** Paths whose 401 means "bad credentials" rather than "session expired". */
const AUTH_EXEMPT_PATHS = ['/api/auth/login', '/api/auth/pair'];

export interface SSHBootstrapRequest {
  target: string;
  sandbox?: boolean;
  enable_mock?: boolean;
  /** SHA256 fingerprint the operator confirmed after a host_key_confirmation result. */
  accept_host_key?: string;
}

/** Frames streamed by POST /api/ssh/bootstrap?stream=1. Unknown types are passed through. */
export type SSHBootstrapEvent =
  | { type: 'stage'; stage?: string; label?: string; detail?: string }
  | { type: 'done'; result: SSHBootstrapResult }
  | { type: string; [key: string]: unknown };

export interface SSHBootstrapOptions {
  /** Aborts the request (closing the stream) when signalled, e.g. on modal close. */
  signal?: AbortSignal;
  /** Receives every streamed frame in order. */
  onEvent?: (ev: SSHBootstrapEvent) => void;
}

/**
 * Parses a `text/event-stream` body, yielding the JSON payload of each `data:` event.
 * Multi-line data fields are joined per the SSE spec; malformed JSON is skipped.
 */
export async function* readServerSentEvents<T>(body: ReadableStream<Uint8Array>): AsyncGenerator<T> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  const parse = (block: string): T | undefined => {
    const data = block
      .split('\n')
      .filter((l) => l.startsWith('data:'))
      .map((l) => l.slice(5).replace(/^ /, ''))
      .join('\n');
    if (!data) return undefined;
    try {
      return JSON.parse(data) as T;
    } catch {
      return undefined;
    }
  };
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true }).replace(/\r\n/g, '\n');
      let sep: number;
      while ((sep = buf.indexOf('\n\n')) !== -1) {
        const ev = parse(buf.slice(0, sep));
        buf = buf.slice(sep + 2);
        if (ev !== undefined) yield ev;
      }
    }
    const tail = parse((buf + decoder.decode()).trim());
    if (tail !== undefined) yield tail;
  } finally {
    reader.releaseLock();
  }
}

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
  private async send(
    path: string,
    init: RequestInit = {},
    opts: { json?: boolean; acceptStatuses?: number[] } = {},
  ): Promise<Response> {
    const method = (init.method ?? 'GET').toUpperCase();
    const headers = new Headers(init.headers);
    headers.set(SYRE_REQUEST_HEADER, '1');
    let body = init.body;
    if (opts.json !== false) {
      if (!headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
      if (body == null && (method === 'POST' || method === 'PUT' || method === 'PATCH')) {
        body = '{}';
      }
    }
    const res = await fetch(path, { ...init, method, headers, body, credentials: 'same-origin' });
    if (!res.ok && !opts.acceptStatuses?.includes(res.status)) {
      if (res.status === 401 && !AUTH_EXEMPT_PATHS.includes(path.split('?')[0])) {
        this.unauthorizedHandler?.();
      }
      throw new ApiError(friendlyErrorMessage(res, await readErrorMessage(res)), res.status);
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

  /** The login page's "Use recovery key" fallback: signs this browser in as a new device. */
  login(token: string, name = ''): Promise<PairResponse> {
    return this.request('/api/auth/login', {
      method: 'POST',
      body: JSON.stringify({ token, name }),
    });
  }

  /** Signing out revokes this browser's device credential. */
  logout(): Promise<{ ok: boolean }> {
    return this.request('/api/auth/logout', { method: 'POST' });
  }

  /** Redeems a pairing code (/pair/{code}); the hub sets the session cookie. */
  pair(code: string, name: string): Promise<PairResponse> {
    return this.request('/api/auth/pair', { method: 'POST', body: JSON.stringify({ code, name }) });
  }

  listDevices(): Promise<DevicesResponse> {
    return this.request<DevicesResponse>('/api/devices');
  }

  renameDevice(id: string, name: string): Promise<Device> {
    return this.request<Device>(`/api/devices/${encodeURIComponent(id)}`, { method: 'PATCH', body: JSON.stringify({ name }) });
  }

  revokeDevice(id: string): Promise<{ ok: boolean }> {
    return this.request(`/api/devices/${encodeURIComponent(id)}`, { method: 'DELETE' });
  }

  createPairingCode(): Promise<PairingCode> {
    return this.request<PairingCode>('/api/pairing-codes', { method: 'POST' });
  }

  createMcpCredential(name: string): Promise<McpCredential> {
    return this.request<McpCredential>('/api/devices/mcp', { method: 'POST', body: JSON.stringify({ name }) });
  }

  getLoginRequest(code: string): Promise<LoginRequest> {
    return this.request<LoginRequest>(`/api/auth/device-code/${encodeURIComponent(code)}`);
  }

  decideLoginRequest(code: string, approve: boolean): Promise<{ ok: boolean }> {
    return this.request(`/api/auth/device-code/${encodeURIComponent(code)}/${approve ? 'approve' : 'deny'}`, { method: 'POST' });
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

  /** Approve or request changes on finished work; the hub also reviews the PR with gh. */
  reviewTask(taskId: string, decision: ReviewDecision, note?: string): Promise<ReviewResult> {
    return this.request(`/api/tasks/${encodeURIComponent(taskId)}/review`, {
      method: 'POST',
      body: JSON.stringify({ decision, note: note || undefined }),
    });
  }

  /** Pins a task or chat ("Keep") so retention never compacts it, or unpins it. */
  setKeep(kind: 'task', id: string, keep: boolean): Promise<{ task: Task }>;
  setKeep(kind: 'chat', id: string, keep: boolean): Promise<{ chat: ChatSession }>;
  setKeep(kind: 'task' | 'chat', id: string, keep: boolean): Promise<{ task?: Task; chat?: ChatSession }> {
    return this.request(`/api/${kind === 'task' ? 'tasks' : 'chats'}/${encodeURIComponent(id)}/keep`, {
      method: 'PUT',
      body: JSON.stringify({ keep }),
    });
  }

  /** Settings → Storage: size, breakdown, retention and the last compaction pass. */
  getStorage(fresh = false): Promise<StorageResponse> {
    return this.request(`/api/storage${fresh ? '?fresh=1' : ''}`);
  }

  setRetention(r: RetentionDays): Promise<StorageResponse> {
    return this.request('/api/storage/retention', { method: 'PUT', body: JSON.stringify(r) });
  }

  /** Starts a full compaction pass in the background (Compact now). */
  compactNow(): Promise<{ compaction: CompactionRun }> {
    return this.request('/api/storage/compact', { method: 'POST' });
  }

  getCompaction(): Promise<{ compaction: CompactionRun }> {
    return this.request('/api/storage/compact');
  }

  /** Puts a task or chat in (or takes it out of) the Expedite lane. */
  setExpedite(kind: 'task' | 'chat', id: string, expedite: boolean): Promise<{ review: Review }> {
    return this.request(`/api/${kind === 'task' ? 'tasks' : 'chats'}/${encodeURIComponent(id)}/expedite`, {
      method: 'PUT',
      body: JSON.stringify({ expedite }),
    });
  }

  /**
   * Fleet shell cleanup: kill shells of a task or chat (syred terminates each one's
   * process tree), or ask its agent to stop them. No shells listed: all of them.
   */
  shellAction(
    kind: 'task' | 'chat',
    id: string,
    action: 'kill' | 'ask',
    shells: Array<{ pid: number; started_at: string }> = [],
  ): Promise<{ ok: boolean; shells: number; delivery?: 'now' | 'after_turn' }> {
    return this.request(`/api/${kind === 'task' ? 'tasks' : 'chats'}/${encodeURIComponent(id)}/shells/${action}`, {
      method: 'POST',
      body: JSON.stringify(shells.length ? { shells } : {}),
    });
  }

  /** Keep running: snoozes the "shells left running" reminder of a task or chat. */
  keepShells(kind: 'task' | 'chat', id: string): Promise<{ ok: boolean }> {
    return this.request(`/api/${kind === 'task' ? 'tasks' : 'chats'}/${encodeURIComponent(id)}/shells/keep`, { method: 'POST', body: '{}' });
  }

  /** Open "shells left running" reminders (SYR-DCEZV). */
  listShellReminders(): Promise<{ reminders: ShellReminder[] }> {
    return this.request('/api/shells/reminders');
  }

  /** What Syre does about shells left running: global mode, idle period, per-project modes. */
  getShellPolicy(): Promise<ShellPolicySettings> {
    return this.request('/api/settings/shells');
  }

  setShellPolicy(p: ShellPolicySettings): Promise<ShellPolicySettings> {
    return this.request('/api/settings/shells', { method: 'PUT', body: JSON.stringify(p) });
  }

  /** The task's acceptance criteria: its ka ticket's items, else its prompt's bullets. */
  getTaskAcceptance(taskId: string): Promise<Acceptance> {
    return this.request(`/api/tasks/${encodeURIComponent(taskId)}/acceptance`);
  }

  /** What Approve will do for this task (mode, merge style, PR/fork info, approve label). */
  getTaskDelivery(taskId: string): Promise<DeliveryPlan> {
    return this.request(`/api/tasks/${encodeURIComponent(taskId)}/delivery`);
  }

  steerTask(taskId: string, inputText: string, appendNewline = true): Promise<{ ok: boolean }> {
    return this.request(`/api/tasks/${encodeURIComponent(taskId)}/steer`, {
      method: 'POST',
      body: JSON.stringify({ input_text: inputText, append_newline: appendNewline }),
    });
  }

  /** Fleet-wide host wake lock wish plus each worker's state. */
  getWakeLock(): Promise<WakeLockStatus> {
    return this.request<WakeLockStatus>('/api/wakelock');
  }

  /** Applies a host wake lock to every host (and hosts that connect later). */
  setFleetWakeLock(enabled: boolean, reason?: string): Promise<{ ok: boolean; sent_to: string[]; status: WakeLockStatus }> {
    return this.request('/api/wakelock', {
      method: 'POST',
      body: JSON.stringify({ enabled, reason }),
    });
  }

  /** Applies a host wake lock to one host, overriding the fleet-wide wish. */
  setWorkerWakeLock(workerId: string, enabled: boolean, reason?: string): Promise<{ ok: boolean; sent: boolean }> {
    return this.request(`/api/workers/${encodeURIComponent(workerId)}/wakelock`, {
      method: 'POST',
      body: JSON.stringify({ enabled, reason }),
    });
  }

  /**
   * Reports the Take Control (Interactive) toggle so the hub can audit who took or
   * released interactive control of a task.
   */
  setTaskControl(taskId: string, enabled: boolean): Promise<{ ok: boolean; hold_id?: string }> {
    return this.request(`/api/tasks/${encodeURIComponent(taskId)}/control`, {
      method: 'POST',
      body: JSON.stringify({ enabled }),
    });
  }

  /** Fleet-wide operator audit log, newest first. */
  listAudit(q: AuditQuery = {}): Promise<AuditListResponse> {
    const params = new URLSearchParams();
    for (const [k, v] of Object.entries(q)) {
      if (v !== undefined && v !== null && v !== '' && v !== 0) params.set(k, String(v));
    }
    const qs = params.toString();
    return this.request<AuditListResponse>(`/api/audit${qs ? `?${qs}` : ''}`);
  }

  cancelTask(taskId: string): Promise<{ ok: boolean }> {
    return this.request(`/api/tasks/${encodeURIComponent(taskId)}/cancel`, {
      method: 'POST',
    });
  }

  // --- Human control (PRD §5.8a) ---

  suspendTask(taskId: string): Promise<TaskActionResult> {
    return this.request(`/api/tasks/${encodeURIComponent(taskId)}/suspend`, { method: 'POST' });
  }

  resumeTask(taskId: string): Promise<TaskActionResult> {
    return this.request(`/api/tasks/${encodeURIComponent(taskId)}/resume`, { method: 'POST' });
  }

  /** Cancels the task and records why, so the commander does not simply retry it. */
  vetoTask(taskId: string, reason: VetoReason, note?: string): Promise<TaskActionResult> {
    return this.request(`/api/tasks/${encodeURIComponent(taskId)}/veto`, {
      method: 'POST',
      body: JSON.stringify({ reason, note: note || undefined }),
    });
  }

  suspendChat(chatId: string): Promise<ChatActionResult> {
    return this.request(`/api/chats/${encodeURIComponent(chatId)}/suspend`, { method: 'POST' });
  }

  resumeChat(chatId: string): Promise<ChatActionResult> {
    return this.request(`/api/chats/${encodeURIComponent(chatId)}/resume`, { method: 'POST' });
  }

  stopChatTurn(chatId: string): Promise<ChatActionResult> {
    return this.request(`/api/chats/${encodeURIComponent(chatId)}/stop`, { method: 'POST' });
  }

  vetoChat(chatId: string, reason: VetoReason, note?: string): Promise<ChatActionResult> {
    return this.request(`/api/chats/${encodeURIComponent(chatId)}/veto`, {
      method: 'POST',
      body: JSON.stringify({ reason, note: note || undefined }),
    });
  }

  setWorkerDrained(workerId: string, drained: boolean): Promise<WorkerControlResult> {
    return this.request(`/api/workers/${encodeURIComponent(workerId)}/drain`, {
      method: 'POST',
      body: JSON.stringify({ drained }),
    });
  }

  /** Bulk machine control: suspend-all, resume-all or stop-all. */
  workerBulk(workerId: string, action: 'suspend-all' | 'resume-all' | 'stop-all'): Promise<WorkerControlResult> {
    return this.request(`/api/workers/${encodeURIComponent(workerId)}/${action}`, { method: 'POST' });
  }

  /** Per-machine heat history over a range, downsampled to `points` buckets (oldest first). */
  getHeatHistory(range: string, points = 48): Promise<HeatHistory> {
    return this.request<HeatHistory>(`/api/fleet/heat?range=${encodeURIComponent(range)}&points=${points}`);
  }

  getFleetPause(): Promise<FleetPause> {
    return this.request('/api/fleet/pause');
  }

  pauseFleet(): Promise<FleetPauseResult> {
    return this.request('/api/fleet/pause', { method: 'POST' });
  }

  resumeFleet(): Promise<FleetPauseResult> {
    return this.request('/api/fleet/resume', { method: 'POST' });
  }

  getAutopilot(): Promise<AutopilotState> {
    return this.request<AutopilotState>('/api/autopilot');
  }

  getProjectAutopilot(projectId: string): Promise<AutopilotProjectState> {
    return this.request<AutopilotProjectState>(`/api/projects/${encodeURIComponent(projectId)}/autopilot`);
  }

  setProjectAutopilot(projectId: string, play: boolean): Promise<AutopilotProjectState> {
    return this.request<AutopilotProjectState>(`/api/projects/${encodeURIComponent(projectId)}/autopilot`, {
      method: 'POST',
      body: JSON.stringify({ play }),
    });
  }

  listHolds(q: { active?: boolean; worker_id?: string } = {}): Promise<{ holds: Hold[] }> {
    const params = new URLSearchParams();
    if (q.active) params.set('active', '1');
    if (q.worker_id) params.set('worker_id', q.worker_id);
    const qs = params.toString();
    return this.request(`/api/holds${qs ? `?${qs}` : ''}`);
  }

  /** Places a hold the user asked for: a session (task or chat) or a whole machine. */
  createHold(target: { task_id?: string; chat_id?: string; worker_id?: string; note?: string }): Promise<Hold> {
    return this.request('/api/holds', { method: 'POST', body: JSON.stringify(target) });
  }

  releaseHold(holdId: string, note?: string): Promise<Hold> {
    return this.request(`/api/holds/${encodeURIComponent(holdId)}/release`, {
      method: 'POST',
      body: JSON.stringify({ note: note || undefined }),
    });
  }

  listRequests(q: { status?: string; worker_id?: string } = {}): Promise<{ requests: UserRequest[] }> {
    const params = new URLSearchParams();
    if (q.status) params.set('status', q.status);
    if (q.worker_id) params.set('worker_id', q.worker_id);
    const qs = params.toString();
    return this.request(`/api/requests${qs ? `?${qs}` : ''}`);
  }

  resolveRequest(requestId: string, status: 'done' | 'declined', note?: string): Promise<UserRequest> {
    return this.request(`/api/requests/${encodeURIComponent(requestId)}/resolve`, {
      method: 'POST',
      body: JSON.stringify({ status, note: note || undefined }),
    });
  }

  listOverrides(q: { status?: string } = {}): Promise<{ overrides: OverrideRequest[] }> {
    const qs = q.status ? `?status=${encodeURIComponent(q.status)}` : '';
    return this.request(`/api/overrides${qs}`);
  }

  decideOverride(id: string, decision: 'approve' | 'deny', note?: string): Promise<OverrideRequest> {
    return this.request(`/api/overrides/${encodeURIComponent(id)}/${decision}`, {
      method: 'POST',
      body: JSON.stringify({ note: note || undefined }),
    });
  }

  listVetoes(q: { open?: boolean } = {}): Promise<{ vetoes: Veto[]; ack_minutes?: number }> {
    return this.request(`/api/vetoes${q.open ? '?open=1' : ''}`);
  }

  dismissVeto(id: string): Promise<Veto> {
    return this.request(`/api/vetoes/${encodeURIComponent(id)}/dismiss`, { method: 'POST' });
  }

  // --- Questions (PRD §5.8b) ---

  /** The fleet-wide question queue (pending, sorted by blocking impact); `all` adds recent closed ones. */
  listQuestions(q: { all?: boolean; chat_id?: string } = {}): Promise<{ questions: QuestionView[] }> {
    const params = new URLSearchParams();
    if (q.all) params.set('status', 'all');
    if (q.chat_id) params.set('chat_id', q.chat_id);
    const qs = params.toString();
    return this.request(`/api/questions${qs ? `?${qs}` : ''}`);
  }

  getQuestion(id: string): Promise<QuestionView> {
    return this.request(`/api/questions/${encodeURIComponent(id)}`);
  }

  answerQuestion(id: string, body: QuestionAnswerRequest): Promise<QuestionView> {
    return this.request(`/api/questions/${encodeURIComponent(id)}/answer`, { method: 'POST', body: JSON.stringify(body) });
  }

  /** "Ask about this": a message in the question's side conversation. */
  askAboutQuestion(id: string, text: string): Promise<QuestionView> {
    return this.request(`/api/questions/${encodeURIComponent(id)}/ask`, { method: 'POST', body: JSON.stringify({ text }) });
  }

  delegateQuestion(id: string): Promise<QuestionView> {
    return this.request(`/api/questions/${encodeURIComponent(id)}/delegate`, { method: 'POST', body: '{}' });
  }

  snoozeQuestion(id: string, minutes: number): Promise<QuestionView> {
    return this.request(`/api/questions/${encodeURIComponent(id)}/snooze`, { method: 'POST', body: JSON.stringify({ minutes }) });
  }

  questionMetrics(days = 7): Promise<QuestionMetrics> {
    return this.request(`/api/questions/metrics?days=${days}`);
  }

  getQuestionSettings(): Promise<QuestionSettings> {
    return this.request('/api/questions/settings');
  }

  putQuestionSettings(s: QuestionSettings): Promise<QuestionSettings> {
    return this.request('/api/questions/settings', { method: 'PUT', body: JSON.stringify(s) });
  }

  /** Fleet timeline (attributed events), newest first. */
  listFleetEvents(q: FleetEventQuery = {}): Promise<FleetEventsResponse> {
    const params = new URLSearchParams();
    for (const [k, v] of Object.entries(q)) {
      if (v === undefined || v === null || v === '' || v === 0 || v === false) continue;
      params.set(k, v === true ? '1' : String(v));
    }
    const qs = params.toString();
    return this.request<FleetEventsResponse>(`/api/events${qs ? `?${qs}` : ''}`);
  }

  getTaskEvents(taskId: string, afterId = 0): Promise<{ events: TaskEvent[] }> {
    return this.request(`/api/tasks/${encodeURIComponent(taskId)}/events?after_id=${afterId}`);
  }

  listChatSessions(): Promise<{ sessions: ChatSession[] }> {
    return this.request('/api/chat/sessions');
  }

  createChatSession(title = 'Commander Session'): Promise<ChatSession> {
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

  // --- Agent chat (subscription agents) -------------------------------------------

  /** Creates an agent chat on a worker's agent account. */
  createAgentChat(req: AgentChatCreateRequest): Promise<ChatSession> {
    return this.request(CHAT_PROTOCOL.restCreate, {
      method: 'POST',
      body: JSON.stringify({ backend: 'agent', ...req }),
    });
  }

  /** Persisted events of a chat after `afterSeq` (0 = all), oldest first. */
  async listAgentChatEvents(chatId: string, afterSeq = 0): Promise<AgentChatEvent[]> {
    const res = await this.request<{ events?: AgentChatEvent[] } | AgentChatEvent[]>(
      CHAT_PROTOCOL.restEvents(chatId, afterSeq),
    );
    return Array.isArray(res) ? res : (res.events ?? []);
  }

  patchAgentChat(chatId: string, patch: AgentChatPatch): Promise<ChatSession> {
    return this.request(CHAT_PROTOCOL.restChat(chatId), {
      method: 'PATCH',
      body: JSON.stringify(patch),
    });
  }

  /** Every worker's agent accounts (raw: normalized by AgentAccountsStore). */
  listAgentAccounts(): Promise<unknown> {
    return this.request(CHAT_PROTOCOL.restAccounts);
  }

  /** Asks workers to re-probe capabilities (optionally one worker / account). */
  refreshAgentAccounts(req: { worker_id?: string; account_id?: string } = {}): Promise<unknown> {
    return this.request(CHAT_PROTOCOL.restAccountsRefresh, {
      method: 'POST',
      body: JSON.stringify(req),
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

  /** First-run state and the hub's own machine (syred found / running). */
  getSetup(): Promise<SetupStatus> {
    return this.request('/api/setup');
  }

  /** Records that first-run setup is done, so Add Machine stops opening on load. */
  dismissSetup(): Promise<{ ok: boolean }> {
    return this.request('/api/setup/dismiss', { method: 'POST' });
  }

  /** Adds the hub's machine: attaches a running syred or starts one. */
  addLocalWorker(): Promise<AddLocalWorkerResult> {
    return this.request('/api/workers/local', { method: 'POST' });
  }

  /** Creates a short-lived join code for `syred join` (Add machine → Manual). */
  createJoinCode(): Promise<JoinCode> {
    return this.request('/api/machines/join-codes', { method: 'POST' });
  }

  getJoinCode(id: string): Promise<JoinCodeStatus> {
    return this.request(`/api/machines/join-codes/${encodeURIComponent(id)}`);
  }

  /** Asks the machine for its listening ports now (falls back to the last list it sent). */
  getWorkerPorts(workerId: string): Promise<WorkerPorts> {
    return this.request(`/api/workers/${encodeURIComponent(workerId)}/ports`);
  }

  getMachineCredential(workerId: string): Promise<MachineCredentialInfo> {
    return this.request(`/api/workers/${encodeURIComponent(workerId)}/credential`);
  }

  /** Revokes a machine's credential; the hub disconnects it at once. */
  revokeMachineCredential(workerId: string): Promise<{ ok: boolean; revoked: number }> {
    return this.request(`/api/workers/${encodeURIComponent(workerId)}/credential/revoke`, { method: 'POST' });
  }

  /** Revokes the credentials of machines that joined by hand (they need new join codes). */
  rotateMachineCredentials(machine?: string): Promise<RotationResponse> {
    return this.request('/api/machines/credentials/rotate', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(machine ? { machine } : {}),
    });
  }

  /** Retries starting a hub-managed machine's syred. */
  startWorker(workerId: string): Promise<{ worker_id: string; state: string }> {
    return this.request(`/api/workers/${encodeURIComponent(workerId)}/start`, { method: 'POST' });
  }

  listSSHHosts(): Promise<{ hosts: SSHHost[] }> {
    return this.request('/api/ssh/hosts');
  }

  /**
   * Bootstraps syred on an SSH target. Progress is streamed as server-sent events
   * (`stage` frames, then a terminal `done` frame carrying the result); every frame is
   * passed to `opts.onEvent`, so new interactive steps (e.g. host-key confirmation) can
   * be handled by the caller without changing this method. Falls back to a plain JSON
   * response when the hub does not stream. Abort via `opts.signal`.
   */
  async bootstrapSSH(req: SSHBootstrapRequest, opts: SSHBootstrapOptions = {}): Promise<SSHBootstrapResult> {
    const res = await this.send(
      '/api/ssh/bootstrap?stream=1',
      {
        method: 'POST',
        headers: { Accept: 'text/event-stream' },
        body: JSON.stringify(req),
        signal: opts.signal,
      },
      // Non-streaming hubs answer an unknown host key with 409 + the result body.
      { acceptStatuses: [409] },
    );
    const contentType = res.headers.get('content-type') || '';
    if (!contentType.includes('text/event-stream') || !res.body) {
      return (await res.json()) as SSHBootstrapResult;
    }
    let result: SSHBootstrapResult | null = null;
    for await (const ev of readServerSentEvents<SSHBootstrapEvent>(res.body)) {
      opts.onEvent?.(ev);
      if (ev.type === 'done' && 'result' in ev && ev.result) {
        result = ev.result as SSHBootstrapResult;
      }
    }
    if (!result) {
      throw new Error('SSH bootstrap ended without a result');
    }
    return result;
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

  /** Sets (or clears with '') a machine's default terminal shell. */
  setWorkerDefaultShell(workerId: string, defaultShell: string): Promise<{ ok: boolean; default_shell: string; worker: Worker }> {
    return this.request(`/api/workers/${encodeURIComponent(workerId)}/default-shell`, {
      method: 'PUT',
      body: JSON.stringify({ default_shell: defaultShell }),
    });
  }

  /** Enables or disables configuring agent CLIs to use Git Bash on a Windows machine. */
  setWorkerAgentsUseGitBash(
    workerId: string,
    enabled: boolean,
  ): Promise<{ ok: boolean; agents_use_git_bash: boolean; worker: Worker }> {
    return this.request(`/api/workers/${encodeURIComponent(workerId)}/agents-use-git-bash`, {
      method: 'PUT',
      body: JSON.stringify({ agents_use_git_bash: enabled }),
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

  listProjects(): Promise<{ projects: Project[] }> {
    return this.request('/api/projects');
  }

  createProject(req: {
    name: string;
    verify_cmd?: string;
    autonomy?: AutonomyPolicy;
    delivery_mode?: string;
    merge_style?: string;
  }): Promise<Project> {
    return this.request('/api/projects', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  updateProject(
    id: string,
    patch: {
      name?: string;
      verify_cmd?: string;
      autonomy?: AutonomyPolicy;
      delivery_mode?: string;
      merge_style?: string;
    },
  ): Promise<Project> {
    return this.request(`/api/projects/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify(patch),
    });
  }

  deleteProject(id: string): Promise<{ ok: boolean; id: string }> {
    return this.request(`/api/projects/${encodeURIComponent(id)}`, { method: 'DELETE' });
  }

  listProjectRepos(projectId: string): Promise<{ repos: Repo[] }> {
    return this.request(`/api/projects/${encodeURIComponent(projectId)}/repos`);
  }

  createRepo(
    projectId: string,
    req: {
      remote_url: string;
      name?: string;
      default_branch?: string;
      verify_cmd?: string;
      delivery_mode?: string;
      merge_style?: string;
    },
  ): Promise<Repo> {
    return this.request(`/api/projects/${encodeURIComponent(projectId)}/repos`, {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  updateRepo(
    projectId: string,
    repoId: string,
    patch: {
      name?: string;
      remote_url?: string;
      default_branch?: string;
      verify_cmd?: string;
      delivery_mode?: string;
      merge_style?: string;
    },
  ): Promise<Repo> {
    return this.request(`/api/projects/${encodeURIComponent(projectId)}/repos/${encodeURIComponent(repoId)}`, {
      method: 'PATCH',
      body: JSON.stringify(patch),
    });
  }

  deleteRepo(projectId: string, repoId: string): Promise<{ ok: boolean; id: string }> {
    return this.request(`/api/projects/${encodeURIComponent(projectId)}/repos/${encodeURIComponent(repoId)}`, {
      method: 'DELETE',
    });
  }

  listTickets(params?: { project_id?: string; parent_id?: string; status?: string; kind?: string }): Promise<{ tickets: Ticket[] }> {
    const sp = new URLSearchParams();
    if (params?.project_id) sp.set('project_id', params.project_id);
    if (params?.parent_id) sp.set('parent_id', params.parent_id);
    if (params?.status) sp.set('status', params.status);
    if (params?.kind) sp.set('kind', params.kind);
    const qs = sp.toString();
    return this.request(`/api/tickets${qs ? '?' + qs : ''}`);
  }

  getTicket(id: string): Promise<{ ticket: Ticket; blocked_by: TicketEdge[]; repo_bindings: TicketRepoBinding[]; rollup?: TicketRollup }> {
    return this.request(`/api/tickets/${encodeURIComponent(id)}`);
  }

  createTicket(req: Partial<Ticket> & { title: string }): Promise<Ticket> {
    return this.request('/api/tickets', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  updateTicket(id: string, patch: Partial<Ticket>): Promise<Ticket> {
    return this.request(`/api/tickets/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify(patch),
    });
  }

  deleteTicket(id: string): Promise<{ ok: boolean; id: string }> {
    return this.request(`/api/tickets/${encodeURIComponent(id)}`, { method: 'DELETE' });
  }

  // --- Forges (SYR-QHGWZ / SYR-CK7S2) ---

  listForges(): Promise<{ forges: ForgeHost[] }> {
    return this.request('/api/forges');
  }

  detectForge(req: { remote_url?: string; host?: string }): Promise<ForgeDetectResult> {
    return this.request('/api/forges/detect', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  putForgeToken(host: string, token: string, kind?: ForgeKind): Promise<ForgeTokenSaveResult> {
    return this.request(`/api/forges/${encodeURIComponent(host)}/token`, {
      method: 'PUT',
      body: JSON.stringify({ token, kind: kind || undefined }),
    });
  }

  deleteForgeToken(host: string): Promise<{ ok: boolean; host: string }> {
    return this.request(`/api/forges/${encodeURIComponent(host)}/token`, {
      method: 'DELETE',
    });
  }

  startForgeDevice(host: string, kind?: ForgeKind): Promise<ForgeDeviceStart> {
    return this.request(`/api/forges/${encodeURIComponent(host)}/device`, {
      method: 'POST',
      body: JSON.stringify({ kind: kind || undefined }),
    });
  }

  pollForgeDevice(host: string, id: string): Promise<ForgeDevicePoll> {
    return this.request(`/api/forges/${encodeURIComponent(host)}/device/${encodeURIComponent(id)}`);
  }
}

/**
 * Maps hub statuses that have a well-known cause to an actionable message; the
 * server's own text is kept where it adds detail.
 */
export function friendlyErrorMessage(res: Response, serverMsg: string): string {
  switch (res.status) {
    case 429: {
      const retry = Number.parseInt(res.headers.get('Retry-After') || '', 10);
      return Number.isFinite(retry) && retry > 0
        ? `Too many attempts, try again in ${retry}s.`
        : 'Too many attempts, try again shortly.';
    }
    case 403:
      return `The hub rejected this request as cross-site (${serverMsg}). Reload Syre from the hub's own address and try again.`;
    case 415:
      return `The hub rejected the request format (${serverMsg}). Reload Syre to pick up the latest version.`;
    case 413:
      return serverMsg.startsWith('HTTP ') ? 'That upload is too large for the hub.' : `Too large: ${serverMsg}.`;
    default:
      return serverMsg;
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

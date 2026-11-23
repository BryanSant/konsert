/**
 * Pure reducer that turns the AgentChatEvent stream into transcript items and a status.
 * No Angular here: the store keeps one TranscriptState per chat in a signal.
 */
import {
  AGENT_EVENT,
  AgentChatError,
  AgentChatEvent,
  AgentElicitation,
  AgentPlanItem,
  AgentRateLimit,
  AgentTokenUsage,
  AgentToolCall,
  ChatStatus,
} from '../models/agent-chat.models';
import { CHAT_PROTOCOL } from '../services/chat-protocol';

export interface UserItem {
  type: 'user';
  key: string;
  turnId: string;
  text: string;
  files: string[];
  at: string;
  /** Sent by this browser and not yet echoed by the hub. */
  pending?: boolean;
}
export interface AssistantItem {
  type: 'assistant';
  key: string;
  turnId: string;
  messageId: string;
  text: string;
  streaming: boolean;
  at: string;
}
export interface ThoughtItem {
  type: 'thought';
  key: string;
  turnId: string;
  text: string;
  streaming: boolean;
  at: string;
}
export interface ToolItem {
  type: 'tool';
  key: string;
  turnId: string;
  call: AgentToolCall;
  at: string;
}
export interface PlanItem {
  type: 'plan';
  key: string;
  turnId: string;
  items: AgentPlanItem[];
  at: string;
}
export type ElicitationState = 'pending' | 'answering' | 'resolved' | 'expired';
export interface ElicitationItem {
  type: 'elicitation';
  key: string;
  turnId: string;
  elicitation: AgentElicitation;
  state: ElicitationState;
  /** What the user chose / typed (label), once answered. */
  answer?: string;
  at: string;
}
export interface ErrorItem {
  type: 'error';
  key: string;
  turnId: string;
  error: AgentChatError;
  /** The user dismissed it (e.g. "Keep waiting" on a timeout). */
  dismissed?: boolean;
  at: string;
}
export interface TurnEndItem {
  type: 'turn_end';
  key: string;
  turnId: string;
  stopReason: string;
  usage?: AgentTokenUsage;
  /** The turn's last assistant message ends with a question. */
  repliedWithQuestion: boolean;
  at: string;
}

export type TranscriptItem =
  | UserItem
  | AssistantItem
  | ThoughtItem
  | ToolItem
  | PlanItem
  | ElicitationItem
  | ErrorItem
  | TurnEndItem;

export interface TranscriptState {
  chatId: string;
  items: TranscriptItem[];
  /** Highest seq applied (0 = nothing from the hub yet). */
  lastSeq: number;
  status: ChatStatus;
  /** Latest running tool title while working. */
  subtitle: string;
  currentTurnId: string;
  model?: string;
  effort?: string;
  approvalMode?: string;
  agentSessionId?: string;
  rateLimit?: AgentRateLimit;
  /** usage that arrived before its turn_end, by turn. */
  pendingUsage: Record<string, AgentTokenUsage>;
}

export function emptyTranscript(chatId: string, status: ChatStatus = 'idle'): TranscriptState {
  return { chatId, items: [], lastSeq: 0, status, subtitle: '', currentTurnId: '', pendingUsage: {} };
}

const KNOWN_STATUSES = new Set<ChatStatus>(['starting', 'idle', 'working', 'needs_you', 'done', 'failed', 'cancelled']);

export function isChatStatus(s: string | undefined): s is ChatStatus {
  return !!s && KNOWN_STATUSES.has(s as ChatStatus);
}

/** Applies every event in order. */
export function applyEvents(state: TranscriptState, events: AgentChatEvent[]): TranscriptState {
  return events.reduce(applyEvent, state);
}

/**
 * Applies one event. Events with seq <= lastSeq are duplicates (replays after reconnect)
 * and are ignored; seq 0 marks local/synthetic events, which always apply.
 */
export function applyEvent(state: TranscriptState, ev: AgentChatEvent): TranscriptState {
  if (ev.seq > 0 && ev.seq <= state.lastSeq) return state;
  const s: TranscriptState = { ...state, lastSeq: ev.seq > 0 ? ev.seq : state.lastSeq };
  const turnId = ev.turn_id || s.currentTurnId || '';
  const at = ev.at || new Date().toISOString();

  switch (ev.kind) {
    case CHAT_PROTOCOL.userMessageKind: {
      const parsed = splitAttachments(ev.text ?? '');
      // Replace the optimistic copy: matched by turn id, or (if the hub assigned its own
      // turn id) by being the oldest pending prompt with the same text.
      let items = s.items.filter((i) => !(i.type === 'user' && i.pending && i.turnId === turnId));
      if (items.length === s.items.length) {
        const idx = items.findIndex((i) => i.type === 'user' && i.pending && i.text === parsed.text);
        if (idx >= 0) items = items.filter((_, n) => n !== idx);
      }
      items.push({ type: 'user', key: `u:${turnId || ev.seq}`, turnId, text: parsed.text, files: parsed.files, at });
      return { ...s, items, currentTurnId: turnId, status: 'working', subtitle: '' };
    }

    case AGENT_EVENT.sessionReady:
      return {
        ...s,
        agentSessionId: ev.agent_session_id || s.agentSessionId,
        model: ev.model || s.model,
        effort: ev.effort || s.effort,
        approvalMode: ev.approval_mode || s.approvalMode,
        status: s.status === 'starting' ? 'idle' : s.status,
      };

    case AGENT_EVENT.status: {
      if (!isChatStatus(ev.status)) return s;
      const next = { ...s, status: ev.status };
      if (ev.status !== 'working') next.subtitle = '';
      return next;
    }

    case AGENT_EVENT.messageDelta: {
      const items = closeThoughts(s.items, turnId);
      const idx = findAssistant(items, turnId, ev.message_id);
      if (idx >= 0) {
        const cur = items[idx] as AssistantItem;
        items[idx] = { ...cur, text: cur.text + (ev.text ?? ''), streaming: true };
      } else {
        const mid = ev.message_id || `live:${turnId}:${items.length}`;
        items.push({ type: 'assistant', key: `a:${mid}`, turnId, messageId: mid, text: ev.text ?? '', streaming: true, at });
      }
      return working({ ...s, items, currentTurnId: turnId || s.currentTurnId });
    }

    case AGENT_EVENT.message: {
      const items = closeThoughts(s.items, turnId);
      const idx = findAssistant(items, turnId, ev.message_id, true);
      if (idx >= 0) {
        items[idx] = { ...(items[idx] as AssistantItem), text: ev.text ?? '', streaming: false };
      } else {
        const mid = ev.message_id || `msg:${turnId}:${items.length}`;
        items.push({ type: 'assistant', key: `a:${mid}`, turnId, messageId: mid, text: ev.text ?? '', streaming: false, at });
      }
      return { ...s, items };
    }

    case AGENT_EVENT.thoughtDelta: {
      const items = [...s.items];
      const last = items[items.length - 1];
      if (last && last.type === 'thought' && last.streaming && last.turnId === turnId) {
        items[items.length - 1] = { ...last, text: last.text + (ev.text ?? '') };
      } else {
        items.push({ type: 'thought', key: `th:${turnId}:${ev.seq || items.length}`, turnId, text: ev.text ?? '', streaming: true, at });
      }
      return working({ ...s, items, currentTurnId: turnId || s.currentTurnId });
    }

    case AGENT_EVENT.toolCall: {
      const call = ev.tool_call;
      if (!call?.id) return s;
      const items = closeThoughts(s.items, turnId);
      const idx = items.findIndex((i) => i.type === 'tool' && i.call.id === call.id);
      let merged: AgentToolCall;
      if (idx >= 0) {
        const cur = items[idx] as ToolItem;
        merged = mergeToolCall(cur.call, call);
        items[idx] = { ...cur, call: merged };
      } else {
        merged = { ...call, status: call.status || 'pending' };
        items.push({ type: 'tool', key: `tc:${call.id}`, turnId, call: merged, at });
      }
      const running = merged.status === 'running' || merged.status === 'pending';
      const next = { ...s, items };
      if (running) {
        next.subtitle = merged.title || merged.name;
        if (next.status !== 'needs_you') next.status = 'working';
      }
      return next;
    }

    case AGENT_EVENT.plan: {
      const items = [...s.items];
      const idx = items.findIndex((i) => i.type === 'plan' && i.turnId === turnId);
      const plan = ev.plan ?? [];
      if (idx >= 0) items[idx] = { ...(items[idx] as PlanItem), items: plan };
      else items.push({ type: 'plan', key: `plan:${turnId}:${ev.seq}`, turnId, items: plan, at });
      return { ...s, items };
    }

    case AGENT_EVENT.elicitation: {
      const e = ev.elicitation;
      if (!e?.id) return s;
      const items = closeThoughts(s.items, turnId);
      const idx = items.findIndex((i) => i.type === 'elicitation' && i.elicitation.id === e.id);
      const item: ElicitationItem = { type: 'elicitation', key: `el:${e.id}`, turnId, elicitation: e, state: 'pending', at };
      if (idx >= 0) items[idx] = item;
      else items.push(item);
      return { ...s, items, status: 'needs_you' };
    }

    case AGENT_EVENT.elicitationResolved: {
      const id = ev.elicitation?.id;
      if (!id) return s;
      const items = s.items.map((i) =>
        i.type === 'elicitation' && i.elicitation.id === id ? { ...i, state: 'resolved' as const } : i,
      );
      const stillPending = items.some((i) => i.type === 'elicitation' && (i.state === 'pending' || i.state === 'answering'));
      const status: ChatStatus = s.status === 'needs_you' && !stillPending ? 'working' : s.status;
      return { ...s, items, status };
    }

    case AGENT_EVENT.usage: {
      if (!ev.usage) return s;
      const items = [...s.items];
      const idx = findLastIndex(items, (i) => i.type === 'turn_end' && i.turnId === turnId);
      if (idx >= 0) {
        items[idx] = { ...(items[idx] as TurnEndItem), usage: ev.usage };
        return { ...s, items };
      }
      return { ...s, pendingUsage: { ...s.pendingUsage, [turnId]: ev.usage } };
    }

    case AGENT_EVENT.rateLimit:
      return ev.rate_limit ? { ...s, rateLimit: ev.rate_limit } : s;

    case AGENT_EVENT.turnEnd: {
      const stop = ev.stop_reason || 'end_turn';
      const items = finishTurn(s.items, turnId);
      const lastAssistant = findLastIndex(items, (i) => i.type === 'assistant' && i.turnId === turnId);
      const text = lastAssistant >= 0 ? (items[lastAssistant] as AssistantItem).text : '';
      const { [turnId]: usage, ...restUsage } = s.pendingUsage;
      items.push({
        type: 'turn_end',
        key: `end:${turnId}:${ev.seq}`,
        turnId,
        stopReason: stop,
        usage,
        repliedWithQuestion: stop === 'end_turn' && endsWithQuestion(text),
        at,
      });
      let status: ChatStatus = 'done';
      if (stop === 'cancelled') status = 'cancelled';
      else if (stop === 'error' || s.status === 'failed') status = 'failed';
      return { ...s, items, status, subtitle: '', pendingUsage: restUsage };
    }

    case AGENT_EVENT.error: {
      if (!ev.error) return s;
      const err = ev.error;
      // A watchdog timeout leaves the engine running: the turn may still finish.
      const terminal = err.kind !== 'timeout';
      const items = terminal ? finishTurn(s.items, turnId) : [...s.items];
      items.push({ type: 'error', key: `err:${turnId}:${ev.seq || items.length}`, turnId, error: err, at });
      return terminal ? { ...s, items, status: 'failed', subtitle: '' } : { ...s, items };
    }

    default:
      return s;
  }
}

/** Optimistically records the user's message before the hub echoes it. */
export function addPendingUser(state: TranscriptState, turnId: string, text: string, files: string[] = []): TranscriptState {
  const item: UserItem = { type: 'user', key: `u:${turnId}`, turnId, text, files, at: new Date().toISOString(), pending: true };
  return { ...state, items: [...state.items, item], currentTurnId: turnId, status: 'working', subtitle: '' };
}

/** Rolls back a pending user message whose frame could not be sent. */
export function removePendingUser(state: TranscriptState, turnId: string): TranscriptState {
  return { ...state, items: state.items.filter((i) => !(i.type === 'user' && i.pending && i.turnId === turnId)) };
}

/** Marks an elicitation as being answered (until elicitation_resolved arrives). */
export function markAnswering(state: TranscriptState, elicitationId: string, answer: string): TranscriptState {
  const items = state.items.map((i) =>
    i.type === 'elicitation' && i.elicitation.id === elicitationId ? { ...i, state: 'answering' as const, answer } : i,
  );
  return { ...state, items };
}

/** Undo markAnswering when the respond frame could not be sent. */
export function unmarkAnswering(state: TranscriptState, elicitationId: string): TranscriptState {
  const items = state.items.map((i) =>
    i.type === 'elicitation' && i.elicitation.id === elicitationId && i.state === 'answering'
      ? { ...i, state: 'pending' as const, answer: undefined }
      : i,
  );
  return { ...state, items };
}

export function dismissError(state: TranscriptState, key: string): TranscriptState {
  return { ...state, items: state.items.map((i) => (i.type === 'error' && i.key === key ? { ...i, dismissed: true } : i)) };
}

/** Pending elicitations (bottom-of-transcript cards). */
export function pendingElicitations(state: TranscriptState): ElicitationItem[] {
  return state.items.filter((i): i is ElicitationItem => i.type === 'elicitation' && (i.state === 'pending' || i.state === 'answering'));
}

export function endsWithQuestion(text: string): boolean {
  const t = text
    .trim()
    .replace(/[*_`)\]"'”’]+$/, '')
    .trim();
  return t.endsWith('?');
}

// --- helpers ---------------------------------------------------------------------------

function working(s: TranscriptState): TranscriptState {
  if (s.status === 'needs_you' || s.status === 'working') return s;
  return { ...s, status: 'working' };
}

function findAssistant(items: TranscriptItem[], turnId: string, messageId?: string, includeDone = false): number {
  if (messageId) return findLastIndex(items, (i) => i.type === 'assistant' && i.messageId === messageId);
  // No id: the turn's still-streaming message (or, for a final message, any live one).
  return findLastIndex(
    items,
    (i) => i.type === 'assistant' && i.turnId === turnId && (i.streaming || (includeDone && i.messageId.startsWith('live:'))),
  );
}

function closeThoughts(items: TranscriptItem[], turnId: string): TranscriptItem[] {
  return items.map((i) => (i.type === 'thought' && i.streaming && i.turnId === turnId ? { ...i, streaming: false } : i));
}

function finishTurn(items: TranscriptItem[], turnId: string): TranscriptItem[] {
  return items.map((i) => {
    if ((i.type === 'assistant' || i.type === 'thought') && i.streaming) return { ...i, streaming: false };
    if (i.type === 'tool' && i.turnId === turnId && (i.call.status === 'running' || i.call.status === 'pending')) {
      return { ...i, call: { ...i.call, status: 'cancelled' } };
    }
    if (i.type === 'elicitation' && (i.state === 'pending' || i.state === 'answering') && i.turnId === turnId) {
      return { ...i, state: 'expired' as const };
    }
    return i;
  });
}

function mergeToolCall(cur: AgentToolCall, upd: AgentToolCall): AgentToolCall {
  return {
    ...cur,
    name: upd.name || cur.name,
    kind: upd.kind || cur.kind,
    title: upd.title || cur.title,
    status: upd.status || cur.status,
    input: upd.input || cur.input,
    output: upd.output || cur.output,
    paths: upd.paths?.length ? upd.paths : cur.paths,
  };
}

function findLastIndex<T>(arr: T[], pred: (v: T) => boolean): number {
  for (let i = arr.length - 1; i >= 0; i--) if (pred(arr[i])) return i;
  return -1;
}

const ATTACHMENT_MARKER = '\n\n[Attached files on host]\n';

/** Splits the legacy "[Attached files on host]" trailer off a user message. */
export function splitAttachments(content: string): { text: string; files: string[] } {
  const idx = content.indexOf(ATTACHMENT_MARKER);
  if (idx === -1) return { text: content, files: [] };
  const files = content
    .slice(idx + ATTACHMENT_MARKER.length)
    .split('\n')
    .map((l) => l.replace(/^-\s*/, '').trim())
    .filter(Boolean);
  return { text: content.slice(0, idx).trim(), files };
}

export { ATTACHMENT_MARKER };

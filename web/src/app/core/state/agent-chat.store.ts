import { Injectable, inject, signal } from '@angular/core';
import { AgentChatCreateRequest, AgentChatEvent, ChatStatus } from '../models/agent-chat.models';
import { ChatSession } from '../models/syre.models';
import { ApiClientService } from '../services/api-client.service';
import { ChatControlFrame, ChatTransportService } from '../services/chat-transport.service';
import {
  TranscriptState,
  UserItem,
  addPendingUser,
  applyEvent,
  applyEvents,
  dismissError,
  emptyTranscript,
  isChatStatus,
  markAnswering,
  removePendingUser,
  unmarkAnswering,
} from './chat-transcript';
import { CHAT_NOT_CONNECTED_ERROR, CommanderStore, isAgentChat } from './commander.store';

let turnCounter = 0;
export function newTurnId(): string {
  turnCounter++;
  const rand = Math.random().toString(36).slice(2, 8);
  return `t-${Date.now().toString(36)}-${turnCounter}-${rand}`;
}

export { isAgentChat };

/**
 * Agent chat state: one TranscriptState per chat, fed by `agent.chat.event` frames and
 * REST catch-up (after_seq). Writes go through ChatTransportService.
 */
@Injectable({ providedIn: 'root' })
export class AgentChatStore {
  private readonly api = inject(ApiClientService);
  private readonly transport = inject(ChatTransportService);
  private readonly orch = inject(CommanderStore);

  readonly transcripts = signal<Record<string, TranscriptState>>({});
  readonly error = signal('');
  private readonly loading = new Map<string, Promise<void>>();

  transcript(chatId: string): TranscriptState {
    return this.transcripts()[chatId] ?? emptyTranscript(chatId, this.sessionStatus(chatId));
  }

  /** Status for the header pill and chat list items. */
  statusOf(chatId: string): ChatStatus {
    const t = this.transcripts()[chatId];
    return t ? t.status : this.sessionStatus(chatId);
  }

  private sessionStatus(chatId: string): ChatStatus {
    const s = this.orch.sessions().find((x) => x.id === chatId)?.status;
    return isChatStatus(s) ? s : 'idle';
  }

  private update(chatId: string, fn: (s: TranscriptState) => TranscriptState): void {
    this.transcripts.update((m) => ({ ...m, [chatId]: fn(m[chatId] ?? emptyTranscript(chatId, this.sessionStatus(chatId))) }));
  }

  /** Loads a chat's event log (the first time) or what was missed since (later). */
  async open(chatId: string): Promise<void> {
    return this.catchUp(chatId);
  }

  /** Fetches events after the last applied seq (reconnect / gap recovery). */
  catchUp(chatId: string): Promise<void> {
    const inflight = this.loading.get(chatId);
    if (inflight) return inflight;
    const p = (async () => {
      try {
        const after = this.transcripts()[chatId]?.lastSeq ?? 0;
        const events = await this.api.listAgentChatEvents(chatId, after);
        events.sort((a, b) => a.seq - b.seq);
        this.update(chatId, (s) => applyEvents(s, events));
      } catch (err) {
        this.error.set(err instanceof Error ? err.message : 'Failed to load the conversation');
        // Keep an (empty) transcript so the view renders.
        this.update(chatId, (s) => s);
      } finally {
        this.loading.delete(chatId);
      }
    })();
    this.loading.set(chatId, p);
    return p;
  }

  /** One `agent.chat.event` frame (payload is the event, or {event}). */
  onEvent(payload: unknown): void {
    const ev = ((payload as { event?: AgentChatEvent })?.event ?? payload) as AgentChatEvent | undefined;
    if (!ev?.chat_id || !ev.kind) return;
    const cur = this.transcripts()[ev.chat_id];
    if (cur && cur.lastSeq > 0 && ev.seq > cur.lastSeq + 1) {
      // Missed events (e.g. dropped while switching): fetch the gap, which includes this one.
      void this.catchUp(ev.chat_id);
      return;
    }
    this.update(ev.chat_id, (s) => applyEvent(s, ev));
    if (ev.kind === 'status' && isChatStatus(ev.status)) {
      this.orch.sessions.update((list) => list.map((x) => (x.id === ev.chat_id ? { ...x, status: ev.status } : x)));
    }
  }

  /**
   * A chat session pushed by the hub (create, settings, title or status change). Chats that
   * aren't open receive no agent.chat.event frames, so their cached transcript status is
   * refreshed from the session; the open chat's status comes from its own event stream.
   */
  onSessionUpsert(session: ChatSession): void {
    this.orch.upsertSession(session);
    const t = this.transcripts()[session.id];
    if (t && session.id !== this.orch.activeSessionId() && isChatStatus(session.status) && t.status !== session.status) {
      this.transcripts.update((m) => ({ ...m, [session.id]: { ...t, status: session.status as ChatStatus } }));
    }
  }

  /** A chat deleted elsewhere: remove it from the list and drop its transcript. */
  async onSessionDeleted(chatId: string): Promise<void> {
    if (this.transcripts()[chatId]) {
      this.transcripts.update((m) => {
        const next = { ...m };
        delete next[chatId];
        return next;
      });
    }
    await this.orch.removeSession(chatId);
  }

  async onReconnected(): Promise<void> {
    const id = this.orch.activeSessionId();
    if (id && this.transcripts()[id]) await this.catchUp(id);
  }

  /**
   * Sends a user turn. Returns false (draft kept, error shown) when the socket is not open.
   */
  send(chat: ChatSession, text: string, attachments: string[] = []): boolean {
    if (!this.transport.isOpen) {
      this.error.set(CHAT_NOT_CONNECTED_ERROR);
      return false;
    }
    const turnId = newTurnId();
    this.error.set('');
    this.update(chat.id, (s) => addPendingUser(s, turnId, text, attachments));
    const ok = this.transport.send({
      session_id: chat.id,
      chat_id: chat.id,
      turn_id: turnId,
      content: text,
      attachments: attachments.length ? attachments : undefined,
    });
    if (!ok) {
      this.update(chat.id, (s) => removePendingUser(s, turnId));
      this.error.set(CHAT_NOT_CONNECTED_ERROR);
    }
    return ok;
  }

  /** Re-sends the last user message of a chat (Retry on retryable errors). */
  retry(chat: ChatSession): boolean {
    const last = [...this.transcript(chat.id).items].reverse().find((i): i is UserItem => i.type === 'user');
    if (!last) return false;
    return this.send(chat, last.text, last.files);
  }

  respond(chatId: string, elicitationId: string, answer: { optionId?: string; text?: string; label: string }): boolean {
    this.update(chatId, (s) => markAnswering(s, elicitationId, answer.label));
    const ok = this.transport.respond({
      chat_id: chatId,
      elicitation_id: elicitationId,
      option_id: answer.optionId,
      text: answer.text,
    });
    if (!ok) {
      this.update(chatId, (s) => unmarkAnswering(s, elicitationId));
      this.error.set(CHAT_NOT_CONNECTED_ERROR);
    }
    return ok;
  }

  cancel(chatId: string): boolean {
    return this.transport.control({ chat_id: chatId, cancel: true });
  }

  /** Live model / effort / approval change; also persisted on the session. */
  control(chatId: string, changes: Omit<ChatControlFrame, 'chat_id' | 'cancel'>): boolean {
    const clean = Object.fromEntries(Object.entries(changes).filter(([, v]) => v)) as typeof changes;
    if (Object.keys(clean).length === 0) return true;
    this.orch.sessions.update((list) =>
      list.map((x) => (x.id === chatId ? { ...x, ...clean } : x)),
    );
    return this.transport.control({ chat_id: chatId, ...clean });
  }

  /** Switches the chat to another account (persisted; applies from the next turn). */
  async switchAccount(chatId: string, accountId: string): Promise<void> {
    try {
      const updated = await this.api.patchAgentChat(chatId, { account_id: accountId });
      this.orch.sessions.update((list) => list.map((x) => (x.id === chatId ? { ...x, ...updated, account_id: accountId } : x)));
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not switch account');
    }
  }

  dismissError(chatId: string, key: string): void {
    this.update(chatId, (s) => dismissError(s, key));
  }

  /** Creates an agent chat, adds it to the session list and selects it. */
  async create(req: AgentChatCreateRequest): Promise<ChatSession> {
    const created = await this.api.createAgentChat(req);
    const session: ChatSession = { backend: 'agent', ...req, ...created };
    this.orch.sessions.update((s) => [session, ...s.filter((x) => x.id !== session.id)]);
    this.transcripts.update((m) => ({ ...m, [session.id]: emptyTranscript(session.id, 'idle') }));
    await this.orch.selectSession(session.id);
    return session;
  }
}

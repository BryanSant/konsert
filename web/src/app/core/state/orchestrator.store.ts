import { Injectable, computed, inject, signal } from '@angular/core';
import { ChatMessage, ChatSession, ChatToolCard } from '../models/konsert.models';
import { ApiClientService } from '../services/api-client.service';

/** Is this a subscription-agent chat (vs the optional API-key orchestrator)? */
export function isAgentChat(s: ChatSession | undefined | null): boolean {
  return !!s && (s.backend === 'agent' || (!s.backend && !!s.agent));
}

export const CHAT_NOT_CONNECTED_ERROR =
  'Not connected to the Konsert hub. Your message was not sent and is still in the composer.';
export const CHAT_CONNECTION_LOST_ERROR =
  'Connection to the hub was lost while waiting for a reply. The conversation will refresh when it reconnects.';

@Injectable({ providedIn: 'root' })
export class OrchestratorStore {
  private readonly api = inject(ApiClientService);

  readonly sessions = signal<ChatSession[]>([]);
  readonly activeSessionId = signal<string>('');
  readonly messages = signal<ChatMessage[]>([]);
  readonly streamingBuffer = signal<string>('');
  readonly activeToolCards = signal<ChatToolCard[]>([]);
  readonly error = signal('');

  /** Session with a chat.send awaiting chat.done / chat.error ('' when idle). */
  readonly inFlightSessionId = signal('');

  /**
   * True only while the *active* session has a reply in flight. Switching to another
   * session therefore never leaves its composer locked.
   */
  readonly sending = computed(() => {
    const inflight = this.inFlightSessionId();
    return inflight !== '' && inflight === this.activeSessionId();
  });

  /** Guards against an older listChatMessages response overwriting a newer one. */
  private messagesSeq = 0;

  async init(): Promise<void> {
    try {
      const res = await this.api.listChatSessions();
      const list = res.sessions ?? [];
      this.sessions.set(list);
      // No chats yet: the view shows a new-chat composer (nothing is created until the
      // first message, and never an API-key chat by default).
      if (list.length > 0) await this.selectSession(list[0].id);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Failed to load chat sessions');
    }
  }

  /**
   * Loads the chat list without opening one, so views other than the orchestrator chat
   * (Live work, Fleet, Work, Wall, the phone inbox) know every chat on a direct load.
   */
  async loadSessions(): Promise<void> {
    try {
      const res = await this.api.listChatSessions();
      // init() may have finished first; never drop chats it (or a live update) added.
      const byId = new Map((res.sessions ?? []).map((s) => [s.id, s]));
      for (const s of this.sessions()) byId.set(s.id, s);
      this.sessions.set([...byId.values()]);
    } catch {
      // The chat view retries with init() and reports errors there.
    }
  }

  async createSession(title?: string): Promise<void> {
    const created = await this.api.createChatSession(
      title || `Session ${this.sessions().length + 1}`,
    );
    this.sessions.update((s) => [created, ...s]);
    await this.selectSession(created.id);
  }

  async renameSession(sessionId: string, title: string): Promise<void> {
    const clean = title.trim();
    if (!clean) return;
    const updated = await this.api.renameChatSession(sessionId, clean);
    this.sessions.update((list) =>
      list.map((s) => (s.id === sessionId ? { ...s, title: updated.title || clean } : s)),
    );
  }

  /**
   * Inserts or merges a session pushed by the hub (fleet.update chat_session): chats created
   * in another tab, by the CLI or by the orchestrator appear without a reload. New chats go
   * to the top; known ones keep their place.
   */
  upsertSession(session: ChatSession): void {
    this.sessions.update((list) => {
      const idx = list.findIndex((s) => s.id === session.id);
      if (idx === -1) return [session, ...list];
      const next = [...list];
      next[idx] = { ...list[idx], ...session };
      return next;
    });
  }

  /** Drops a session deleted elsewhere (fleet.update chat_session_deleted). */
  async removeSession(sessionId: string): Promise<void> {
    if (!this.sessions().some((s) => s.id === sessionId)) return;
    await this.forgetSession(sessionId);
  }

  async deleteSession(sessionId: string): Promise<void> {
    await this.api.deleteChatSession(sessionId);
    await this.forgetSession(sessionId);
  }

  private async forgetSession(sessionId: string): Promise<void> {
    if (this.inFlightSessionId() === sessionId) {
      this.inFlightSessionId.set('');
    }
    const remaining = this.sessions().filter((s) => s.id !== sessionId);
    this.sessions.set(remaining);
    if (remaining.length === 0) {
      this.startDraft();
      return;
    }
    if (this.activeSessionId() === sessionId) {
      await this.selectSession(remaining[0].id);
    }
  }

  /** Shows the new-chat composer (no active session). */
  startDraft(): void {
    this.activeSessionId.set('');
    this.messages.set([]);
    this.streamingBuffer.set('');
    this.activeToolCards.set([]);
    this.error.set('');
  }

  async selectSession(sessionId: string): Promise<void> {
    this.activeSessionId.set(sessionId);
    this.streamingBuffer.set('');
    this.activeToolCards.set([]);
    this.error.set('');
    this.messages.set([]);
    // Agent chats are rebuilt from their event log (AgentChatStore), not messages.
    if (isAgentChat(this.sessions().find((s) => s.id === sessionId))) return;
    await this.reloadMessages(sessionId, true);
  }

  /**
   * Records the optimistic user message and marks the session as awaiting a reply.
   * Call only once the frame is known to be deliverable (socket open).
   */
  beginSend(sessionId: string, content: string): void {
    this.inFlightSessionId.set(sessionId);
    this.streamingBuffer.set('');
    this.activeToolCards.set([]);
    this.error.set('');
    this.messages.update((list) => [
      ...list,
      {
        // Negative ids mark optimistic rows that are not yet persisted.
        id: -Date.now(),
        session_id: sessionId,
        role: 'user',
        content,
        created_at: new Date().toISOString(),
      },
    ]);
  }

  /** Rolls back a beginSend whose frame could not be written to the socket. */
  abortSend(sessionId: string, errMsg = CHAT_NOT_CONNECTED_ERROR): void {
    if (this.inFlightSessionId() === sessionId) {
      this.inFlightSessionId.set('');
    }
    this.messages.update((list) => {
      const last = list[list.length - 1];
      return last && last.role === 'user' && last.id < 0 ? list.slice(0, -1) : list;
    });
    this.error.set(errMsg);
  }

  /** Refuses a send outright (socket not open); the caller keeps the draft. */
  rejectSend(errMsg = CHAT_NOT_CONNECTED_ERROR): void {
    this.error.set(errMsg);
  }

  /** The socket closed: any in-flight reply is lost, so unlock the composer. */
  onConnectionLost(): void {
    if (this.inFlightSessionId() === '') return;
    const wasActive = this.sending();
    this.inFlightSessionId.set('');
    this.streamingBuffer.set('');
    this.activeToolCards.set([]);
    if (wasActive) {
      this.error.set(CHAT_CONNECTION_LOST_ERROR);
    }
  }

  /** The socket reconnected: pull anything persisted while we were away. */
  async onReconnected(): Promise<void> {
    const sid = this.activeSessionId();
    if (!sid || isAgentChat(this.sessions().find((s) => s.id === sid))) return;
    if (this.error() === CHAT_CONNECTION_LOST_ERROR || this.error() === CHAT_NOT_CONNECTED_ERROR) {
      this.error.set('');
    }
    await this.reloadMessages(sid, false);
  }

  onChatDelta(sessionId: string, delta: string): void {
    if (sessionId !== this.activeSessionId()) return;
    this.streamingBuffer.update((buf) => buf + delta);
  }

  onChatTool(card: ChatToolCard): void {
    if (card.session_id !== this.activeSessionId()) return;
    this.activeToolCards.update((cards) => {
      const idx = cards.findIndex((c) => c.call_id === card.call_id);
      if (idx === -1) return [...cards, card];
      const next = [...cards];
      next[idx] = card;
      return next;
    });
  }

  async onChatDone(sessionId: string): Promise<void> {
    if (sessionId === this.inFlightSessionId()) {
      this.inFlightSessionId.set('');
    }
    if (sessionId !== this.activeSessionId()) return;
    this.streamingBuffer.set('');
    this.activeToolCards.set([]);
    await this.reloadMessages(sessionId, false);
  }

  onChatError(sessionId: string, errMsg: string): void {
    if (sessionId === this.inFlightSessionId()) {
      this.inFlightSessionId.set('');
    }
    if (sessionId !== this.activeSessionId()) return;
    this.streamingBuffer.set('');
    this.error.set(errMsg);
  }

  private async reloadMessages(sessionId: string, reportErrors: boolean): Promise<void> {
    const seq = ++this.messagesSeq;
    try {
      const res = await this.api.listChatMessages(sessionId);
      if (seq !== this.messagesSeq || sessionId !== this.activeSessionId()) return;
      this.messages.set(res.messages ?? []);
    } catch (err) {
      if (!reportErrors || seq !== this.messagesSeq) return;
      this.error.set(err instanceof Error ? err.message : 'Failed to load messages');
    }
  }
}

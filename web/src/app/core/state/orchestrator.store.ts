import { Injectable, inject, signal } from '@angular/core';
import { ChatMessage, ChatSession, ChatToolCard } from '../models/konsert.models';
import { ApiClientService } from '../services/api-client.service';

@Injectable({ providedIn: 'root' })
export class OrchestratorStore {
  private readonly api = inject(ApiClientService);

  readonly sessions = signal<ChatSession[]>([]);
  readonly activeSessionId = signal<string>('');
  readonly messages = signal<ChatMessage[]>([]);
  readonly streamingBuffer = signal<string>('');
  readonly activeToolCards = signal<ChatToolCard[]>([]);
  readonly sending = signal(false);
  readonly error = signal('');

  async init(): Promise<void> {
    try {
      const res = await this.api.listChatSessions();
      let list = res.sessions ?? [];
      if (list.length === 0) {
        const created = await this.api.createChatSession('Default Orchestrator');
        list = [created];
      }
      this.sessions.set(list);
      await this.selectSession(list[0].id);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Failed to load chat sessions');
    }
  }

  async createSession(title?: string): Promise<void> {
    const created = await this.api.createChatSession(title || `Session ${this.sessions().length + 1}`);
    this.sessions.update((s) => [created, ...s]);
    await this.selectSession(created.id);
  }

  async renameSession(sessionId: string, title: string): Promise<void> {
    const clean = title.trim();
    if (!clean) return;
    const updated = await this.api.renameChatSession(sessionId, clean);
    this.sessions.update((list) =>
      list.map((s) => (s.id === sessionId ? { ...s, title: updated.title || clean } : s))
    );
  }

  async deleteSession(sessionId: string): Promise<void> {
    await this.api.deleteChatSession(sessionId);
    const remaining = this.sessions().filter((s) => s.id !== sessionId);
    if (remaining.length === 0) {
      const created = await this.api.createChatSession('Default Orchestrator');
      this.sessions.set([created]);
      await this.selectSession(created.id);
      return;
    }
    this.sessions.set(remaining);
    if (this.activeSessionId() === sessionId) {
      await this.selectSession(remaining[0].id);
    }
  }

  async selectSession(sessionId: string): Promise<void> {
    this.activeSessionId.set(sessionId);
    this.streamingBuffer.set('');
    this.activeToolCards.set([]);
    this.error.set('');
    try {
      const res = await this.api.listChatMessages(sessionId);
      this.messages.set(res.messages ?? []);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Failed to load messages');
    }
  }

  appendLocalUserMessage(content: string): void {
    const sid = this.activeSessionId();
    this.sending.set(true);
    this.streamingBuffer.set('');
    this.activeToolCards.set([]);
    this.error.set('');
    this.messages.update((list) => [
      ...list,
      {
        id: Date.now(),
        session_id: sid,
        role: 'user',
        content,
        created_at: new Date().toISOString(),
      },
    ]);
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
    if (sessionId !== this.activeSessionId()) return;
    this.sending.set(false);
    this.streamingBuffer.set('');
    this.activeToolCards.set([]);
    try {
      const res = await this.api.listChatMessages(sessionId);
      this.messages.set(res.messages ?? []);
    } catch {
      // ignore
    }
  }

  onChatError(sessionId: string, errMsg: string): void {
    if (sessionId !== this.activeSessionId()) return;
    this.sending.set(false);
    this.error.set(errMsg);
  }
}

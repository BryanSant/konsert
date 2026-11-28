import { Injectable, inject, signal } from '@angular/core';
import { AgentChatEvent } from '../models/agent-chat.models';
import { ApiClientService } from '../services/api-client.service';
import { TranscriptState, applyEvents, emptyTranscript } from './chat-transcript';

/** GET /api/tasks/{id}/transcript: an observed session's transcript, read on its machine. */
export interface ObservedTranscriptResponse {
  external_id: string;
  available: boolean;
  reason?: string;
  truncated?: boolean;
  events?: AgentChatEvent[] | null;
}

/** A live push: an observed session's new events (observed.events). */
export interface ObservedEventsPush {
  external_id: string;
  task_id?: string;
  events: AgentChatEvent[];
}

export interface ObservedView {
  loading: boolean;
  available: boolean;
  reason: string;
  truncated: boolean;
  transcript: TranscriptState;
}

/** How often an open tab renews the machine's watch (the hub renews every 2 min). */
export const OBSERVED_RENEW_MS = 110_000;

/**
 * Transcripts of observed sessions (SYR-XHAHC): fetched when a tab opens, folded with the
 * same reducer as managed chats, extended by the live events the hub relays. Read-only:
 * these sessions are steered from their own terminal, or through the session strip.
 */
@Injectable({ providedIn: 'root' })
export class ObservedTranscriptStore {
  private readonly api = inject(ApiClientService);
  readonly views = signal<Record<string, ObservedView>>({});

  async load(taskId: string): Promise<void> {
    const prev = this.views()[taskId];
    if (!prev) this.patch(taskId, { loading: true, available: false, reason: '', truncated: false, transcript: emptyTranscript(taskId) });
    try {
      const res = await this.api.getObservedTranscript(taskId);
      const base = emptyTranscript(taskId);
      this.patch(taskId, {
        loading: false,
        available: res.available,
        reason: res.reason ?? '',
        truncated: !!res.truncated,
        transcript: applyEvents(base, res.events ?? []),
      });
    } catch (err) {
      if (!prev?.available) {
        this.patch(taskId, { loading: false, available: false, reason: err instanceof Error ? err.message : 'The transcript could not be loaded', truncated: false, transcript: emptyTranscript(taskId) });
      }
    }
  }

  /** Applies a live push to the tab showing it (ignored when no tab loaded it). */
  onEvents(p: ObservedEventsPush): void {
    if (!p?.task_id || !p.events?.length) return;
    const v = this.views()[p.task_id];
    if (!v?.available) return;
    this.patch(p.task_id, { ...v, transcript: applyEvents(v.transcript, p.events) });
  }

  forget(taskId: string): void {
    const next = { ...this.views() };
    delete next[taskId];
    this.views.set(next);
  }

  private patch(taskId: string, v: ObservedView): void {
    this.views.set({ ...this.views(), [taskId]: v });
  }
}

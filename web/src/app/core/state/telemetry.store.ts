import { Injectable, inject, signal } from '@angular/core';
import { TaskEvent } from '../models/konsert.models';
import { ApiClientService } from '../services/api-client.service';

/** Upper bound on events retained per task (oldest are evicted first). */
export const MAX_EVENTS_PER_TASK = 500;

/**
 * Per-task event timelines. Only tasks that a view has hydrated are tracked; live
 * fleet.update events for other tasks are dropped (they will be fetched on demand).
 */
@Injectable({ providedIn: 'root' })
export class TelemetryStore {
  private readonly api = inject(ApiClientService);

  readonly eventsByTask = signal<Record<string, TaskEvent[]>>({});

  /** Tasks being tracked (hydration started); live events are kept only for these. */
  private readonly tracked = new Set<string>();

  async hydrateTask(taskId: string): Promise<void> {
    if (this.tracked.has(taskId)) return;
    this.tracked.add(taskId);
    try {
      const res = await this.api.getTaskEvents(taskId);
      this.merge(taskId, res.events ?? []);
    } catch {
      this.tracked.delete(taskId);
    }
  }

  appendEvent(ev: TaskEvent): void {
    if (!this.tracked.has(ev.task_id)) return;
    this.eventsByTask.update((map) => {
      const existing = map[ev.task_id] ?? [];
      const last = existing[existing.length - 1];
      // Fast path: ids are monotonic, so a newer event is a plain append.
      if (ev.id > 0 && (!last || (last.id > 0 && ev.id > last.id))) {
        return { ...map, [ev.task_id]: capEvents([...existing, ev]) };
      }
      return { ...map, [ev.task_id]: mergeEvents(existing, [ev]) };
    });
  }

  /**
   * After a WebSocket reconnect, fetch whatever each tracked task emitted while we
   * were disconnected (incrementally via after_id).
   */
  async onReconnected(): Promise<void> {
    await Promise.all(
      Array.from(this.tracked).map(async (taskId) => {
        try {
          const res = await this.api.getTaskEvents(taskId, this.lastEventId(taskId));
          this.merge(taskId, res.events ?? []);
        } catch {
          // Leave the existing timeline; the next reconnect will retry.
        }
      }),
    );
  }

  private lastEventId(taskId: string): number {
    const list = this.eventsByTask()[taskId] ?? [];
    for (let i = list.length - 1; i >= 0; i--) {
      if (list[i].id > 0) return list[i].id;
    }
    return 0;
  }

  private merge(taskId: string, incoming: TaskEvent[]): void {
    if (incoming.length === 0 && this.eventsByTask()[taskId]) return;
    this.eventsByTask.update((map) => ({
      ...map,
      [taskId]: mergeEvents(map[taskId] ?? [], incoming),
    }));
  }
}

function eventKey(e: TaskEvent): string {
  return e.id > 0 ? `id:${e.id}` : `${e.created_at}:${e.kind}:${e.message}`;
}

/** Deduplicating, ordered merge (used for hydration and out-of-order events). */
export function mergeEvents(a: TaskEvent[], b: TaskEvent[]): TaskEvent[] {
  const byKey = new Map<string, TaskEvent>();
  for (const e of a) byKey.set(eventKey(e), e);
  for (const e of b) byKey.set(eventKey(e), e);
  const merged = Array.from(byKey.values()).sort((x, y) => {
    if (x.id && y.id) return x.id - y.id;
    return new Date(x.created_at).getTime() - new Date(y.created_at).getTime();
  });
  return capEvents(merged);
}

function capEvents(list: TaskEvent[]): TaskEvent[] {
  return list.length > MAX_EVENTS_PER_TASK ? list.slice(list.length - MAX_EVENTS_PER_TASK) : list;
}

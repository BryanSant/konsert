import { Injectable, inject, signal } from '@angular/core';
import { TaskEvent } from '../models/konsert.models';
import { ApiClientService } from '../services/api-client.service';

@Injectable({ providedIn: 'root' })
export class TelemetryStore {
  private readonly api = inject(ApiClientService);

  readonly eventsByTask = signal<Record<string, TaskEvent[]>>({});
  private readonly hydrated = new Set<string>();

  async hydrateTask(taskId: string): Promise<void> {
    if (this.hydrated.has(taskId)) return;
    this.hydrated.add(taskId);
    try {
      const res = await this.api.getTaskEvents(taskId);
      const incoming = res.events ?? [];
      this.eventsByTask.update((map) => {
        const existing = map[taskId] ?? [];
        const merged = mergeEvents(existing, incoming);
        return { ...map, [taskId]: merged };
      });
    } catch {
      this.hydrated.delete(taskId);
    }
  }

  appendEvent(ev: TaskEvent): void {
    this.eventsByTask.update((map) => {
      const existing = map[ev.task_id] ?? [];
      return { ...map, [ev.task_id]: mergeEvents(existing, [ev]) };
    });
  }
}

function mergeEvents(a: TaskEvent[], b: TaskEvent[]): TaskEvent[] {
  const byKey = new Map<string, TaskEvent>();
  for (const e of [...a, ...b]) {
    const key = e.id > 0 ? `id:${e.id}` : `${e.created_at}:${e.kind}:${e.message}`;
    byKey.set(key, e);
  }
  return Array.from(byKey.values()).sort((x, y) => {
    if (x.id && y.id) return x.id - y.id;
    return new Date(x.created_at).getTime() - new Date(y.created_at).getTime();
  });
}

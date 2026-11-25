import { Injectable, computed, signal } from '@angular/core';
import { SessionActivity } from '../models/activity.models';
import { activityKey, isNewerActivity } from './activity-logic';

/**
 * Live session activity (thinking / working / speaking / asking / blocked / queued / done /
 * error) from fleet.update kind "activity" and the GET /api/fleet snapshot. Reports are
 * rate-limited by syred and the hub; the newest report per session wins, so a slower
 * snapshot never reverts a live update. Sessions with no report yet have no entry here:
 * WorkStore falls back to a coarse state from the task or chat status.
 */
@Injectable({ providedIn: 'root' })
export class ActivityStore {
  /** Keyed "chat:<id>" / "task:<id>". */
  readonly byKey = signal<Record<string, SessionActivity>>({});

  readonly byChat = computed(() => {
    const out: Record<string, SessionActivity> = {};
    for (const a of Object.values(this.byKey())) if (a.chat_id) out[a.chat_id] = a;
    return out;
  });

  readonly byTask = computed(() => {
    const out: Record<string, SessionActivity> = {};
    for (const a of Object.values(this.byKey())) if (a.task_id) out[a.task_id] = a;
    return out;
  });

  forChat(chatId: string): SessionActivity | undefined {
    return this.byKey()[`chat:${chatId}`];
  }

  forTask(taskId: string): SessionActivity | undefined {
    return this.byKey()[`task:${taskId}`];
  }

  apply(a: SessionActivity): void {
    if (!a?.state || (!a.chat_id && !a.task_id)) return;
    const key = activityKey(a);
    if (!isNewerActivity(this.byKey()[key], a)) return;
    this.byKey.update((m) => ({ ...m, [key]: a }));
  }

  applySnapshot(list: SessionActivity[]): void {
    if (!list?.length) return;
    this.byKey.update((m) => {
      const next = { ...m };
      for (const a of list) {
        if (!a?.state || (!a.chat_id && !a.task_id)) continue;
        const key = activityKey(a);
        if (isNewerActivity(next[key], a)) next[key] = a;
      }
      return next;
    });
  }

  /** Drops a session (its chat was deleted). */
  forget(key: string): void {
    if (!(key in this.byKey())) return;
    this.byKey.update((m) => {
      const next = { ...m };
      delete next[key];
      return next;
    });
  }
}

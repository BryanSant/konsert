import { Injectable, computed, inject } from '@angular/core';
import { ProjectGroup, SessionActivity, WorkItem } from '../models/activity.models';
import { ActivityStore } from './activity.store';
import { activityFromChatStatus, activityFromTaskStatus, groupWork } from './activity-logic';
import { FleetStore } from './fleet.store';
import { OrchestratorStore } from './orchestrator.store';

/**
 * Live-work view model: every task and agent chat grouped by project with counts
 * (running / queued / needs you / blocked / done), each with its live activity and a
 * one-line narration that is never blank (the orchestrator's headline and "now" lines,
 * else facts: the live tool label, task title, ticket title). Data only; the Work and
 * Command modes render it.
 */
@Injectable({ providedIn: 'root' })
export class WorkStore {
  private readonly fleet = inject(FleetStore);
  private readonly activity = inject(ActivityStore);
  private readonly orch = inject(OrchestratorStore);

  readonly projectGroups = computed<ProjectGroup[]>(() => {
    const byKey = this.activity.byKey();
    const tax = { projects: this.fleet.projects(), repos: this.fleet.repos() };
    return groupWork({
      ...tax,
      tasks: this.fleet.tasks(),
      chats: this.orch.sessions(),
      activityOf: (key) => byKey[key],
    });
  });

  /** Every work item by "task:<id>" / "chat:<id>". */
  readonly itemsByKey = computed(() => {
    const out: Record<string, WorkItem> = {};
    for (const g of this.projectGroups()) for (const it of g.items) out[`${it.kind}:${it.id}`] = it;
    return out;
  });

  /** Fleet-wide counts across all groups. */
  readonly totals = computed(() => {
    const t = { running: 0, queued: 0, needs_you: 0, blocked: 0, done: 0, failed: 0 };
    for (const g of this.projectGroups()) {
      for (const k of Object.keys(t) as (keyof typeof t)[]) t[k] += g.counts[k];
    }
    return t;
  });

  /** A task's activity: the reported one, else a coarse one from its status. */
  taskActivity(taskId: string): SessionActivity | undefined {
    const a = this.activity.forTask(taskId);
    if (a) return a;
    const t = this.fleet.tasksById()[taskId];
    return t ? activityFromTaskStatus(t) : undefined;
  }

  /** A chat's activity: the reported one, else a coarse one from its status pill. */
  chatActivity(chatId: string): SessionActivity | undefined {
    const a = this.activity.forChat(chatId);
    if (a) return a;
    const c = this.orch.sessions().find((s) => s.id === chatId);
    return c ? activityFromChatStatus(c) : undefined;
  }
}

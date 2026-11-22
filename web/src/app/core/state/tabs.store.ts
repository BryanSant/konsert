import { Injectable, computed, signal } from '@angular/core';
import { Task, WorkspaceTab } from '../models/konsert.models';
import { basenamePath } from './fleet.store';

export const ORCHESTRATOR_TAB_ID = 'orchestrator';

@Injectable({ providedIn: 'root' })
export class TabsStore {
  readonly openTabs = signal<WorkspaceTab[]>([
    {
      id: ORCHESTRATOR_TAB_ID,
      kind: 'orchestrator',
      title: 'Orchestrator',
      pinned: true,
      interactiveMode: false,
    },
  ]);

  readonly activeTabId = signal<string>(ORCHESTRATOR_TAB_ID);

  // Floating Scratch Workspace state (persists remote PTY even while minimized)
  readonly scratchOpen = signal(false);
  readonly scratchTaskId = signal<string | null>(null);

  readonly activeTab = computed(() => {
    const id = this.activeTabId();
    return this.openTabs().find((t) => t.id === id) ?? this.openTabs()[0];
  });

  openTaskTab(task: Task, workerName?: string): void {
    const existing = this.openTabs().find((t) => t.id === task.id);
    if (!existing) {
      const dir = basenamePath(task.effective_dir || task.workdir);
      const label = dir && dir !== task.agent ? `${dir} · ${task.agent}` : workerName ? `${workerName} · ${task.agent}` : task.agent;
      // Direct Mode & Scratch sessions default to Interactive; Orchestrated runs default to Read-Only.
      const defaultInteractive = task.origin === 'direct' || task.origin === 'scratch';
      this.openTabs.update((tabs) => [
        ...tabs,
        {
          id: task.id,
          kind: 'terminal',
          title: label,
          pinned: false,
          taskId: task.id,
          workerId: task.worker_id,
          agent: task.agent,
          origin: task.origin || 'orchestrator',
          interactiveMode: defaultInteractive,
        },
      ]);
    }
    this.activeTabId.set(task.id);
  }

  selectTab(id: string): void {
    if (this.openTabs().some((t) => t.id === id)) {
      this.activeTabId.set(id);
    }
  }

  closeTab(id: string): void {
    const tab = this.openTabs().find((t) => t.id === id);
    if (!tab || tab.pinned) return;

    const current = this.openTabs();
    const idx = current.findIndex((t) => t.id === id);
    const next = current.filter((t) => t.id !== id);
    this.openTabs.set(next);

    if (this.activeTabId() === id) {
      const fallback = next[Math.max(0, idx - 1)] ?? next[0];
      this.activeTabId.set(fallback.id);
    }
  }

  toggleInteractiveMode(tabId: string): void {
    this.openTabs.update((tabs) =>
      tabs.map((t) => (t.id === tabId ? { ...t, interactiveMode: !t.interactiveMode } : t))
    );
  }

  setInteractiveMode(tabId: string, enabled: boolean): void {
    this.openTabs.update((tabs) =>
      tabs.map((t) => (t.id === tabId ? { ...t, interactiveMode: enabled } : t))
    );
  }

  toggleScratch(): void {
    this.scratchOpen.update((v) => !v);
  }
}

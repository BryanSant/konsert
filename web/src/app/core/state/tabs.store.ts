import { Injectable, computed, signal } from '@angular/core';
import { Space, Task, WorkspaceTab } from '../models/konsert.models';
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
  readonly activeSpaceId = signal<string | null>(null);

  // Floating Scratch Workspace state (persists remote PTY even while minimized)
  readonly scratchOpen = signal(false);
  readonly scratchTaskId = signal<string | null>(null);

  readonly activeTab = computed(() => {
    const id = this.activeTabId();
    return this.openTabs().find((t) => t.id === id) ?? this.openTabs()[0];
  });

  /** Tabs displayed in the center strip: scoped to active Space when one is selected, otherwise all open tabs. */
  readonly visibleTabs = computed(() => {
    const spId = this.activeSpaceId();
    const all = this.openTabs();
    if (!spId) return all;
    return all.filter((t) => t.pinned || t.spaceId === spId);
  });

  selectSpace(space: Space, existingTasksInSpace: Task[], workerName?: string): void {
    this.activeSpaceId.set(space.id);
    // Ensure active tasks belonging to this space are opened as tabs in this space.
    for (const t of existingTasksInSpace) {
      if (!this.openTabs().some((tab) => tab.id === t.id)) {
        this.openTaskTab(t, workerName, false);
      }
    }
    const spaceTabs = this.openTabs().filter((t) => t.spaceId === space.id);
    if (spaceTabs.length > 0) {
      this.activeTabId.set(spaceTabs[0].id);
    }
  }

  openTaskTab(task: Task, workerName?: string, activate = true): void {
    const existing = this.openTabs().find((t) => t.id === task.id);
    if (!existing) {
      const dir = basenamePath(task.effective_dir || task.workdir);
      const label = task.space_id
        ? task.agent
        : dir && dir !== task.agent
          ? `${dir} · ${task.agent}`
          : workerName
            ? `${workerName} · ${task.agent}`
            : task.agent;
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
          spaceId: task.space_id || undefined,
          agent: task.agent,
          origin: task.origin || 'orchestrator',
          interactiveMode: defaultInteractive,
        },
      ]);
    }
    if (activate) {
      this.activeSpaceId.set(task.space_id || null);
      this.activeTabId.set(task.id);
    }
  }

  selectTab(id: string): void {
    const tab = this.openTabs().find((t) => t.id === id);
    if (tab) {
      if (tab.id === ORCHESTRATOR_TAB_ID) {
        this.activeSpaceId.set(null);
      } else if (tab.spaceId) {
        this.activeSpaceId.set(tab.spaceId);
      }
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

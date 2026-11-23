import { Injectable, computed, signal } from '@angular/core';
import { Space, Task, TaskStatus, WorkspaceTab } from '../models/konsert.models';
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

  // Side-by-side split terminal pane state
  readonly splitTabId = signal<string | null>(null);
  readonly splitRatio = signal<number>(0.5);

  // Floating Scratch Workspace state (persists remote PTY even while minimized)
  readonly scratchOpen = signal(false);
  readonly scratchTaskId = signal<string | null>(null);

  readonly activeTab = computed(() => {
    const id = this.activeTabId();
    return this.openTabs().find((t) => t.id === id) ?? this.openTabs()[0];
  });

  readonly splitTab = computed(() => {
    const id = this.splitTabId();
    if (!id || id === this.activeTabId()) return undefined;
    return this.openTabs().find((t) => t.id === id);
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
          model: task.model || undefined,
          sessionKind: task.session_kind || (task.agent === 'shell' ? 'terminal' : 'chat'),
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
        this.splitTabId.set(null);
      } else if (tab.spaceId) {
        this.activeSpaceId.set(tab.spaceId);
      }
      if (this.splitTabId() === id) {
        const prevActive = this.activeTabId();
        this.activeTabId.set(id);
        this.splitTabId.set(prevActive !== ORCHESTRATOR_TAB_ID && prevActive !== id ? prevActive : null);
        return;
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

    if (this.splitTabId() === id) {
      this.splitTabId.set(null);
    }
    if (this.activeTabId() === id) {
      if (this.splitTabId()) {
        const promoted = this.splitTabId()!;
        this.splitTabId.set(null);
        this.activeTabId.set(promoted);
      } else {
        const fallback = next[Math.max(0, idx - 1)] ?? next[0];
        this.activeTabId.set(fallback.id);
      }
    }
  }

  /**
   * Toggles side-by-side split view with another terminal tab.
   * Returns true if split is now active or cleanly closed; returns false if no second terminal tab exists to pair with.
   */
  toggleSplit(candidateTabId?: string): boolean {
    const active = this.activeTabId();
    if (this.splitTabId() && (!candidateTabId || candidateTabId === this.splitTabId())) {
      this.splitTabId.set(null);
      return true;
    }
    const terminals = this.openTabs().filter((t) => t.kind === 'terminal' && t.id !== active);
    const chosen = candidateTabId
      ? this.openTabs().find((t) => t.id === candidateTabId && t.kind === 'terminal' && t.id !== active)
      : terminals[0];
    if (!chosen) {
      return false;
    }
    if (active === ORCHESTRATOR_TAB_ID && terminals.length >= 2) {
      this.activeTabId.set(terminals[0].id);
      this.splitTabId.set(terminals[1].id);
      return true;
    }
    this.splitTabId.set(chosen.id);
    return true;
  }

  closeSplit(): void {
    this.splitTabId.set(null);
  }

  setSplitRatio(ratio: number): void {
    this.splitRatio.set(Math.min(0.75, Math.max(0.25, ratio)));
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

  /** Last observed status per task, used to detect live shell exits. */
  private readonly lastStatus = new Map<string, TaskStatus>();

  /**
   * Closes shell tabs (and the scratch shell) whose shell process exited while
   * we were watching, so typing `exit` closes the tab like a desktop terminal.
   * Only a live transition from an active status counts: opening an
   * already-finished shell to read its output leaves the tab alone. Agent
   * (chat) sessions stay open so their final output and summary remain visible.
   */
  closeExitedShells(tasks: readonly Task[]): void {
    const exited: string[] = [];
    for (const t of tasks) {
      const prev = this.lastStatus.get(t.id);
      this.lastStatus.set(t.id, t.status);
      const isShell = t.agent === 'shell' || t.session_kind === 'terminal';
      if (isShell && prev && ACTIVE_STATUSES.has(prev) && SHELL_EXIT_STATUSES.has(t.status)) {
        exited.push(t.id);
      }
    }
    for (const id of exited) {
      if (this.openTabs().some((tab) => tab.id === id)) {
        this.closeTab(id);
      }
      if (this.scratchTaskId() === id) {
        this.scratchOpen.set(false);
        this.scratchTaskId.set(null);
      }
    }
  }
}

const ACTIVE_STATUSES = new Set<TaskStatus>(['queued', 'assigned', 'running', 'waiting_input']);
// A shell's exit code is its last command's, so `exit` after a failing command reports 'failed'.
const SHELL_EXIT_STATUSES = new Set<TaskStatus>(['completed', 'failed']);

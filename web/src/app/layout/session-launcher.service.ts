import { Injectable, inject } from '@angular/core';
import { Space, Worker } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { FleetStore } from '../core/state/fleet.store';
import { TabsStore } from '../core/state/tabs.store';

const LIVE_STATUSES = new Set(['running', 'waiting_input', 'assigned', 'suspended']);

/** One-click session launches triggered from the shell (no modal involved). */
@Injectable({ providedIn: 'root' })
export class SessionLauncherService {
  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);
  private readonly tabs = inject(TabsStore);

  /** Starts a shell next to the active tab and shows it in the split pane. */
  async openSplitShell(fallbackPath = '~'): Promise<void> {
    const active = this.tabs.activeTab();
    const activeTask = active?.taskId ? this.fleet.tasksById()[active.taskId] : undefined;
    const workerId =
      active?.workerId || activeTask?.worker_id || this.fleet.resolveSessionDefaults().machine_id;
    const repoPath = activeTask?.effective_dir || activeTask?.workdir || fallbackPath || '~';
    if (!workerId) return;
    try {
      const res = await this.api.createSession({
        worker_selector: { worker_id: workerId },
        space_id: active?.spaceId || undefined,
        session_kind: 'terminal',
        agent: 'shell',
        repo_path: repoPath,
        mode: 'interactive',
        origin: 'direct',
        permission_mode: this.fleet.settings().default_permission_mode,
      });
      await this.fleet.refresh();
      const newTask = this.fleet.tasksById()[res.task_id];
      const w = this.fleet.workersById()[workerId];
      if (newTask) {
        this.tabs.openTaskTab(newTask, w?.name, false);
        this.tabs.toggleSplit(newTask.id);
      }
    } catch {
      // ignore: the split button simply does nothing
    }
  }

  /**
   * Activates a Space: shows its live sessions or already-mounted tabs, otherwise
   * starts a fresh interactive shell in it. Returns false when that shell could not be
   * started (the caller can fall back to the session modal).
   */
  async openSpace(space: Space, worker: Worker): Promise<boolean> {
    const spaceTasks = this.fleet
      .tasks()
      .filter((t) => t.space_id === space.id && LIVE_STATUSES.has(t.status));
    if (spaceTasks.length > 0) {
      this.tabs.selectSpace(space, spaceTasks, worker.name);
      return true;
    }
    const existingTabs = this.tabs.openTabs().filter((t) => t.spaceId === space.id);
    if (existingTabs.length > 0) {
      this.tabs.activeSpaceId.set(space.id);
      this.tabs.activeTabId.set(existingTabs[0].id);
      return true;
    }
    this.tabs.activeSpaceId.set(space.id);
    if (worker.status !== 'online') return true;
    try {
      const res = await this.api.dispatchTask({
        space_id: space.id,
        agent: 'shell',
        mode: 'interactive',
        origin: 'direct',
        permission_mode: this.fleet.settings().default_permission_mode,
      });
      await this.fleet.refresh();
      const task = this.fleet.tasksById()[res.task_id];
      if (task) {
        this.tabs.openTaskTab(task, worker.name);
      }
      return true;
    } catch {
      return false;
    }
  }
}

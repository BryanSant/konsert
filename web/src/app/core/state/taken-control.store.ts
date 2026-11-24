import { Injectable, signal } from '@angular/core';
import { Task, TaskStatus } from '../models/konsert.models';

/**
 * Which terminals the user has taken control of (typing goes to the PTY, nothing automatic
 * touches the session). Never persisted: a reload always starts handed back. Also spots
 * shells that exit while watched, so their tab can close like a desktop terminal.
 */
@Injectable({ providedIn: 'root' })
export class TakenControlStore {
  /** task id -> the user types into its PTY. */
  readonly takenControl = signal<Record<string, boolean>>({});

  isInteractive(taskId: string): boolean {
    return !!this.takenControl()[taskId];
  }

  toggleInteractiveMode(taskId: string): void {
    this.setInteractiveMode(taskId, !this.isInteractive(taskId));
  }

  setInteractiveMode(taskId: string, enabled: boolean): void {
    this.takenControl.update((m) => {
      const next = { ...m };
      if (enabled) next[taskId] = true;
      else delete next[taskId];
      return next;
    });
  }

  /** Last observed status per task, to detect live shell exits. */
  private readonly lastStatus = new Map<string, TaskStatus>();

  /**
   * Shells whose process exited while we were watching (a live transition from an active
   * status; opening an already-finished shell doesn't count). Their taken control is
   * dropped; the caller closes their tab. Agent sessions never count: their final output
   * stays readable.
   */
  exitedShells(tasks: readonly Task[]): string[] {
    const exited: string[] = [];
    for (const t of tasks) {
      const prev = this.lastStatus.get(t.id);
      this.lastStatus.set(t.id, t.status);
      const isShell = t.agent === 'shell' || t.session_kind === 'terminal';
      if (isShell && prev && ACTIVE_STATUSES.has(prev) && SHELL_EXIT_STATUSES.has(t.status)) exited.push(t.id);
    }
    for (const id of exited) if (this.isInteractive(id)) this.setInteractiveMode(id, false);
    return exited;
  }
}

const ACTIVE_STATUSES = new Set<TaskStatus>(['queued', 'assigned', 'running', 'waiting_input', 'suspended']);
// A shell's exit code is its last command's, so `exit` after a failing command reports 'failed'.
const SHELL_EXIT_STATUSES = new Set<TaskStatus>(['completed', 'failed']);

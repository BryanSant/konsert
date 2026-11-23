import { Injectable, inject } from '@angular/core';
import { Router } from '@angular/router';
import { Task } from '../models/konsert.models';
import { OrchestratorStore } from '../state/orchestrator.store';

/**
 * Opening a session from anywhere (a notification, the audit log, Your actions, a dialog):
 * every session is a tab in Command at /command/<sid>; orchestrator conversations open in
 * Command's pinned orchestrator tab.
 */
@Injectable({ providedIn: 'root' })
export class SessionNavService {
  private readonly router = inject(Router);
  private readonly orch = inject(OrchestratorStore);

  openTask(task: Pick<Task, 'sid'>): void {
    if (task.sid) void this.router.navigate(['/command', task.sid]);
  }

  openChat(chatId: string): void {
    const chat = this.orch.sessions().find((s) => s.id === chatId);
    if (chat && !chat.fleet_tools && chat.sid) {
      void this.router.navigate(['/command', chat.sid]);
      return;
    }
    void this.router.navigate(['/command']);
    void this.orch.selectSession(chatId);
  }

  /** The session is on screen (its Command tab is the current route). */
  isOpen(sid: string): boolean {
    return !!sid && this.router.url.split(/[?#]/)[0].endsWith(`/${sid}`);
  }
}

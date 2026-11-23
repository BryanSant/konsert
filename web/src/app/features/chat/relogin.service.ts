import { Injectable, inject, signal } from '@angular/core';
import { AgentRelogin } from '../../core/models/agent-chat.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { FleetStore } from '../../core/state/fleet.store';
import { SessionNavService } from '../../core/services/session-nav.service';

/** What the footer alert / chip asks the shell to show (the auth help dialog). */
export interface ReloginRequest {
  workerId: string;
  accountId: string;
  label: string;
  relogin?: AgentRelogin;
}

/** Shell-level helpers for re-authenticating an agent account on its host. */
@Injectable({ providedIn: 'root' })
export class ReloginService {
  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);
  private readonly nav = inject(SessionNavService);

  /** Set to open the auth help dialog (footer alert, account chip). */
  readonly request = signal<ReloginRequest | null>(null);

  show(req: ReloginRequest): void {
    this.request.set(req);
  }

  /** The command line to run, with the account's profile env (never secrets). */
  commandLine(relogin: AgentRelogin | undefined): string {
    if (!relogin?.command) return '';
    const env = Object.entries(relogin.env ?? {})
      .map(([k, v]) => `${k}=${/[\s"'$]/.test(v) ? JSON.stringify(v) : v}`)
      .join(' ');
    return env ? `${env} ${relogin.command}` : relogin.command;
  }

  /**
   * Opens a shell on the account's host with the relogin command (session dispatch with
   * agent "shell"; the hub applies the account env via account_id).
   */
  async openTerminal(workerId: string, accountId: string, relogin: AgentRelogin | undefined): Promise<void> {
    const res = await this.api.createSession({
      worker_selector: { worker_id: workerId },
      session_kind: 'terminal',
      agent: 'shell',
      account_id: accountId || undefined,
      prompt: relogin?.command ?? '',
      repo_path: '~',
      mode: 'interactive',
      origin: 'direct',
      permission_mode: this.fleet.settings().default_permission_mode,
    });
    await this.fleet.refresh();
    const task = this.fleet.tasksById()[res.task_id];
    if (task) this.nav.openTask(task);
    // The terminal tab now shows the sign-in flow; don't leave the dialog covering it.
    this.request.set(null);
  }
}

import { ChangeDetectionStrategy, Component, computed, inject } from '@angular/core';
import { AgentAccountsStore } from '../../core/state/agent-accounts.store';
import { FleetStore } from '../../core/state/fleet.store';
import { IconComponent } from '../../shared/icon.component';
import { ModalDialogComponent } from '../../shared/modal-dialog.component';
import { ReloginHelpComponent } from './relogin-help.component';
import { ReloginService } from './relogin.service';

/** "Claude 2 needs you to sign in again" dialog (from the footer alert or account chip). */
@Component({
  selector: 'sy-relogin-dialog',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [ModalDialogComponent, ReloginHelpComponent, IconComponent],
  template: `
    @if (svc.request(); as req) {
      <app-modal-dialog
        labelledBy="sy-relogin-title"
        panelClass="w-full max-w-md rounded-[var(--sy-radius-xl)] bg-[var(--sy-surface-2)] p-5 space-y-4 shadow-[var(--sy-shadow-popover)]"
        (close)="close()"
      >
        <div class="flex items-start gap-3">
          <span class="mt-0.5 text-[var(--sy-red)]"><app-icon name="key" [size]="16" /></span>
          <div class="flex-1 min-w-0">
            <h2 id="sy-relogin-title" class="text-[14px] font-semibold text-[var(--sy-text)]">{{ req.label }} needs you to sign in again</h2>
            <p class="text-[12px] text-[var(--sy-text-secondary)]">On {{ hostName() }}. Chats with this account are blocked until it is signed in.</p>
          </div>
          <button type="button" class="sy-icon-btn sy-icon-btn-sm" aria-label="Close" (click)="close()">
            <app-icon name="close" [size]="13" />
          </button>
        </div>
        <sy-relogin-help [relogin]="req.relogin" [workerId]="req.workerId" [accountId]="req.accountId" />
        <div class="flex justify-end gap-1.5 pt-1">
          <button type="button" class="sy-btn sy-btn-sm" [disabled]="accounts.refreshing()" (click)="recheck()">
            <app-icon name="refresh" [size]="12" />
            {{ accounts.refreshing() ? 'Checking…' : "I've signed in, check again" }}
          </button>
        </div>
      </app-modal-dialog>
    }
  `,
})
export class ReloginDialogComponent {
  readonly svc = inject(ReloginService);
  readonly accounts = inject(AgentAccountsStore);
  private readonly fleet = inject(FleetStore);
  readonly hostName = computed(() => {
    const r = this.svc.request();
    return (r && this.fleet.workersById()[r.workerId]?.name) || 'its machine';
  });

  close(): void {
    this.svc.request.set(null);
  }

  async recheck(): Promise<void> {
    const r = this.svc.request();
    if (!r) return;
    await this.accounts.refresh(r.workerId, r.accountId);
    const acct = this.accounts.find(r.workerId, r.accountId);
    if (acct && acct.auth?.state !== 'logged_out') this.close();
  }
}

import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { ControlStore } from '../../core/state/control.store';
import { FleetStore } from '../../core/state/fleet.store';
import { IconComponent } from '../../shared/icon.component';
import { ModalDialogComponent } from '../../shared/modal-dialog.component';

/**
 * The phone's emergency brake: a small, quiet "Emergency: pause everything" link at the
 * bottom of the inbox, behind a confirmation sheet whose default is Cancel. Like an
 * emergency number: comforting to see, rarely used. Resume needs no confirmation.
 */
@Component({
  selector: 'ks-mobile-pause',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, ModalDialogComponent],
  host: { class: 'block' },
  template: `
    @if (control.paused()) {
      <button type="button" class="ks-btn w-full h-[44px] text-[var(--ks-violet)] bg-[var(--ks-violet-soft)]" data-testid="mobile-resume" [disabled]="control.isBusy('fleet')" (click)="control.resumeEverything()">
        <app-icon name="play" [size]="12" />
        Resume everything
      </button>
    } @else {
      <button type="button" class="w-full h-[44px] bg-transparent border-0 text-[12.5px] font-medium text-[var(--ks-text-muted)] cursor-pointer rounded-[var(--ks-radius-md)] ks-focusable" data-testid="mobile-pause" (click)="confirming.set(true)">
        ❚❚ Emergency: pause everything
      </button>
    }
    @if (confirming()) {
      <app-modal-dialog labelledBy="ks-mpause-title" describedBy="ks-mpause-body" panelClass="!fixed !inset-x-0 !bottom-0 w-full rounded-t-[22px] bg-[var(--ks-surface-2)] p-5 pb-7" (close)="confirming.set(false)">
        <div class="mx-auto mb-3 h-1 w-10 rounded-full bg-white/20" aria-hidden="true"></div>
        <h2 id="ks-mpause-title" class="m-0 text-[16px] font-semibold text-[var(--ks-text)]">Pause everything?</h2>
        <p id="ks-mpause-body" class="mt-1.5 mb-0 text-[13.5px] leading-relaxed text-[var(--ks-text-secondary)]">
          Suspends all agent work on {{ machines() }} and stops dispatching until you resume. Nothing is lost, and agents are told you paused them.
        </p>
        <div class="grid gap-2 mt-4">
          <button type="button" class="ks-btn h-[44px] bg-[var(--ks-red)] !text-[#1e0b0b] font-semibold" data-testid="mobile-pause-confirm" (click)="confirm()">Pause everything</button>
          <button type="button" class="ks-btn h-[44px] ks-btn-soft" data-testid="mobile-pause-cancel" autofocus (click)="confirming.set(false)">Cancel</button>
        </div>
      </app-modal-dialog>
    }
  `,
})
export class MobilePauseComponent {
  readonly control = inject(ControlStore);
  private readonly fleet = inject(FleetStore);
  readonly confirming = signal(false);
  readonly machines = computed(() => {
    const n = this.fleet.onlineWorkersCount();
    return n === 1 ? '1 machine' : `${n} machines`;
  });

  async confirm(): Promise<void> {
    this.confirming.set(false);
    await this.control.pauseEverything();
  }
}

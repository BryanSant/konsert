import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { ControlStore } from '../../core/state/control.store';
import { FleetStore } from '../../core/state/fleet.store';
import { ModalDialogComponent } from '../../shared/modal-dialog.component';
import { IconComponent } from '../../shared/icon.component';
import { TooltipDirective } from '../../shared/tooltip.directive';

/** "10:42" today, "Nov 3 10:42" otherwise. */
export function shortWhen(iso: string | undefined): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '';
  const time = d.toLocaleTimeString([], { hour12: false, hour: '2-digit', minute: '2-digit' });
  return d.toDateString() === new Date().toDateString()
    ? time
    : `${d.toLocaleDateString([], { month: 'short', day: 'numeric' })} ${time}`;
}

/**
 * Fleet-wide Pause everything: an emergency control that stays visible in the top bar but
 * is deliberately quiet, and asks for confirmation (Cancel is the default) because it is
 * almost never what the user wants. It stops dispatching and suspends running managed work.
 * While paused it turns into Resume, which needs no confirmation.
 */
@Component({
  selector: 'sy-pause-everything',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective, ModalDialogComponent],
  host: { class: 'inline-flex' },
  template: `
    @if (control.paused()) {
      <button
        type="button"
        class="sy-btn sy-btn-sm text-[var(--sy-violet)] bg-[var(--sy-violet-soft)]"
        data-testid="resume-everything"
        [disabled]="control.isBusy('fleet')"
        (click)="control.resumeEverything()"
        syTooltip="Resume what Pause everything suspended and start dispatching again"
        aria-label="Resume everything"
      >
        <app-icon name="play" [size]="11" />
        <span class="hidden sm:inline">Resume</span>
      </button>
    } @else {
      <button
        type="button"
        class="sy-btn sy-btn-sm text-[var(--sy-text-muted)]"
        data-testid="pause-everything"
        [disabled]="control.isBusy('fleet')"
        (click)="confirming.set(true)"
        syTooltip="Emergency stop: suspend all agent work and stop dispatching (asks first)"
        aria-label="Pause everything"
      >
        <app-icon name="pause" [size]="11" />
        <span class="hidden lg:inline">Pause all</span>
      </button>
    }
    @if (confirming()) {
      <app-modal-dialog labelledBy="pause-confirm-title" describedBy="pause-confirm-body" panelClass="w-full max-w-[420px] p-5" (close)="confirming.set(false)">
        <h2 id="pause-confirm-title" class="text-[15px] font-semibold text-[var(--sy-text)] m-0">Pause everything?</h2>
        <p id="pause-confirm-body" class="mt-2 mb-0 text-[13px] leading-relaxed text-[var(--sy-text-secondary)]">
          This suspends every running agent session and terminal on {{ machines() }} and stops new work
          from being dispatched until you press Resume. Nothing is lost, and agents are told you paused them.
        </p>
        <div class="mt-5 flex justify-end gap-2">
          <button type="button" class="sy-btn" data-testid="pause-cancel" autofocus (click)="confirming.set(false)">Cancel</button>
          <button type="button" class="sy-btn sy-btn-danger" data-testid="pause-confirm" (click)="confirm()">Pause everything</button>
        </div>
      </app-modal-dialog>
    }
  `,
})
export class PauseEverythingComponent {
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

/** Banner shown under the top bar while the fleet is paused. */
@Component({
  selector: 'sy-paused-banner',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent],
  template: `
    @if (control.paused()) {
      <div
        role="status"
        data-testid="paused-banner"
        class="flex items-center gap-2 px-3.5 py-1.5 bg-[var(--sy-violet-soft)] text-[12px] text-[var(--sy-violet)] shrink-0"
      >
        <app-icon name="pause" [size]="12" />
        <span class="flex-1 min-w-0 truncate">
          <strong class="font-semibold">Everything is paused.</strong>
          No new work is dispatched and running sessions are suspended{{ who() }}.
        </span>
        <button
          type="button"
          class="sy-btn sy-btn-sm sy-btn-soft"
          [disabled]="control.isBusy('fleet')"
          (click)="control.resumeEverything()"
        >
          <app-icon name="play" [size]="10" />
          Resume
        </button>
      </div>
    }
    @if (control.error(); as err) {
      <div
        role="alert"
        class="flex items-center gap-2 px-3.5 py-1.5 bg-[var(--sy-red-soft)] text-[12px] text-[#f6c0c0] shrink-0"
        data-testid="control-error"
      >
        <app-icon name="alert" [size]="12" />
        <span class="flex-1 min-w-0 truncate">{{ err }}</span>
        <button
          type="button"
          class="sy-icon-btn sy-icon-btn-sm"
          aria-label="Dismiss"
          (click)="control.error.set('')"
        >
          <app-icon name="close" [size]="11" />
        </button>
      </div>
    }
  `,
})
export class PausedBannerComponent {
  readonly control = inject(ControlStore);
  readonly who = computed(() => {
    const p = this.control.pause();
    const by = p.paused_by ? ` by ${p.paused_by}` : '';
    const at = shortWhen(p.paused_at);
    return `${by}${at ? ` at ${at}` : ''}`;
  });
}

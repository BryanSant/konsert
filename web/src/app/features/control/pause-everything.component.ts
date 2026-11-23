import { ChangeDetectionStrategy, Component, computed, inject } from '@angular/core';
import { ControlStore } from '../../core/state/control.store';
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
 * Fleet-wide Pause everything, always one click away in the top bar: stops dispatching and
 * suspends running managed work. While paused it turns into Resume.
 */
@Component({
  selector: 'ks-pause-everything',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective],
  host: { class: 'inline-flex' },
  template: `
    @if (control.paused()) {
      <button
        type="button"
        class="ks-btn ks-btn-sm text-[var(--ks-violet)] bg-[var(--ks-violet-soft)]"
        data-testid="resume-everything"
        [disabled]="control.isBusy('fleet')"
        (click)="control.resumeEverything()"
        ksTooltip="Resume what Pause everything suspended and start dispatching again"
        aria-label="Resume everything"
      >
        <app-icon name="play" [size]="11" />
        <span class="hidden sm:inline">Resume</span>
      </button>
    } @else {
      <button
        type="button"
        class="ks-btn ks-btn-sm"
        data-testid="pause-everything"
        [disabled]="control.isBusy('fleet')"
        (click)="control.pauseEverything()"
        ksTooltip="Stop dispatching and suspend all running managed work"
        aria-label="Pause everything"
      >
        <app-icon name="pause" [size]="11" />
        <span class="hidden sm:inline">Pause everything</span>
      </button>
    }
  `,
})
export class PauseEverythingComponent {
  readonly control = inject(ControlStore);
}

/** Banner shown under the top bar while the fleet is paused. */
@Component({
  selector: 'ks-paused-banner',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent],
  template: `
    @if (control.paused()) {
      <div
        role="status"
        data-testid="paused-banner"
        class="flex items-center gap-2 px-3.5 py-1.5 bg-[var(--ks-violet-soft)] text-[12px] text-[var(--ks-violet)] shrink-0"
      >
        <app-icon name="pause" [size]="12" />
        <span class="flex-1 min-w-0 truncate">
          <strong class="font-semibold">Everything is paused.</strong>
          No new work is dispatched and running sessions are suspended{{ who() }}.
        </span>
        <button
          type="button"
          class="ks-btn ks-btn-sm ks-btn-soft"
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
        class="flex items-center gap-2 px-3.5 py-1.5 bg-[var(--ks-red-soft)] text-[12px] text-[#f6c0c0] shrink-0"
        data-testid="control-error"
      >
        <app-icon name="alert" [size]="12" />
        <span class="flex-1 min-w-0 truncate">{{ err }}</span>
        <button
          type="button"
          class="ks-icon-btn ks-icon-btn-sm"
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

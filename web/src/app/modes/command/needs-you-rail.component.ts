import { ChangeDetectionStrategy, Component, DestroyRef, inject } from '@angular/core';
import { ControlStore } from '../../core/state/control.store';
import { NeedsYouStore } from '../../core/state/needs-you.store';
import { YourActionsComponent } from '../../features/control/your-actions.component';
import { NeedsYouItemComponent } from '../../features/needs-you/needs-you-item.component';
import { IconComponent } from '../../shared/icon.component';
import { TooltipDirective } from '../../shared/tooltip.directive';
import { ModeUiStore } from '../mode-ui.store';

/**
 * Command's right rail, the one Needs-you area on the page: every question, agent request,
 * override request, veto awaiting the agent's acknowledgement and review as the same compact item, each opening the Needs-you
 * dialog. Below: the holds the user placed and "Your actions".
 */
@Component({
  selector: 'ks-needs-you-rail',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective, NeedsYouItemComponent, YourActionsComponent],
  host: { class: 'flex flex-col min-h-0' },
  template: `
    <div class="flex items-center justify-between gap-2 px-4 pt-3 pb-2 shrink-0">
      <h2
        id="ks-needs-you-heading"
        class="m-0 text-[11.5px] font-semibold uppercase tracking-wider"
        [class.text-[var(--ks-amber)]]="store.count() > 0"
        [class.text-[var(--ks-text-secondary)]]="store.count() === 0"
        data-testid="rail-heading"
      >
        Needs you@if (!ui.firstRun()) { · {{ store.count() }} }
      </h2>
      @if (store.count() > 0) {
        <button type="button" class="ks-btn ks-btn-sm text-[var(--ks-blue)]" data-testid="rail-open-all" (click)="store.open()">Go through all</button>
      }
    </div>

    <div class="flex-1 min-h-0 overflow-y-auto px-3 pb-4" role="region" aria-labelledby="ks-needs-you-heading" data-testid="needs-you-rail">
      @if (ui.firstRun()) {
        <p class="px-1 text-[13px] leading-relaxed text-[var(--ks-text-secondary)]">
          When an agent is blocked on a decision, its question lands here with its options and an “Ask about this” button, so you can find out what
          you're agreeing to before you answer.
        </p>
      } @else {
        @if (store.empty()) {
          <p class="px-1 text-[12.5px] leading-relaxed text-[var(--ks-text-muted)]" data-testid="rail-empty">
            Nothing needs you. Questions, requests and override requests from agents, vetoes and finished work to review land here.
          </p>
        }
        <ul class="m-0 p-0 list-none space-y-1.5">
          @for (e of store.entries(); track e.key) {
            <li><ks-needs-you-item [entry]="e" /></li>
          }
        </ul>

        @if (store.holds().length) {
          <h3 class="m-0 pt-4 pb-1.5 px-1 text-[11px] font-semibold uppercase tracking-wider text-[var(--ks-text-muted)]">Your holds · {{ store.holds().length }}</h3>
          <ul class="m-0 p-0 list-none space-y-1.5">
            @for (h of store.holds(); track h.id) {
              <li class="flex items-start gap-2 rounded-[var(--ks-radius-md)] px-2.5 py-2 text-[12px] bg-[var(--ks-raised)]" [attr.data-hold]="h.sid">
                <app-icon name="lock" [size]="12" />
                <span class="flex-1 min-w-0 break-words text-[var(--ks-text-secondary)]">{{ h.summary }}</span>
                <button type="button" class="ks-btn ks-btn-sm shrink-0" [attr.data-testid]="'hold-release-' + h.sid" [disabled]="control.isBusy(h.id)" (click)="control.releaseHold(h)" ksTooltip="Allow automatic actions here again">Release</button>
              </li>
            }
          </ul>
        }

        <div class="pt-4 px-1">
          <ks-your-actions [limit]="6" />
          <button type="button" class="ks-btn ks-btn-sm mt-1 text-[var(--ks-blue)]" data-testid="open-audit" (click)="ui.auditOpen.set(true)">Full audit log</button>
        </div>
      }
    </div>
  `,
})
export class NeedsYouRailComponent {
  readonly store = inject(NeedsYouStore);
  readonly control = inject(ControlStore);
  readonly ui = inject(ModeUiStore);

  constructor() {
    // Keep "blocked 12m" and veto escalation current while the rail is on screen.
    const timer = setInterval(() => this.store.now.set(Date.now()), 30_000);
    inject(DestroyRef).onDestroy(() => clearInterval(timer));
  }
}

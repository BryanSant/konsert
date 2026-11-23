import { ChangeDetectionStrategy, Component, DestroyRef, computed, inject, signal } from '@angular/core';
import { RouterLink } from '@angular/router';
import { ControlStore } from '../../core/state/control.store';
import { NeedsYouStore } from '../../core/state/needs-you.store';
import { WorkItem } from '../../core/state/work-logic';
import { WorkStore } from '../../core/state/work.store';
import { PausedBannerComponent } from '../../features/control/pause-everything.component';
import { YourActionsComponent } from '../../features/control/your-actions.component';
import { IconComponent } from '../../shared/icon.component';
import { ControlLabelComponent } from '../control-label.component';
import { ModeUiStore } from '../mode-ui.store';
import { MobilePauseComponent } from './mobile-pause.component';
import { NeedsYouItemComponent } from '../../features/needs-you/needs-you-item.component';

/**
 * The phone default: watch and answer. Everything that needs you, a glance at live work
 * and reviews, the orchestrator one tap away, and the quiet emergency brake at the bottom.
 * No board, wall or placement editing on a phone.
 */
@Component({
  selector: 'ks-mobile-inbox',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RouterLink, IconComponent, ControlLabelComponent, MobilePauseComponent, PausedBannerComponent, YourActionsComponent, NeedsYouItemComponent],
  host: { class: 'flex flex-col h-full min-h-0 overflow-y-auto' },
  template: `
    <header class="flex items-center justify-between px-4 pt-4 pb-2.5 shrink-0">
      <h1 class="m-0 text-[16px] font-semibold tracking-tight">konsert</h1>
      <span class="font-mono-ide text-[11px]" data-testid="mobile-fleet-state" [class.text-[var(--ks-violet)]]="control.paused()" [class.text-[var(--ks-text-muted)]]="!control.paused()">
        {{ control.paused() ? 'FLEET: PAUSED' : ui.firstRun() ? 'FLEET: no machines' : 'FLEET: RUNNING' }}
      </span>
    </header>
    <ks-paused-banner />

    <div class="mx-4 mt-1 rounded-[var(--ks-radius-md)] px-3 py-2.5 text-[14px]" [class.bg-[var(--ks-amber-soft)]]="store.count() > 0" [class.bg-[var(--ks-surface-1)]]="store.count() === 0" role="status" data-testid="mobile-banner">
      @if (store.count() > 0) {
        <b>▲ {{ store.count() }} need{{ store.count() === 1 ? 's' : '' }} you</b>
      } @else {
        <span class="text-[var(--ks-text-secondary)]">Nothing needs you</span>
      }
      · {{ work.counts().working }} working
    </div>

    <main class="flex-1 px-4 pb-2">
      @if (ui.firstRun()) {
        <div class="mt-4 rounded-[var(--ks-radius-lg)] bg-[var(--ks-surface-1)] p-4 text-[13.5px] leading-relaxed text-[var(--ks-text-secondary)]">
          Konsert runs coding agents on your own machines. Add a machine to get started; whatever needs you lands here.
          <button type="button" class="ks-btn ks-btn-primary w-full h-[44px] mt-3" (click)="ui.addMachineOpen.set(true)">Add a machine</button>
        </div>
      }

      <h2 class="mt-4 mb-1 text-[12px] font-semibold uppercase tracking-wider text-[var(--ks-text-secondary)]" id="ks-inbox-needs">Needs you</h2>
      <ul class="m-0 p-0 list-none space-y-1.5" aria-labelledby="ks-inbox-needs" data-testid="mobile-needs-you">
        @for (e of store.entries(); track e.key) {
          <li><ks-needs-you-item [entry]="e" /></li>
        } @empty {
          <li class="py-3 text-[13px] text-[var(--ks-text-muted)]">When an agent needs a decision, it shows up here.</li>
        }
      </ul>

      <div class="grid grid-cols-2 gap-2 mt-5" role="group" aria-label="More">
        <button type="button" class="ks-btn ks-btn-soft h-[44px]" [attr.aria-expanded]="open() === 'working'" data-testid="mobile-working" (click)="toggle('working')">Working {{ work.counts().working }}</button>
        <button type="button" class="ks-btn ks-btn-soft h-[44px]" [attr.aria-expanded]="open() === 'review'" (click)="toggle('review')">Review {{ work.counts().review }}</button>
        <button type="button" class="ks-btn ks-btn-soft h-[44px]" [attr.aria-expanded]="open() === 'actions'" (click)="toggle('actions')">Your actions</button>
        <a class="ks-btn ks-btn-soft h-[44px] no-underline" routerLink="/inbox/fleet" data-testid="mobile-fleet-link">Fleet</a>
      </div>

      @if (open() === 'working' || open() === 'review') {
        <ul class="m-0 mt-3 p-0 list-none" [attr.aria-label]="open() === 'working' ? 'Working' : 'Review'">
          @for (it of openList(); track it.id) {
            <li>
              <a class="ks-inbox-row no-underline text-inherit" [routerLink]="['/inbox', it.sid]" [attr.data-work-item]="it.sid">
                <ks-control-label [label]="it.control" />
                <span class="flex-1 min-w-0">
                  <span class="block truncate">{{ it.title }}</span>
                  <span class="block truncate text-[12.5px] text-[var(--ks-text-secondary)]">{{ it.agent }} · {{ it.machine }} · {{ it.now }}</span>
                </span>
                <app-icon name="chevron-right" [size]="14" />
              </a>
            </li>
          } @empty {
            <li class="py-3 text-[13px] text-[var(--ks-text-muted)]">Nothing here.</li>
          }
        </ul>
      } @else if (open() === 'actions') {
        <div class="mt-3"><ks-your-actions [limit]="12" /></div>
      }
    </main>

    <div class="px-4 pt-3 pb-2 shrink-0">
      <a class="ks-btn ks-btn-soft w-full h-[44px] no-underline" routerLink="/command" data-testid="mobile-orchestrator">Tell the orchestrator…</a>
    </div>
    <div class="px-4 pb-4 shrink-0 text-center">
      @if (!ui.firstRun()) {
        <ks-mobile-pause />
      }
    </div>
  `,
  styles: `
    .ks-inbox-row {
      display: flex;
      align-items: center;
      gap: 12px;
      width: 100%;
      min-height: 56px;
      padding: 10px 0;
      border: 0;
      border-bottom: 1px solid var(--ks-hairline);
      background: transparent;
      color: var(--ks-text);
      font: inherit;
      font-size: 14px;
      text-align: left;
      cursor: pointer;
    }
    .ks-inbox-row:focus-visible {
      outline: none;
      box-shadow: var(--ks-focus-ring);
      border-radius: var(--ks-radius-sm);
    }
    .ks-ptag {
      flex-shrink: 0;
      padding: 2px 7px;
      border-radius: 5px;
      font-family: 'JetBrains Mono', ui-monospace, monospace;
      font-size: 11px;
    }
  `,
})
export class MobileInboxComponent {
  readonly store = inject(NeedsYouStore);
  readonly work = inject(WorkStore);
  readonly control = inject(ControlStore);
  readonly ui = inject(ModeUiStore);

  readonly open = signal<'working' | 'review' | 'actions' | ''>('');
  readonly openList = computed<WorkItem[]>(() => (this.open() === 'review' ? this.work.reviews() : this.work.live()));

  constructor() {
    const t = setInterval(() => this.store.now.set(Date.now()), 30_000);
    inject(DestroyRef).onDestroy(() => clearInterval(t));
  }

  toggle(which: 'working' | 'review' | 'actions'): void {
    this.open.set(this.open() === which ? '' : which);
  }

}

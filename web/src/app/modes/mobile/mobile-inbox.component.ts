import { ChangeDetectionStrategy, Component, DestroyRef, computed, inject, signal } from '@angular/core';
import { Router, RouterLink } from '@angular/router';
import { FleetEvent, UserRequest } from '../../core/models/control.models';
import { QuestionView } from '../../core/models/question.models';
import { ControlStore } from '../../core/state/control.store';
import { FleetStore } from '../../core/state/fleet.store';
import { NeedsYouStore } from '../../core/state/needs-you.store';
import { QuestionsStore } from '../../core/state/questions.store';
import { formatSince } from '../../core/state/questions-logic';
import { WorkItem } from '../../core/state/work-logic';
import { WorkStore } from '../../core/state/work.store';
import { PausedBannerComponent } from '../../features/control/pause-everything.component';
import { YourActionsComponent } from '../../features/control/your-actions.component';
import { IconComponent } from '../../shared/icon.component';
import { ControlLabelComponent } from '../control-label.component';
import { ModeUiStore } from '../mode-ui.store';
import { MobilePauseComponent } from './mobile-pause.component';

/**
 * The phone default: watch and answer. Everything that needs you, a glance at live work
 * and reviews, the orchestrator one tap away, and the quiet emergency brake at the bottom.
 * No board, wall or placement editing on a phone.
 */
@Component({
  selector: 'ks-mobile-inbox',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RouterLink, IconComponent, ControlLabelComponent, MobilePauseComponent, PausedBannerComponent, YourActionsComponent],
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
          <button type="button" class="ks-btn ks-btn-primary w-full min-h-11 mt-3" (click)="ui.addMachineOpen.set(true)">Add a machine</button>
        </div>
      }

      <h2 class="mt-4 mb-1 text-[12px] font-semibold uppercase tracking-wider text-[var(--ks-text-secondary)]" id="ks-inbox-needs">Needs you</h2>
      <ul class="m-0 p-0 list-none" aria-labelledby="ks-inbox-needs" data-testid="mobile-needs-you">
        @if (store.count() === 0) {
          <li class="py-3 text-[13px] text-[var(--ks-text-muted)]">When an agent needs a decision, it shows up here.</li>
        }
        @for (v of store.vetoes(); track v.id) {
          <li>
            <button type="button" class="ks-inbox-row" (click)="openVeto(v)">
              <span class="ks-ptag bg-[var(--ks-red)] text-[#1e0b0b]">P1</span>
              <span class="flex-1 min-w-0">
                <span class="block truncate">Veto sent</span>
                <span class="block truncate text-[12.5px] text-[var(--ks-text-secondary)]">{{ vetoLine(v) }}</span>
              </span>
              <app-icon name="chevron-right" [size]="14" />
            </button>
          </li>
        }
        @for (q of store.questions(); track q.id) {
          <li>
            <button type="button" class="ks-inbox-row" [attr.data-question]="q.sid" (click)="openQuestion(q)">
              <span class="ks-ptag bg-[rgba(224,168,76,0.2)] text-[#e9c27f]">P2</span>
              <span class="flex-1 min-w-0">
                <span class="block line-clamp-2">{{ q.prompt }}</span>
                <span class="block truncate text-[12.5px] text-[var(--ks-text-secondary)]">{{ questionLine(q) }}</span>
              </span>
              <app-icon name="chevron-right" [size]="14" />
            </button>
          </li>
        }
        @for (r of store.requests(); track r.id) {
          <li class="py-2.5 border-b border-[var(--ks-hairline)]" [attr.data-request]="r.sid">
            <div class="flex gap-3">
              <span class="ks-ptag bg-[rgba(224,168,76,0.2)] text-[#e9c27f]">P2</span>
              <span class="flex-1 min-w-0 text-[14px]">
                {{ r.description }}
                <span class="block text-[12.5px] text-[var(--ks-text-secondary)]">request · {{ machine(r) }}</span>
              </span>
            </div>
            <div class="grid grid-cols-2 gap-2 mt-2">
              <button type="button" class="ks-btn ks-btn-soft min-h-11" [disabled]="control.isBusy(r.id)" (click)="control.resolveRequest(r, 'done')">Mark done</button>
              <button type="button" class="ks-btn min-h-11" [disabled]="control.isBusy(r.id)" (click)="control.resolveRequest(r, 'declined')">Decline</button>
            </div>
          </li>
        }
        @for (it of store.reviews(); track it.id) {
          <li>
            <a class="ks-inbox-row no-underline text-inherit" [routerLink]="['/inbox', it.sid]">
              <span class="ks-ptag bg-[var(--ks-raised-strong)] text-[var(--ks-text-secondary)]">P3</span>
              <span class="flex-1 min-w-0">
                <span class="block truncate">Ready for review: {{ it.title }}</span>
                <span class="block truncate text-[12.5px] text-[var(--ks-text-secondary)]">{{ it.agent }} · {{ it.machine }}</span>
              </span>
              <app-icon name="chevron-right" [size]="14" />
            </a>
          </li>
        }
      </ul>

      <div class="grid grid-cols-2 gap-2 mt-5" role="group" aria-label="More">
        <button type="button" class="ks-btn ks-btn-soft min-h-11" [attr.aria-expanded]="open() === 'working'" data-testid="mobile-working" (click)="toggle('working')">Working {{ work.counts().working }}</button>
        <button type="button" class="ks-btn ks-btn-soft min-h-11" [attr.aria-expanded]="open() === 'review'" (click)="toggle('review')">Review {{ work.counts().review }}</button>
        <button type="button" class="ks-btn ks-btn-soft min-h-11" [attr.aria-expanded]="open() === 'actions'" (click)="toggle('actions')">Your actions</button>
        <a class="ks-btn ks-btn-soft min-h-11 no-underline" routerLink="/inbox/fleet" data-testid="mobile-fleet-link">Fleet</a>
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
      <a class="ks-btn ks-btn-soft w-full min-h-11 no-underline" routerLink="/command" data-testid="mobile-orchestrator">Tell the orchestrator…</a>
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
  private readonly questions = inject(QuestionsStore);
  private readonly fleet = inject(FleetStore);
  private readonly router = inject(Router);

  readonly open = signal<'working' | 'review' | 'actions' | ''>('');
  readonly openList = computed<WorkItem[]>(() => (this.open() === 'review' ? this.work.reviews() : this.work.live()));

  constructor() {
    const t = setInterval(() => this.store.now.set(Date.now()), 30_000);
    inject(DestroyRef).onDestroy(() => clearInterval(t));
  }

  toggle(which: 'working' | 'review' | 'actions'): void {
    this.open.set(this.open() === which ? '' : which);
  }

  /** The questions flow (full-screen question, side conversation as a bottom sheet). */
  openQuestion(q: QuestionView): void {
    this.questions.open(q.id);
  }

  openVeto(v: FleetEvent): void {
    const id = v.task_id || v.chat_id;
    const it = id ? this.work.byId()[id] : undefined;
    if (it) void this.router.navigate(['/inbox', it.sid]);
    else this.store.dismissVeto(v);
  }

  vetoLine(v: FleetEvent): string {
    const id = v.task_id || v.chat_id;
    const it = id ? this.work.byId()[id] : undefined;
    return [it?.sid, it?.machine, `${formatSince(v.created_at, this.store.now())} ago`, 'enforced'].filter(Boolean).join(' · ');
  }

  questionLine(q: QuestionView): string {
    return [q.orchestrator ? 'orchestrator' : q.agent, q.chat_sid, q.machine_name, formatSince(q.blocked_since, this.store.now())].filter(Boolean).join(' · ');
  }

  machine(r: UserRequest): string {
    return this.fleet.workersById()[r.worker_id]?.name ?? '';
  }
}

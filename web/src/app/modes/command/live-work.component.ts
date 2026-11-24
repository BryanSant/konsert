import { NgTemplateOutlet } from '@angular/common';
import { ChangeDetectionStrategy, Component, DestroyRef, computed, inject, input, output, signal } from '@angular/core';
import { FleetStore } from '../../core/state/fleet.store';
import { NeedsYouStore } from '../../core/state/needs-you.store';
import { WorkItem, startOfToday } from '../../core/state/work-logic';
import { WorkStore } from '../../core/state/work.store';
import { ActivitySpinnerComponent } from '../../shared/activity-spinner.component';
import { IconComponent } from '../../shared/icon.component';
import { TooltipDirective } from '../../shared/tooltip.directive';
import { SpinnerLegendComponent } from '../../shared/spinner-legend.component';
import { SPINNER_LABEL_COLORS, SPINNER_WORDS } from '../../shared/spinner-logic';
import { ModeUiStore } from '../mode-ui.store';
import { StoryLine, StoryProject, buildStory, queuedLabel } from './live-work-logic';

/**
 * Command's left pane, "Live work": the project story. Per project its name, the
 * orchestrator's headline (✦ marks orchestrator-written text), a segmented progress bar,
 * "N in flight on M machines · K queued", the in-flight lines (spinner, title, the 'now'
 * line coloured by state, the agent), the queue collapsed, and done lines fading out.
 * "Other work" holds observed sessions and your terminals. Needs-you items live only in the
 * right rail: an asking line links to the shared Needs-you dialog instead.
 */
@Component({
  selector: 'ks-live-work',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [NgTemplateOutlet, TooltipDirective, IconComponent, ActivitySpinnerComponent, SpinnerLegendComponent],
  host: { class: 'flex flex-col min-h-0' },
  template: `
    <div class="flex items-center justify-between gap-2 px-4 pt-3 pb-2 shrink-0">
      <h2 class="m-0 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--ks-text-secondary)]" id="ks-live-work-heading">Live work</h2>
      @if (!ui.firstRun()) {
        <ks-spinner-legend />
      }
    </div>
    @if (ui.firstRun()) {
      <p class="px-4 text-[13px] leading-relaxed text-[var(--ks-text-secondary)]" data-testid="live-work-empty">
        Every agent session on your machines shows up here, whether the orchestrator started it or you did, grouped by project.
      </p>
    } @else if (story().length === 0) {
      <p class="px-4 text-[12.5px] leading-relaxed text-[var(--ks-text-muted)]" data-testid="live-work-empty">
        Nothing is running. Ask the orchestrator for something, or start a session.
      </p>
      <div class="px-3 pt-2 flex flex-wrap gap-1.5">
        <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" (click)="ui.newSession('chat')"><app-icon name="plus" [size]="12" />Agent chat</button>
        <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" (click)="ui.newSession('terminal')"><app-icon name="shell" [size]="12" />Terminal</button>
      </div>
    } @else {
      <nav class="flex-1 min-h-0 overflow-y-auto px-2 pb-3" aria-labelledby="ks-live-work-heading" data-testid="live-work">
        @for (p of story(); track p.key) {
          <section class="ks-lw-project" [attr.aria-labelledby]="'ks-lw-' + p.key" [attr.data-project]="p.name">
            <div class="px-2 flex flex-col gap-[3px]">
              <div class="flex items-baseline gap-2">
                <h3 class="m-0 text-[13.5px] font-semibold text-[var(--ks-text)] truncate" [id]="'ks-lw-' + p.key">{{ p.name }}</h3>
                @if (p.total) {
                  <span class="ml-auto font-mono-ide text-[11px] text-[var(--ks-text-muted)] shrink-0">{{ p.total }} {{ p.total === 1 ? 'task' : 'tasks' }}</span>
                }
              </div>
              @if (p.headline) {
                <p class="m-0 text-[12.5px] leading-snug text-[#c9cbd1]" data-testid="project-headline">
                  {{ p.headline }}
                  @if (p.headlineByOrchestrator) {
                    <span class="text-[11px] text-[var(--ks-text-muted)]" ksTooltip="Headline written by the orchestrator" aria-hidden="true">✦</span>
                    <span class="ks-sr-only">(written by the orchestrator)</span>
                  }
                </p>
              }
              @if (p.total) {
                <span class="ks-lw-bar" role="img" [attr.aria-label]="barLabel(p)" data-testid="project-bar">
                  @if (p.bar.done) {
                    <span [style.flex-grow]="p.bar.done" class="bg-[var(--ks-green)]"></span>
                  }
                  @if (p.bar.running) {
                    <span [style.flex-grow]="p.bar.running" class="bg-[#c9cbd1]"></span>
                  }
                  @if (p.bar.needsYou) {
                    <span [style.flex-grow]="p.bar.needsYou" class="bg-[var(--ks-amber)]"></span>
                  }
                  @if (p.bar.queued) {
                    <span [style.flex-grow]="p.bar.queued" class="bg-white/[0.12]"></span>
                  }
                </span>
              }
              <span class="text-[11px] text-[var(--ks-text-muted)]" data-testid="project-summary">{{ p.summary }}</span>
            </div>
            <ul class="m-0 mt-1 p-0 list-none flex flex-col gap-px">
              @for (l of p.inFlight; track l.key) {
                <li>
                  <ng-container *ngTemplateOutlet="lineTpl; context: { $implicit: l }" />
                </li>
              }
              @if (p.queued.length) {
                <li>
                  <button
                    type="button"
                    class="ks-focusable ks-lw-more"
                    [attr.aria-expanded]="isExpanded(p.key)"
                    [attr.aria-controls]="'ks-lw-q-' + p.key"
                    data-testid="queued-toggle"
                    (click)="toggle(p.key)"
                  >
                    <span aria-hidden="true">{{ isExpanded(p.key) ? '▾' : '▸' }}</span>
                    <span class="truncate">{{ isExpanded(p.key) ? 'hide queued' : queued(p) }}</span>
                  </button>
                </li>
                @if (isExpanded(p.key)) {
                  <li [id]="'ks-lw-q-' + p.key">
                    <ul class="m-0 p-0 list-none flex flex-col gap-px">
                      @for (l of p.queued; track l.key) {
                        <li><ng-container *ngTemplateOutlet="lineTpl; context: { $implicit: l }" /></li>
                      }
                    </ul>
                  </li>
                }
              }
              @for (l of p.done; track l.key) {
                <li [style.opacity]="l.opacity" data-testid="done-line">
                  <ng-container *ngTemplateOutlet="lineTpl; context: { $implicit: l }" />
                </li>
              }
            </ul>
          </section>
        }
      </nav>
    }

    <ng-template #lineTpl let-l>
      <div class="ks-lw-line" [class.ks-lw-active]="activeId() === l.id" [attr.data-work-item]="l.sid" [attr.data-state]="l.state">
        <button
          type="button"
          class="ks-focusable ks-lw-open"
          [attr.aria-current]="activeId() === l.id ? 'true' : null"
          [disabled]="!l.item"
          (click)="openLine(l)"
        >
          <ks-spinner [state]="l.state" [machines]="l.machines" [coarse]="l.coarse" [tps]="l.tps" />
          <span class="flex-1 min-w-0">
            <span class="block truncate text-[12.5px] text-[var(--ks-text)]">{{ l.title }}</span>
            <span class="block truncate text-[11.5px]" [style.color]="colorOf(l)" [attr.title]="l.nowTitle || null" data-testid="live-now">
              <span class="ks-sr-only">{{ wordOf(l) }}: </span>{{ l.now }}
              @if (l.nowByOrchestrator) {
                <span class="text-[var(--ks-text-muted)]" aria-hidden="true"> ✦</span>
              }
            </span>
          </span>
          @if (l.state !== 'asking') {
            <span class="text-[11px] text-[var(--ks-text-muted)] shrink-0 max-w-[72px] truncate">{{ l.agent }}</span>
          }
        </button>
        @if (l.state === 'asking') {
          <button type="button" class="ks-focusable ks-lw-ask" data-testid="line-needs-you" [attr.aria-label]="'Needs you: ' + l.title" (click)="openNeedsYou(l)">→ Needs you</button>
        }
      </div>
    </ng-template>
  `,
  styles: `
    .ks-lw-project {
      padding: 10px 0 6px;
      border-top: 1px solid rgba(255, 255, 255, 0.05);
    }
    .ks-lw-project:first-child {
      border-top: 0;
      padding-top: 2px;
    }
    .ks-lw-bar {
      display: flex;
      height: 5px;
      margin-top: 3px;
      border-radius: 3px;
      overflow: hidden;
      background: rgba(255, 255, 255, 0.05);
    }
    .ks-lw-bar > span {
      flex-basis: 0;
    }
    .ks-lw-line {
      display: flex;
      align-items: center;
      border-radius: var(--ks-radius-md);
    }
    .ks-lw-line:hover,
    .ks-lw-line.ks-lw-active {
      background: rgba(255, 255, 255, 0.05);
    }
    .ks-lw-line.ks-lw-active {
      background: var(--ks-blue-soft);
    }
    .ks-lw-open {
      flex: 1;
      min-width: 0;
      display: flex;
      align-items: center;
      gap: 10px;
      padding: 6px 8px 6px 10px;
      border: 0;
      border-radius: var(--ks-radius-md);
      background: transparent;
      text-align: left;
      cursor: pointer;
    }
    .ks-lw-open:disabled {
      cursor: default;
    }
    .ks-lw-ask {
      flex: none;
      margin-right: 6px;
      padding: 3px 7px;
      border: 0;
      border-radius: var(--ks-radius-sm);
      background: var(--ks-amber-soft);
      color: #e9c27f;
      font-size: 11.5px;
      white-space: nowrap;
      cursor: pointer;
    }
    .ks-lw-ask:hover {
      background: rgba(224, 168, 76, 0.24);
    }
    .ks-lw-more {
      display: flex;
      align-items: center;
      gap: 6px;
      width: 100%;
      padding: 6px 8px 6px 14px;
      border: 0;
      border-radius: var(--ks-radius-md);
      background: transparent;
      color: var(--ks-text-secondary);
      font-size: 12px;
      text-align: left;
      cursor: pointer;
    }
    .ks-lw-more:hover {
      background: rgba(255, 255, 255, 0.05);
    }
  `,
})
export class LiveWorkComponent {
  readonly activeId = input('');
  readonly open = output<WorkItem>();
  readonly ui = inject(ModeUiStore);
  private readonly work = inject(WorkStore);
  private readonly fleet = inject(FleetStore);
  private readonly needsYou = inject(NeedsYouStore);
  readonly colors = SPINNER_LABEL_COLORS;
  readonly words = SPINNER_WORDS;

  /** Ticks every 20 s so done lines fade and drop out. */
  private readonly now = signal(Date.now());
  private readonly expanded = signal<Record<string, boolean>>({});

  readonly story = computed<StoryProject[]>(() => {
    const now = this.now();
    return buildStory({
      groups: this.work.projectGroups(),
      items: this.work.byId(),
      allItems: this.work.items(),
      workers: this.fleet.workersById(),
      heat: this.fleet.heatByWorker(),
      now,
      today: startOfToday(now),
    });
  });

  constructor() {
    const t = setInterval(() => this.now.set(Date.now()), 20_000);
    inject(DestroyRef).onDestroy(() => clearInterval(t));
  }

  isExpanded(key: string): boolean {
    return !!this.expanded()[key];
  }

  toggle(key: string): void {
    this.expanded.update((m) => ({ ...m, [key]: !m[key] }));
  }

  queued(p: StoryProject): string {
    return queuedLabel(p);
  }

  barLabel(p: StoryProject): string {
    const b = p.bar;
    return `${b.done} done today, ${b.running} running, ${b.needsYou} need you, ${b.queued} queued`;
  }

  colorOf(l: StoryLine): string {
    return this.colors[l.state];
  }

  wordOf(l: StoryLine): string {
    return this.words[l.state];
  }

  openLine(l: StoryLine): void {
    if (l.item) this.open.emit(l.item);
  }

  openNeedsYou(l: StoryLine): void {
    const e = this.needsYou.entryFor(l.id);
    if (e) this.needsYou.open(e.key);
    else this.openLine(l);
  }
}

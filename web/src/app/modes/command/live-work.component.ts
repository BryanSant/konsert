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
 * commander's headline (✦ marks commander-written text), a segmented progress bar,
 * "N in flight on M machines · K queued", the in-flight lines (spinner, title, the 'now'
 * line coloured by state, the agent), the queue collapsed, and done lines fading out.
 * "Other work" holds observed sessions and your terminals. Needs-you items live only in the
 * right rail: an asking line links to the shared Needs-you dialog instead.
 */
@Component({
  selector: 'sy-live-work',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [NgTemplateOutlet, TooltipDirective, IconComponent, ActivitySpinnerComponent, SpinnerLegendComponent],
  host: { class: 'flex flex-col min-h-0' },
  template: `
    <div class="flex items-center justify-between gap-2 px-4 pt-3 pb-2 shrink-0">
      <h2 class="m-0 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--sy-text-secondary)]" id="sy-live-work-heading">Live work</h2>
      @if (!ui.firstRun()) {
        <sy-spinner-legend />
      }
    </div>
    @if (ui.firstRun()) {
      <p class="px-4 text-[13px] leading-relaxed text-[var(--sy-text-secondary)]" data-testid="live-work-empty">
        Every agent session on your machines shows up here, whether the commander started it or you did, grouped by project.
      </p>
    } @else if (story().length === 0) {
      <p class="px-4 text-[12.5px] leading-relaxed text-[var(--sy-text-muted)]" data-testid="live-work-empty">
        Nothing is running. Ask the commander for something, or start a session.
      </p>
      <div class="px-3 pt-2 flex flex-wrap gap-1.5">
        <button type="button" class="sy-btn sy-btn-sm sy-btn-soft" (click)="ui.newSession('chat')"><app-icon name="plus" [size]="12" />Agent chat</button>
        <button type="button" class="sy-btn sy-btn-sm sy-btn-soft" (click)="ui.newSession('terminal')"><app-icon name="shell" [size]="12" />Terminal</button>
      </div>
    } @else {
      <nav class="flex-1 min-h-0 overflow-y-auto px-2 pb-3" aria-labelledby="sy-live-work-heading" data-testid="live-work">
        @for (p of story(); track p.key) {
          <section class="sy-lw-project" [attr.aria-labelledby]="'sy-lw-' + p.key" [attr.data-project]="p.name">
            <div class="px-2 flex flex-col gap-[3px]">
              <div class="flex items-baseline gap-2">
                <h3 class="m-0 text-[13.5px] font-semibold text-[var(--sy-text)] truncate" [id]="'sy-lw-' + p.key">{{ p.name }}</h3>
                @if (p.total) {
                  <span class="ml-auto font-mono-ide text-[11px] text-[var(--sy-text-muted)] shrink-0">{{ p.total }} {{ p.total === 1 ? 'task' : 'tasks' }}</span>
                }
              </div>
              @if (p.headline) {
                <p class="m-0 text-[12.5px] leading-snug text-[#c9cbd1]" data-testid="project-headline">
                  {{ p.headline }}
                  @if (p.headlineByCommander) {
                    <span class="text-[11px] text-[var(--sy-text-muted)]" syTooltip="Headline written by the commander" aria-hidden="true">✦</span>
                    <span class="sy-sr-only">(written by the commander)</span>
                  }
                </p>
              }
              @if (p.total) {
                <span class="sy-lw-bar" role="img" [attr.aria-label]="barLabel(p)" data-testid="project-bar">
                  @if (p.bar.done) {
                    <span [style.flex-grow]="p.bar.done" class="bg-[var(--sy-green)]"></span>
                  }
                  @if (p.bar.running) {
                    <span [style.flex-grow]="p.bar.running" class="bg-[#c9cbd1]"></span>
                  }
                  @if (p.bar.needsYou) {
                    <span [style.flex-grow]="p.bar.needsYou" class="bg-[var(--sy-amber)]"></span>
                  }
                  @if (p.bar.queued) {
                    <span [style.flex-grow]="p.bar.queued" class="bg-white/[0.12]"></span>
                  }
                </span>
              }
              <span class="text-[11px] text-[var(--sy-text-muted)]" data-testid="project-summary">{{ p.summary }}</span>
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
                    class="sy-focusable sy-lw-more"
                    [attr.aria-expanded]="isExpanded(p.key)"
                    [attr.aria-controls]="'sy-lw-q-' + p.key"
                    data-testid="queued-toggle"
                    (click)="toggle(p.key)"
                  >
                    <span aria-hidden="true">{{ isExpanded(p.key) ? '▾' : '▸' }}</span>
                    <span class="truncate">{{ isExpanded(p.key) ? 'hide queued' : queued(p) }}</span>
                  </button>
                </li>
                @if (isExpanded(p.key)) {
                  <li [id]="'sy-lw-q-' + p.key">
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
      <div class="sy-lw-line" [class.sy-lw-active]="activeId() === l.id" [attr.data-work-item]="l.sid" [attr.data-state]="l.state">
        <button
          type="button"
          class="sy-focusable sy-lw-open"
          [attr.aria-current]="activeId() === l.id ? 'true' : null"
          [disabled]="!l.item"
          (click)="openLine(l)"
        >
          <sy-spinner [state]="l.state" [machines]="l.machines" [coarse]="l.coarse" [tps]="l.tps" [detail]="l.detail" />
          <span class="flex-1 min-w-0">
            <span class="block truncate text-[12.5px] text-[var(--sy-text)]">{{ l.title }}</span>
            <span class="block truncate text-[11.5px]" [style.color]="colorOf(l)" [syTooltip]="l.nowTitle" data-testid="live-now">
              <span class="sy-sr-only">{{ wordOf(l) }}: </span>{{ l.now }}
              @if (l.nowByCommander) {
                <span class="text-[var(--sy-text-muted)]" aria-hidden="true"> ✦</span>
              }
            </span>
          </span>
          @if (l.state !== 'asking') {
            <span class="text-[11px] text-[var(--sy-text-muted)] shrink-0 max-w-[72px] truncate">{{ l.agent }}</span>
          }
        </button>
        @if (l.state === 'asking') {
          <button type="button" class="sy-focusable sy-lw-ask" data-testid="line-needs-you" [attr.aria-label]="'Needs you: ' + l.title" (click)="openNeedsYou(l)">→ Needs you</button>
        }
      </div>
    </ng-template>
  `,
  styles: `
    .sy-lw-project {
      padding: 10px 0 6px;
      border-top: 1px solid rgba(255, 255, 255, 0.05);
    }
    .sy-lw-project:first-child {
      border-top: 0;
      padding-top: 2px;
    }
    .sy-lw-bar {
      display: flex;
      height: 5px;
      margin-top: 3px;
      border-radius: 3px;
      overflow: hidden;
      background: rgba(255, 255, 255, 0.05);
    }
    .sy-lw-bar > span {
      flex-basis: 0;
    }
    .sy-lw-line {
      display: flex;
      align-items: center;
      border-radius: var(--sy-radius-md);
    }
    .sy-lw-line:hover,
    .sy-lw-line.sy-lw-active {
      background: rgba(255, 255, 255, 0.05);
    }
    .sy-lw-line.sy-lw-active {
      background: var(--sy-blue-soft);
    }
    .sy-lw-open {
      flex: 1;
      min-width: 0;
      display: flex;
      align-items: center;
      gap: 10px;
      padding: 6px 8px 6px 10px;
      border: 0;
      border-radius: var(--sy-radius-md);
      background: transparent;
      text-align: left;
      cursor: pointer;
    }
    .sy-lw-open:disabled {
      cursor: default;
    }
    .sy-lw-ask {
      flex: none;
      margin-right: 6px;
      padding: 3px 7px;
      border: 0;
      border-radius: var(--sy-radius-sm);
      background: var(--sy-amber-soft);
      color: #e9c27f;
      font-size: 11.5px;
      white-space: nowrap;
      cursor: pointer;
    }
    .sy-lw-ask:hover {
      background: rgba(224, 168, 76, 0.24);
    }
    .sy-lw-more {
      display: flex;
      align-items: center;
      gap: 6px;
      width: 100%;
      padding: 6px 8px 6px 14px;
      border: 0;
      border-radius: var(--sy-radius-md);
      background: transparent;
      color: var(--sy-text-secondary);
      font-size: 12px;
      text-align: left;
      cursor: pointer;
    }
    .sy-lw-more:hover {
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
      terminalActivity: (id) => this.work.taskActivity(id),
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

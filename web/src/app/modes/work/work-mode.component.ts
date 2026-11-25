import { DOCUMENT } from '@angular/common';
import { ChangeDetectionStrategy, Component, DestroyRef, computed, inject, signal } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { ActivatedRoute, Router } from '@angular/router';
import { map } from 'rxjs';
import { WorkColumn, WorkItem } from '../../core/state/work-logic';
import { WorkStore } from '../../core/state/work.store';
import { NeedsYouStore } from '../../core/state/needs-you.store';
import { IconComponent } from '../../shared/icon.component';
import { TooltipDirective } from '../../shared/tooltip.directive';
import { ControlLabelComponent } from '../control-label.component';
import { ModeUiStore } from '../mode-ui.store';
import { isPhoneWidth } from '../../app.routes';
import { ReviewPanelComponent } from './review-panel.component';
import { ItemSpinnerComponent } from '../item-spinner.component';

export const COLUMN_LABELS: Record<WorkColumn, string> = {
  queued: 'Queued',
  running: 'Running',
  needs_you: 'Needs you',
  review: 'Review',
  done: 'Done today',
};

const VIEW_KEY = 'syre.work.view';

/**
 * 2 Work: the flow board. Lanes are projects (plus Expedite), columns are lifecycle
 * states Syre owns, so agents and the scheduler move the cards and the user reviews
 * and decides. ⌘B toggles board and list.
 */
@Component({
  selector: 'sy-work-mode',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [ItemSpinnerComponent, IconComponent, ControlLabelComponent, ReviewPanelComponent, TooltipDirective],
  host: { class: 'flex flex-col h-full min-h-0' },
  template: `
    <div class="flex flex-wrap items-center gap-3 px-4 pt-3 pb-2 text-[12.5px] shrink-0">
      <span class="text-[var(--sy-text-muted)]">Lanes: project</span>
      <div class="inline-flex rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-1)] p-0.5" role="group" aria-label="View">
        <button type="button" class="sy-btn sy-btn-sm" [attr.aria-pressed]="view() === 'board'" data-testid="view-board" (click)="setView('board')">Board</button>
        <button type="button" class="sy-btn sy-btn-sm" [attr.aria-pressed]="view() === 'list'" data-testid="view-list" (click)="setView('list')">List</button>
      </div>
      <span class="sy-kbd" aria-hidden="true">⌘B</span>
      <span class="ml-auto hidden md:inline text-[var(--sy-text-secondary)]">Syre moves the cards; you review and decide.</span>
      <button type="button" class="sy-btn sy-btn-sm" data-testid="open-projects" (click)="ui.projectsOpen.set(true)">
        <app-icon name="project" [size]="12" />
        Projects
      </button>
      <button type="button" class="sy-btn sy-btn-sm sy-btn-soft" data-testid="new-task" (click)="ui.newSession('chat')">
        <app-icon name="plus" [size]="12" />
        New task
      </button>
    </div>

    @if (missingSid()) {
      <div class="mx-4 mb-2 flex flex-wrap items-center justify-between gap-2 rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-1)] border border-[var(--sy-hairline)] px-3 py-2 text-[12.5px] text-[var(--sy-text-secondary)] shrink-0" role="status" data-testid="work-not-found">
        <span>No work item {{ missingSid() }} here (it may have been removed).</span>
        <button type="button" class="sy-btn sy-btn-sm sy-btn-soft" (click)="clear()">Clear</button>
      </div>
    }

    <div class="flex flex-1 min-h-0">
      <main class="flex-1 min-w-0 overflow-auto px-4 pb-4" aria-label="Work board">
        @if (view() === 'board') {
          <div class="sy-board" role="table" aria-label="Work by project and state" data-testid="work-board">
            <div role="row" class="contents">
              <div role="columnheader" class="sy-board-head"><span class="sy-sr-only">Lane</span></div>
              @for (c of board().columns; track c) {
                <div role="columnheader" class="sy-board-head" [attr.data-column]="c" [class.text-[var(--sy-amber)]]="c === 'needs_you' && countOf(c) > 0 || (c === 'running' && overCapacity())">
                  {{ labels[c] }}
                  <span class="font-mono-ide normal-case tracking-normal" [attr.data-testid]="'count-' + c">{{ c === 'running' && cap() ? countOf(c) + ' / ' + cap() : countOf(c) }}</span>
                </div>
              }
            </div>
            @for (lane of lanes(); track lane.key) {
              <div role="row" class="contents" [attr.data-lane]="lane.name">
                <div role="rowheader" class="pt-2.5 text-[12.5px] font-medium truncate" [class.text-[var(--sy-amber)]]="lane.key === 'expedite'" [class.text-[var(--sy-text)]]="lane.key !== 'expedite'">
                  {{ lane.key === 'expedite' ? '⚑ Expedite' : lane.name }}
                </div>
                @for (c of board().columns; track c) {
                  <div role="cell" class="pt-1.5 min-h-[44px] flex flex-col gap-1.5" [attr.data-cell]="c">
                    @for (it of lane.cells[c]; track it.id) {
                      <button
                        type="button"
                        class="sy-work-card"
                        [class.sy-work-card-selected]="selected()?.id === it.id"
                        [attr.aria-current]="selected()?.id === it.id ? 'true' : null"
                        [attr.data-card]="it.sid"
                        (click)="select(it)"
                      >
                        <span class="flex items-center gap-1.5 w-full">
                          <sy-item-spinner [item]="it" [size]="20" [wordless]="true" class="shrink-0" />
                          <span class="font-mono-ide text-[10.5px] text-[var(--sy-text-muted)]">{{ it.sid }}</span>
                          <span class="flex-1"></span>
                          <sy-control-label [label]="it.control" />
                        </span>
                        <span class="block mt-1 text-[12.5px] text-[var(--sy-text)] line-clamp-2">{{ it.title }}</span>
                        <span class="block mt-0.5 text-[11px] truncate" [class.text-[var(--sy-amber)]]="it.stale" [class.text-[var(--sy-text-muted)]]="!it.stale">{{ cardSub(it) }}</span>
                      </button>
                    }
                  </div>
                }
              </div>
            }
          </div>
          @if (empty()) {
            <p class="mt-6 text-center text-[12.5px] text-[var(--sy-text-muted)]" data-testid="work-empty">No work today yet. Queue something from Command or with New task.</p>
          }
        } @else {
          <table class="sy-work-list w-full text-[12.5px]" data-testid="work-list">
            <caption class="sy-sr-only">Work by state</caption>
            <thead>
              <tr>
                <th scope="col">State</th>
                <th scope="col">Work</th>
                <th scope="col">Project</th>
                <th scope="col">Agent</th>
                <th scope="col">Machine</th>
                <th scope="col">Now</th>
              </tr>
            </thead>
            @for (c of board().columns; track c) {
              <tbody>
                <tr>
                  <th colspan="6" scope="rowgroup" class="sy-list-group">{{ labels[c] }} · {{ countOf(c) }}</th>
                </tr>
                @for (it of columnItems(c); track it.id) {
                  <tr class="cursor-pointer hover:bg-[var(--sy-raised)]" [class.bg-[var(--sy-blue-soft)]]="selected()?.id === it.id" [attr.data-row]="it.sid" (click)="select(it)">
                    <td><sy-control-label [label]="it.control" [fixed]="true" /></td>
                    <td>
                      <button type="button" class="sy-focusable text-left text-[var(--sy-text)] rounded-sm" (click)="$event.stopPropagation(); select(it)">
                        <span class="font-mono-ide text-[10.5px] text-[var(--sy-text-muted)] mr-1.5">{{ it.sid }}</span>{{ it.title }}
                      </button>
                    </td>
                    <td class="text-[var(--sy-text-secondary)]">{{ work.isExpedited(it.id) ? '⚑ ' : '' }}{{ it.projectName }}</td>
                    <td class="text-[var(--sy-text-secondary)]">{{ it.sessionKind === 'terminal' ? 'terminal' : it.agent }}</td>
                    <td class="text-[var(--sy-text-secondary)]">{{ it.machine }}</td>
                    <td class="text-[var(--sy-text-muted)] max-w-[320px] truncate" [syTooltip]="it.nowTitle">{{ it.now }}</td>
                  </tr>
                }
              </tbody>
            }
          </table>
        }
      </main>

      <aside class="hidden lg:flex flex-col w-[400px] shrink-0 bg-[var(--sy-bg-chrome)] border-l border-[var(--sy-hairline)]" aria-label="Review">
        <sy-review-panel class="flex-1" [item]="selected()" />
      </aside>
    </div>

    @if (selected(); as it) {
      <!-- Narrow screens: the panel slides over the board. -->
      <div class="lg:hidden fixed inset-0 z-40 flex justify-end" role="dialog" aria-modal="true" aria-label="Review">
        <div class="sy-backdrop absolute inset-0" (click)="clear()"></div>
        <div class="sy-drawer-bottom relative z-10 w-full max-w-[440px] h-full bg-[var(--sy-bg-chrome)] flex flex-col">
          <sy-review-panel class="flex-1" [item]="it" />
        </div>
      </div>
    }
  `,
  styles: `
    .sy-board {
      display: grid;
      grid-template-columns: 120px repeat(5, minmax(160px, 1fr));
      column-gap: 8px;
      min-width: 960px;
    }
    .sy-board-head {
      padding: 4px 2px 6px;
      border-bottom: 1px solid var(--sy-hairline);
      font-size: 11.5px;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--sy-text-secondary);
    }
    .sy-work-card {
      display: block;
      width: 100%;
      text-align: left;
      padding: 8px 10px;
      border: 0;
      border-radius: var(--sy-radius-md);
      background: var(--sy-surface-2);
      color: inherit;
      cursor: pointer;
      transition: background-color var(--sy-dur-fast) var(--sy-ease-out);
    }
    .sy-work-card:hover {
      background: var(--sy-surface-3);
    }
    .sy-work-card:focus-visible {
      outline: none;
      box-shadow: var(--sy-focus-ring);
    }
    .sy-work-card-selected {
      box-shadow: inset 0 0 0 1px var(--sy-blue);
    }
    .sy-work-list th,
    .sy-work-list td {
      padding: 6px 8px;
      text-align: left;
      border-bottom: 1px solid var(--sy-hairline);
    }
    .sy-work-list thead th {
      font-size: 11px;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--sy-text-muted);
    }
    .sy-list-group {
      padding-top: 14px !important;
      font-size: 11.5px;
      font-weight: 600;
      color: var(--sy-text-secondary);
    }
    @media (prefers-reduced-motion: reduce) {
      .sy-work-card {
        transition: none;
      }
    }
  `,
})
export class WorkModeComponent {
  readonly work = inject(WorkStore);
  readonly ui = inject(ModeUiStore);
  private readonly needsYou = inject(NeedsYouStore);
  private readonly router = inject(Router);
  readonly labels = COLUMN_LABELS;

  readonly sid = toSignal(inject(ActivatedRoute).paramMap.pipe(map((p) => p.get('sid') ?? '')), { initialValue: '' });
  // A phone gets the list: the board needs a wide screen.
  readonly view = signal<'board' | 'list'>(isPhoneWidth() ? 'list' : loadView());
  readonly board = this.work.board;
  readonly cap = this.work.capacity;

  readonly selected = computed(() => {
    const sid = this.sid();
    return sid ? (this.work.bySid(sid) ?? null) : null;
  });
  readonly missingSid = computed(() => (this.sid() && !this.selected() && this.ui.workLoaded() ? this.sid() : ''));

  readonly lanes = computed(() => {
    const b = this.board();
    return [{ key: 'expedite', name: 'Expedite', cells: b.expedite }, ...b.lanes];
  });

  readonly empty = computed(() => this.board().columns.every((c) => this.countOf(c) === 0));
  readonly overCapacity = computed(() => this.cap() > 0 && this.countOf('running') > this.cap());

  constructor() {
    const doc = inject(DOCUMENT);
    const onKey = (ev: KeyboardEvent) => {
      if ((ev.metaKey || ev.ctrlKey) && !ev.altKey && ev.key.toLowerCase() === 'b') {
        ev.preventDefault();
        this.setView(this.view() === 'board' ? 'list' : 'board');
      }
    };
    doc.addEventListener('keydown', onKey);
    inject(DestroyRef).onDestroy(() => doc.removeEventListener('keydown', onKey));
  }

  countOf(c: WorkColumn): number {
    const b = this.board();
    return b.expedite[c].length + b.lanes.reduce((n, l) => n + l.cells[c].length, 0);
  }

  columnItems(c: WorkColumn): WorkItem[] {
    const b = this.board();
    return [...b.expedite[c], ...b.lanes.flatMap((l) => l.cells[c])];
  }

  cardSub(it: WorkItem): string {
    const who = it.sessionKind === 'terminal' ? 'terminal' : it.agent;
    const tail = it.stale ? 'machine not reporting' : it.column === 'running' || it.column === 'needs_you' || it.column === 'queued' ? it.now : '';
    return [who, it.machine, tail].filter(Boolean).join(' · ');
  }

  /** Needs-you and review cards open the Needs-you dialog; others the side panel. */
  select(it: WorkItem): void {
    const e = it.column === 'needs_you' || it.column === 'review' ? this.needsYou.entryFor(it.id) : undefined;
    if (e) {
      this.needsYou.open(e.key);
      return;
    }
    void this.router.navigate(['/work', it.sid]);
  }

  clear(): void {
    void this.router.navigate(['/work']);
  }

  setView(v: 'board' | 'list'): void {
    this.view.set(v);
    try {
      localStorage.setItem(VIEW_KEY, v);
    } catch {
      // ignore
    }
  }
}

function loadView(): 'board' | 'list' {
  try {
    return localStorage.getItem(VIEW_KEY) === 'list' ? 'list' : 'board';
  } catch {
    return 'board';
  }
}

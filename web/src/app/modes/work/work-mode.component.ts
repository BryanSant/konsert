import { DOCUMENT } from '@angular/common';
import { ChangeDetectionStrategy, Component, DestroyRef, computed, inject, signal } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { ActivatedRoute, Router } from '@angular/router';
import { map } from 'rxjs';
import { WorkColumn } from '../../core/state/work-logic';
import { WorkStore } from '../../core/state/work.store';
import { NeedsYouStore } from '../../core/state/needs-you.store';
import { FleetStore } from '../../core/state/fleet.store';
import { TicketStore } from '../../core/state/ticket.store';
import { IconComponent } from '../../shared/icon.component';
import { ControlLabelComponent } from '../control-label.component';
import { ModeUiStore } from '../mode-ui.store';
import { isPhoneWidth } from '../../app.routes';
import { ReviewPanelComponent } from './review-panel.component';
import { AutopilotControlComponent } from './autopilot-control.component';
import { ItemSpinnerComponent } from '../item-spinner.component';
import { TicketPanelComponent } from './ticket-panel.component';
import { NewTicketDialogComponent } from './new-ticket-dialog.component';
import { BoardCard, BoardColumn, SwimlaneBy, buildTicketBoard, defaultLaneBy } from './board-logic';

export const COLUMN_LABELS: Record<BoardColumn, string> = {
  backlog: 'Backlog',
  queued: 'Queued',
  running: 'Running',
  needs_you: 'Needs you',
  review: 'Review',
  done: 'Done',
};

const VIEW_KEY = 'syre.work.view';
const LANE_KEY = 'syre.work.lanes';

/**
 * 2 Work: the flow board. Columns follow ticket status. Swimlanes are Expedite plus
 * one per epic, initiative or project. Ad-hoc tasks and chats with no ticket stay
 * on the board. ⌘B toggles board and list; a phone opens on the list.
 */
@Component({
  selector: 'sy-work-mode',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [ItemSpinnerComponent, IconComponent, ControlLabelComponent, ReviewPanelComponent, AutopilotControlComponent, TicketPanelComponent, NewTicketDialogComponent],
  host: { class: 'flex flex-col h-full min-h-0' },
  template: `
    <div class="flex flex-wrap items-center gap-3 px-4 pt-3 pb-2 text-[12.5px] shrink-0">
      <label class="inline-flex items-center gap-1.5 text-[var(--sy-text-muted)]">Lanes
        <select class="sy-field sy-field-sm" data-testid="lane-by" aria-label="Swimlanes" [value]="laneBy()" (change)="setLane($any($event.target).value)">
          <option value="epic">Epic</option>
          <option value="initiative">Initiative</option>
          <option value="project">Project</option>
        </select>
      </label>
      <div class="inline-flex rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-1)] p-0.5" role="group" aria-label="View">
        <button type="button" class="sy-btn sy-btn-sm" [attr.aria-pressed]="view() === 'board'" data-testid="view-board" (click)="setView('board')">Board</button>
        <button type="button" class="sy-btn sy-btn-sm" [attr.aria-pressed]="view() === 'list'" data-testid="view-list" (click)="setView('list')">List</button>
      </div>
      <span class="sy-kbd" aria-hidden="true">⌘B</span>
      <button type="button" class="sy-btn sy-btn-sm" [attr.aria-pressed]="showBacklog()" data-testid="show-backlog" (click)="showBacklog.set(!showBacklog())">Backlog</button>
      <button type="button" class="sy-btn sy-btn-sm" [attr.aria-pressed]="showClosed()" data-testid="show-closed" (click)="showClosed.set(!showClosed())">Failed &amp; canceled</button>
      <sy-autopilot-control />
      <span class="ml-auto hidden xl:inline text-[var(--sy-text-secondary)]">Syre moves the cards; you review and decide.</span>
      <button type="button" class="sy-btn sy-btn-sm" data-testid="open-projects" (click)="ui.projectsOpen.set(true)">
        <app-icon name="project" [size]="12" />
        Projects
      </button>
      <button type="button" class="sy-btn sy-btn-sm sy-btn-soft" data-testid="new-ticket" (click)="creating.set(true)">
        <app-icon name="plus" [size]="12" />
        New ticket
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
          <div class="sy-board" role="table" aria-label="Work by lane and state" data-testid="work-board" [style.--board-cols]="board().columns.length">
            <div role="row" class="contents">
              <div role="columnheader" class="sy-board-head"><span class="sy-sr-only">Lane</span></div>
              @for (c of board().columns; track c) {
                <div role="columnheader" class="sy-board-head" [attr.data-column]="c" [class.text-[var(--sy-amber)]]="c === 'needs_you' && countOf(c) > 0 || (c === 'running' && overCapacity())">
                  {{ labels[c] }}
                  <span class="font-mono-ide normal-case tracking-normal" [attr.data-testid]="'count-' + c">{{ c === 'running' && cap() ? countOf(c) + ' / ' + cap() : countOf(c) }}</span>
                </div>
              }
            </div>
            @for (lane of board().lanes; track lane.key) {
              <div role="row" class="contents" [attr.data-lane]="lane.name">
                <div role="rowheader" class="pt-2.5 text-[12.5px] font-medium" [class.text-[var(--sy-amber)]]="lane.expedite" [class.text-[var(--sy-text)]]="!lane.expedite">
                  <div class="truncate">{{ lane.expedite ? '⚑ Expedite' : lane.name }}</div>
                  @if (lane.rollup; as roll) {
                    <div class="mt-0.5 text-[10.5px] font-normal text-[var(--sy-text-muted)]" data-testid="lane-rollup">{{ roll.done }}/{{ roll.total }} · {{ roll.running }} running · {{ roll.blocked }} blocked</div>
                  }
                </div>
                @for (c of board().columns; track c) {
                  <div role="cell" class="pt-1.5 min-h-[44px] flex flex-col gap-1.5" [attr.data-cell]="c">
                    @for (card of lane.cells[c]; track card.id) {
                      <button
                        type="button"
                        class="sy-work-card"
                        [class.sy-work-card-selected]="isSelected(card)"
                        [class.sy-work-card-blocked]="card.blocked"
                        [attr.aria-current]="isSelected(card) ? 'true' : null"
                        [attr.data-card]="card.sid"
                        [attr.data-blocked]="card.blocked ? 'true' : null"
                        (click)="selectCard(card)"
                      >
                        <span class="flex items-center gap-1.5 w-full">
                          @if (card.work) {
                            <sy-item-spinner [item]="card.work" [size]="20" [wordless]="true" class="shrink-0" />
                          }
                          <span class="font-mono-ide text-[10.5px] text-[var(--sy-text-muted)]">{{ card.sid }}</span>
                          @if (card.priority) {
                            <span class="text-[10px] font-semibold text-[var(--sy-text-secondary)]" [class.text-[var(--sy-amber)]]="card.priority === 'P0'">{{ card.priority }}</span>
                          }
                          <span class="flex-1"></span>
                          @if (card.work) {
                            <sy-control-label [label]="card.work.control" />
                          }
                          @if (card.acceptanceTotal > 0) {
                            <span class="font-mono-ide text-[10.5px] text-[var(--sy-text-muted)]" data-testid="acceptance-progress">{{ card.acceptanceDone }}/{{ card.acceptanceTotal }}</span>
                          }
                        </span>
                        <span class="block mt-1 text-[12.5px] text-[var(--sy-text)] line-clamp-2">{{ card.title }}</span>
                        @if (card.kind === 'ticket') {
                          @if (card.leaseMachine || card.leaseAgent) {
                            <span class="block mt-0.5 text-[11px] text-[var(--sy-text-muted)] truncate" data-testid="lease">{{ leaseLine(card) }}</span>
                          }
                          @if (card.attempt > 1) {
                            <span class="block mt-0.5 text-[11px] text-[var(--sy-text-muted)]" data-testid="attempt-count">attempt {{ card.attempt }}</span>
                          }
                          @if (card.digest) {
                            <span class="block mt-0.5 text-[11px] text-[var(--sy-text-muted)] truncate" data-testid="digest-line">{{ card.digest }}</span>
                          }
                          @if (card.blockers.length) {
                            <span class="mt-1 flex flex-wrap gap-1">
                              @for (b of card.blockers; track b.id) {
                                <span class="inline-flex rounded-full bg-[var(--sy-surface-1)] px-1.5 py-0.5 text-[10px] text-[var(--sy-text-secondary)]" data-testid="blocker-chip">blocked by {{ b.sid }}</span>
                              }
                            </span>
                          }
                        } @else {
                          <span class="block mt-0.5 text-[11px] truncate" [class.text-[var(--sy-amber)]]="card.work?.stale" [class.text-[var(--sy-text-muted)]]="!card.work?.stale">{{ cardSub(card) }}</span>
                        }
                      </button>
                    }
                  </div>
                }
              </div>
            }
          </div>
          @if (empty()) {
            <p class="mt-6 text-center text-[12.5px] text-[var(--sy-text-muted)]" data-testid="work-empty">Nothing on the board. Create a ticket, or queue a task from Command.</p>
          }
        } @else {
          <table class="sy-work-list w-full text-[12.5px]" data-testid="work-list">
            <caption class="sy-sr-only">Work by state</caption>
            <thead>
              <tr>
                <th scope="col">State</th>
                <th scope="col">Work</th>
                <th scope="col">Lane</th>
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
                @for (card of columnCards(c); track card.id) {
                  <tr class="cursor-pointer hover:bg-[var(--sy-raised)]" [class.bg-[var(--sy-blue-soft)]]="isSelected(card)" [class.opacity-55]="card.blocked" [attr.data-row]="card.sid" [attr.data-blocked]="card.blocked ? 'true' : null" (click)="selectCard(card)">
                    <td>
                      @if (card.work) {
                        <sy-control-label [label]="card.work.control" [fixed]="true" />
                      } @else {
                        <span class="text-[var(--sy-text-secondary)]">{{ card.priority }}</span>
                      }
                    </td>
                    <td>
                      <button type="button" class="sy-focusable text-left text-[var(--sy-text)] rounded-sm" (click)="$event.stopPropagation(); selectCard(card)">
                        <span class="font-mono-ide text-[10.5px] text-[var(--sy-text-muted)] mr-1.5">{{ card.sid }}</span>{{ card.title }}
                      </button>
                      @for (b of card.blockers; track b.id) {
                        <span class="ml-1.5 inline-flex rounded-full bg-[var(--sy-surface-1)] px-1.5 py-0.5 text-[10px] text-[var(--sy-text-secondary)]" data-testid="blocker-chip">blocked by {{ b.sid }}</span>
                      }
                    </td>
                    <td class="text-[var(--sy-text-secondary)]">{{ card.priority === 'P0' || (card.work && work.isExpedited(card.id)) ? '⚑ ' : '' }}{{ card.laneName }}</td>
                    <td class="text-[var(--sy-text-secondary)]">{{ card.agent }}</td>
                    <td class="text-[var(--sy-text-secondary)]">{{ card.machine || card.leaseMachine }}</td>
                    <td class="text-[var(--sy-text-muted)] max-w-[320px] truncate">{{ card.digest || card.now }}</td>
                  </tr>
                }
              </tbody>
            }
          </table>
        }
      </main>

      <aside class="hidden lg:flex flex-col w-[400px] shrink-0 bg-[var(--sy-bg-chrome)] border-l border-[var(--sy-hairline)]" aria-label="Review">
        @if (selectedTicket(); as tk) {
          <sy-ticket-panel class="flex-1" [ticket]="tk" />
        } @else {
          <sy-review-panel class="flex-1" [item]="selectedWork()" />
        }
      </aside>
    </div>

    @if (selectedTicket(); as tk) {
      <div class="lg:hidden fixed inset-0 z-40 flex justify-end" role="dialog" aria-modal="true" aria-label="Ticket">
        <div class="sy-backdrop absolute inset-0" (click)="clear()"></div>
        <div class="sy-drawer-bottom relative z-10 w-full max-w-[440px] h-full bg-[var(--sy-bg-chrome)] flex flex-col">
          <sy-ticket-panel class="flex-1" [ticket]="tk" />
        </div>
      </div>
    } @else if (selectedWork(); as it) {
      <div class="lg:hidden fixed inset-0 z-40 flex justify-end" role="dialog" aria-modal="true" aria-label="Review">
        <div class="sy-backdrop absolute inset-0" (click)="clear()"></div>
        <div class="sy-drawer-bottom relative z-10 w-full max-w-[440px] h-full bg-[var(--sy-bg-chrome)] flex flex-col">
          <sy-review-panel class="flex-1" [item]="it" />
        </div>
      </div>
    }

    @if (creating()) {
      <sy-new-ticket-dialog (closed)="creating.set(false)" />
    }
  `,
  styles: `
    .sy-board {
      display: grid;
      grid-template-columns: 148px repeat(var(--board-cols, 5), minmax(160px, 1fr));
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
    .sy-work-card-blocked {
      opacity: 0.55;
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
  private readonly fleet = inject(FleetStore);
  readonly tickets = inject(TicketStore);
  private readonly router = inject(Router);
  readonly labels = COLUMN_LABELS;

  readonly sid = toSignal(inject(ActivatedRoute).paramMap.pipe(map((p) => p.get('sid') ?? '')), { initialValue: '' });
  readonly view = signal<'board' | 'list'>(isPhoneWidth() ? 'list' : loadView());
  readonly laneChoice = signal<SwimlaneBy | null>(loadLane());
  readonly showBacklog = signal(false);
  readonly showClosed = signal(false);
  readonly creating = signal(false);
  readonly cap = this.work.capacity;

  readonly laneBy = computed(() => this.laneChoice() ?? defaultLaneBy(this.tickets.tickets()));

  readonly board = computed(() =>
    buildTicketBoard({
      tickets: this.tickets.tickets(),
      edges: this.tickets.edges(),
      digests: this.tickets.digests(),
      projects: this.fleet.projects(),
      workers: this.fleet.workersById(),
      tasks: this.fleet.tasks(),
      adHoc: this.work.items(),
      expedited: (id) => this.work.isExpedited(id),
      laneBy: this.laneBy(),
      showBacklog: this.showBacklog(),
      showClosed: this.showClosed(),
    }),
  );

  readonly selectedTicket = computed(() => {
    const sid = this.sid();
    if (!sid) return null;
    const tk = this.tickets.bySid(sid);
    return tk ?? null;
  });
  readonly selectedWork = computed(() => {
    if (this.selectedTicket()) return null;
    const sid = this.sid();
    return sid ? (this.work.bySid(sid) ?? null) : null;
  });
  readonly missingSid = computed(() => {
    const sid = this.sid();
    if (!sid || this.selectedTicket() || this.selectedWork()) return '';
    if (!this.ui.workLoaded() || !this.tickets.loaded()) return '';
    return sid;
  });

  readonly empty = computed(() => this.board().columns.every((c) => this.countOf(c) === 0));
  readonly overCapacity = computed(() => this.cap() > 0 && this.countOf('running') > this.cap());

  constructor() {
    const doc = inject(DOCUMENT);
    const onKey = (ev: KeyboardEvent) => {
      const target = ev.target as HTMLElement | null;
      const typing = target && (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.tagName === 'SELECT' || target.isContentEditable);
      if (typing) return;
      if ((ev.metaKey || ev.ctrlKey) && !ev.altKey && ev.key.toLowerCase() === 'b') {
        ev.preventDefault();
        this.setView(this.view() === 'board' ? 'list' : 'board');
      }
    };
    doc.addEventListener('keydown', onKey);
    inject(DestroyRef).onDestroy(() => doc.removeEventListener('keydown', onKey));
  }

  countOf(c: BoardColumn): number {
    return this.board().lanes.reduce((n, l) => n + l.cells[c].length, 0);
  }

  columnCards(c: BoardColumn): BoardCard[] {
    return this.board().lanes.flatMap((l) => l.cells[c]);
  }

  isSelected(card: BoardCard): boolean {
    const sid = this.sid();
    return !!sid && (sid === card.sid || sid === card.id);
  }

  leaseLine(card: BoardCard): string {
    return [card.leaseMachine, card.leaseAgent].filter(Boolean).join(' · ');
  }

  cardSub(card: BoardCard): string {
    const it = card.work;
    if (!it) return card.now;
    const who = it.sessionKind === 'terminal' ? 'terminal' : it.agent;
    const tail = it.stale ? 'machine not reporting' : it.column === 'running' || it.column === 'needs_you' || it.column === 'queued' ? it.now : '';
    return [who, it.machine, tail].filter(Boolean).join(' · ');
  }

  selectCard(card: BoardCard): void {
    if (card.kind !== 'ticket' && card.work) {
      const it = card.work;
      const e = (it.column === 'needs_you' || it.column === 'review') && (it.column as WorkColumn) ? this.needsYou.entryFor(it.id) : undefined;
      if (e) {
        this.needsYou.open(e.key);
        return;
      }
    }
    void this.router.navigate(['/work', card.sid]);
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

  setLane(v: SwimlaneBy): void {
    this.laneChoice.set(v);
    try {
      localStorage.setItem(LANE_KEY, v);
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

function loadLane(): SwimlaneBy | null {
  try {
    const v = localStorage.getItem(LANE_KEY);
    if (v === 'epic' || v === 'initiative' || v === 'project') return v;
  } catch {
    // ignore
  }
  return null;
}

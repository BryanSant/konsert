import { ChangeDetectionStrategy, Component, computed, effect, inject, signal, untracked } from '@angular/core';
import { Router } from '@angular/router';
import { WorkItem } from '../../core/state/work-logic';
import { WorkStore } from '../../core/state/work.store';
import { SessionChatComponent } from '../../features/chat/session-chat.component';
import { SessionControlsComponent } from '../../features/control/session-controls.component';
import { XtermPaneComponent } from '../../features/terminal/xterm-pane.component';
import { IconComponent } from '../../shared/icon.component';
import { PopoverComponent, PopoverTriggerDirective } from '../../shared/popover.component';
import { TooltipDirective } from '../../shared/tooltip.directive';
import { ControlLabelComponent } from '../control-label.component';
import { ModeUiStore } from '../mode-ui.store';
import { TakeControlService } from '../take-control.service';
import { WALL_LAYOUTS, WALL_SOFT_CAP, WallStore, pageCount, pageOf, tilesPerPage } from './wall.store';

interface Tile {
  item: WorkItem;
  pinned: boolean;
}

/** Suggestions to fill an empty wall: what needs watching first (asking, stale), then live work. */
export function suggestTiles(items: readonly WorkItem[], pinned: readonly string[], n: number): string[] {
  if (n <= 0) return [];
  const rank = (i: WorkItem) => (i.control === 'ASKING' ? 0 : i.control === 'STALE' ? 1 : i.column === 'running' ? 2 : 3);
  return items
    .filter((i) => i.live && !i.observed && !pinned.includes(i.id) && i.column !== 'queued')
    .sort((a, b) => rank(a) - rank(b))
    .slice(0, n)
    .map((i) => i.id);
}

/**
 * 4 Wall: live sessions watched on purpose. Pinned tiles in the user's order (they never
 * move on their own) plus suggestions to fill an empty wall; up to 9 per page, paged
 * sideways; only the tiles on screen stream. Every tile is interactive: chat tiles have
 * a reply box, terminal tiles are typeable after Take control.
 */
@Component({
  selector: 'ks-wall-mode',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [
    IconComponent,
    TooltipDirective,
    PopoverComponent,
    PopoverTriggerDirective,
    ControlLabelComponent,
    SessionControlsComponent,
    SessionChatComponent,
    XtermPaneComponent,
  ],
  host: { class: 'flex flex-col h-full min-h-0' },
  styles: `
    .ks-tile-you {
      box-shadow: inset 0 0 0 2px #2f6fd6;
    }
  `,
  template: `
    <div class="flex flex-wrap items-center gap-2 px-4 pt-3 pb-2 text-[12.5px] shrink-0">
      <span class="text-[var(--ks-text-secondary)]" id="ks-wall-layout-label">Layout</span>
      <div class="inline-flex gap-1" role="group" aria-labelledby="ks-wall-layout-label">
        @for (l of layouts; track l.id) {
          <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" [attr.aria-pressed]="wall.layout() === l.id" [attr.data-layout]="l.id" (click)="wall.setLayout(l.id)">{{ l.label }}</button>
        }
      </div>
      <ks-popover label="Add a tile" placement="bottom-start" panelClass="w-[320px] max-h-[360px] overflow-y-auto">
        <button type="button" ksPopoverTrigger class="ks-btn ks-btn-sm" data-testid="wall-add">
          <app-icon name="plus" [size]="12" />
          Add tile
        </button>
        @if (addable().length === 0) {
          <p class="px-3 py-2 text-[12px] text-[var(--ks-text-muted)]">Every session is already on the wall.</p>
        }
        @for (it of addable(); track it.id) {
          <button type="button" role="menuitem" class="ks-menu-item items-center" (click)="wall.pin(it.id)">
            <app-icon [name]="it.sessionKind === 'terminal' ? 'shell' : it.agent" [size]="13" />
            <span class="flex-1 min-w-0">
              <span class="ks-menu-label block truncate">{{ it.title }}</span>
              <span class="ks-menu-desc block truncate">{{ it.sid }} · {{ it.machine }} · {{ it.status }}</span>
            </span>
          </button>
        }
      </ks-popover>
      <span class="text-[var(--ks-text-muted)] hidden xl:inline">fill: pinned + suggested (asking, not reporting, running)</span>
      <span class="ml-auto text-[var(--ks-text-muted)]" data-testid="wall-summary">
        {{ perPage() }} per page · pinned {{ wall.pinned().length }}@if (wall.overSoftCap()) {<span class="text-[var(--ks-amber)]"> (over {{ softCap }}: hard to watch)</span>} · only visible tiles stream
      </span>
      <button type="button" class="ks-icon-btn ks-icon-btn-sm" aria-label="Previous page" [disabled]="wall.page() === 0" (click)="go(-1)">
        <span class="inline-flex rotate-180"><app-icon name="chevron-right" [size]="13" /></span>
      </button>
      <span class="font-mono-ide text-[11.5px] text-[var(--ks-text-secondary)]" aria-live="polite" data-testid="wall-page">page {{ wall.page() + 1 }} / {{ pages() }}</span>
      <button type="button" class="ks-icon-btn ks-icon-btn-sm" aria-label="Next page" [disabled]="wall.page() >= pages() - 1" (click)="go(1)">
        <app-icon name="chevron-right" [size]="13" />
      </button>
    </div>

    @if (tiles().length === 0) {
      <div class="flex-1 flex flex-col items-center justify-center gap-3 text-center px-6" data-testid="wall-empty">
        <p class="max-w-md text-[13px] text-[var(--ks-text-secondary)]">
          Nothing on the wall. Pin sessions from Command or Work (or Add tile) to watch them side by side; tiles stay where you put them.
        </p>
      </div>
    } @else {
      <main class="flex-1 min-h-0 grid gap-3 px-4 pb-4" [style.grid-template-columns]="'repeat(' + cols() + ', minmax(0, 1fr))'" [style.grid-template-rows]="'repeat(' + rows() + ', minmax(0, 1fr))'" aria-label="Wall" data-testid="wall-grid">
        @for (t of visible(); track t.item.id; let i = $index) {
          <section
            class="flex flex-col min-h-0 min-w-0 rounded-[var(--ks-radius-lg)] bg-[var(--ks-surface-1)] overflow-hidden"
            [class.ring-1]="t.item.control === 'ASKING' || t.item.stale"
            [class.ring-[var(--ks-amber)]/50]="t.item.control === 'ASKING' || t.item.stale"
            [class.ks-tile-you]="takeControl.has(t.item.id)"
            [attr.aria-label]="t.item.sid + ' ' + t.item.title"
            [attr.data-tile]="t.item.sid"
          >
            <header class="flex items-center gap-2 px-3 h-9 shrink-0 border-b border-[var(--ks-hairline)] text-[12.5px]">
              <ks-control-label [label]="t.item.control" />
              <span class="font-mono-ide text-[11px] text-[var(--ks-text-muted)]">{{ t.item.sid }}</span>
              <span class="truncate min-w-0">{{ t.item.sessionKind === 'terminal' ? 'terminal' : t.item.agent }} · {{ t.item.machine }}</span>
              <span class="flex-1"></span>
              @if (t.item.stale) {
                <span class="text-[var(--ks-amber)] text-[11.5px] shrink-0">not reporting</span>
              } @else if (t.item.observed) {
                <span class="text-[11.5px] text-[var(--ks-text-muted)] shrink-0">observed</span>
              } @else if (!t.pinned) {
                <span class="text-[11.5px] text-[var(--ks-text-muted)] shrink-0">suggested</span>
              }
              @if (t.pinned) {
                <button type="button" class="ks-icon-btn ks-icon-btn-sm" [attr.aria-label]="'Move ' + t.item.sid + ' earlier'" [disabled]="pinIndex(t.item.id) === 0" (click)="wall.move(t.item.id, -1)">
                  <span class="inline-flex rotate-180"><app-icon name="chevron-right" [size]="11" /></span>
                </button>
                <button type="button" class="ks-icon-btn ks-icon-btn-sm" [attr.aria-label]="'Move ' + t.item.sid + ' later'" [disabled]="pinIndex(t.item.id) === wall.pinned().length - 1" (click)="wall.move(t.item.id, 1)">
                  <app-icon name="chevron-right" [size]="11" />
                </button>
              } @else {
                <button type="button" class="ks-btn ks-btn-sm !h-6" [attr.data-testid]="'pin-' + t.item.sid" (click)="wall.pin(t.item.id)">Pin</button>
              }
              <button type="button" class="ks-icon-btn ks-icon-btn-sm" [attr.aria-label]="'Open ' + t.item.sid + ' in Command'" ksTooltip="Open in Command" (click)="open(t.item)">
                <app-icon name="orchestrator" [size]="12" />
              </button>
              <button type="button" class="ks-icon-btn ks-icon-btn-sm" [attr.aria-label]="'Remove ' + t.item.sid + ' from the wall'" [attr.data-testid]="'remove-' + t.item.sid" (click)="remove(t)">
                <app-icon name="close" [size]="11" />
              </button>
            </header>
            <div class="flex-1 min-h-0">
              @if (t.item.kind === 'chat') {
                <ks-session-chat class="h-full" [chatId]="t.item.id" [compact]="true" />
              } @else {
                @defer (on immediate) {
                  <app-xterm-pane class="block h-full" [taskId]="t.item.id" [visible]="true" [bare]="true" />
                } @placeholder {
                  <div class="h-full flex items-center justify-center text-[12px] text-[var(--ks-text-muted)]" role="status">Loading terminal…</div>
                }
              }
            </div>
            <footer class="flex flex-wrap items-center gap-1 px-2.5 py-1.5 shrink-0 border-t border-[var(--ks-hairline)]">
              <ks-session-controls [kind]="t.item.kind" [sessionId]="t.item.id" [status]="t.item.status" [showHold]="t.item.live" [compact]="compactControls()" />
              @if (t.item.kind === 'task' && t.item.live && !t.item.observed) {
                <button type="button" class="ks-btn ks-btn-sm" [attr.aria-pressed]="takeControl.has(t.item.id)" [attr.data-testid]="'take-control-' + t.item.sid" (click)="takeControl.toggle(t.item.id)">
                  <app-icon [name]="takeControl.has(t.item.id) ? 'unlock' : 'hand'" [size]="11" />
                  {{ takeControl.has(t.item.id) ? 'Hand back' : 'Take control' }}
                </button>
              }
              <span class="flex-1"></span>
              <span class="truncate max-w-[50%] text-[11px] text-[var(--ks-text-muted)]">{{ t.item.now }}</span>
            </footer>
          </section>
        }
      </main>
    }
  `,
})
export class WallModeComponent {
  readonly wall = inject(WallStore);
  readonly takeControl = inject(TakeControlService);
  private readonly work = inject(WorkStore);
  private readonly ui = inject(ModeUiStore);
  private readonly router = inject(Router);
  readonly layouts = WALL_LAYOUTS;
  readonly softCap = WALL_SOFT_CAP;

  /** Suggestions are picked once when the wall opens, so tiles never rearrange on their own. */
  private readonly suggested = signal<string[] | null>(null);

  readonly perPage = computed(() => tilesPerPage(this.wall.layout()));
  readonly cols = computed(() => WALL_LAYOUTS.find((l) => l.id === this.wall.layout())!.cols);
  readonly rows = computed(() => WALL_LAYOUTS.find((l) => l.id === this.wall.layout())!.rows);
  readonly compactControls = computed(() => this.cols() >= 3);

  readonly tiles = computed<Tile[]>(() => {
    const byId = this.work.byId();
    const pinned = this.wall.pinned();
    const out: Tile[] = pinned.filter((id) => byId[id]).map((id) => ({ item: byId[id], pinned: true }));
    for (const id of this.suggested() ?? []) {
      if (byId[id] && !pinned.includes(id)) out.push({ item: byId[id], pinned: false });
    }
    return out;
  });
  readonly pages = computed(() => pageCount(this.tiles().length, this.wall.layout()));
  readonly visible = computed(() => pageOf(this.tiles(), this.wall.layout(), this.wall.page()));
  readonly addable = computed(() => {
    const on = new Set(this.tiles().map((t) => t.item.id));
    return this.work.items().filter((i) => !on.has(i.id) && (i.live || i.column === 'review')).slice(0, 40);
  });

  constructor() {
    // Pick suggestions once the work list has loaded.
    effect(() => {
      if (!this.ui.fleetLoaded() || this.suggested() !== null) return;
      const items = this.work.items();
      untracked(() => {
        const pinned = this.wall.pinned();
        this.suggested.set(suggestTiles(items, pinned, this.perPage() - pinned.length));
      });
    });
    // Forget pins whose session is gone; keep the page in range.
    effect(() => {
      if (!this.ui.fleetLoaded()) return;
      const byId = this.work.byId();
      if (Object.keys(byId).length === 0) return;
      untracked(() => this.wall.prune((id) => !!byId[id]));
    });
    effect(() => {
      const n = this.pages();
      untracked(() => {
        if (this.wall.page() > n - 1) this.wall.page.set(n - 1);
      });
    });
  }

  pinIndex(id: string): number {
    return this.wall.pinned().indexOf(id);
  }

  go(delta: number): void {
    this.wall.page.set(Math.min(this.pages() - 1, Math.max(0, this.wall.page() + delta)));
  }

  remove(t: Tile): void {
    if (t.pinned) this.wall.unpin(t.item.id);
    else this.suggested.update((l) => (l ?? []).filter((id) => id !== t.item.id));
  }

  open(it: WorkItem): void {
    void this.router.navigate(['/command', it.sid]);
  }

}

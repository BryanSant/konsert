import { Injectable, computed, effect, signal } from '@angular/core';

export type WallLayout = '1x2' | '2x2' | '3x2' | '3x3';

export const WALL_LAYOUTS: ReadonlyArray<{ id: WallLayout; label: string; cols: number; rows: number }> = [
  { id: '1x2', label: '1 × 2', cols: 2, rows: 1 },
  { id: '2x2', label: '2 × 2', cols: 2, rows: 2 },
  { id: '3x2', label: '3 × 2', cols: 3, rows: 2 },
  { id: '3x3', label: '3 × 3', cols: 3, rows: 3 },
];

/** Pins beyond this still work, but the Wall says it is getting hard to watch. */
export const WALL_SOFT_CAP = 24;
const KEY = 'syre.wall.v1';

interface Persisted {
  pinned: string[];
  layout: WallLayout;
}

export function tilesPerPage(layout: WallLayout): number {
  const l = WALL_LAYOUTS.find((x) => x.id === layout) ?? WALL_LAYOUTS[1];
  return l.cols * l.rows;
}

/** Splits tiles into pages of the layout's size; never reorders. */
export function pageOf<T>(tiles: readonly T[], layout: WallLayout, page: number): T[] {
  const n = tilesPerPage(layout);
  return tiles.slice(page * n, page * n + n);
}

export function pageCount(total: number, layout: WallLayout): number {
  return Math.max(1, Math.ceil(total / tilesPerPage(layout)));
}

/**
 * The Wall's pinned sessions (in the user's order: tiles never move on their own) and its
 * layout, kept across reloads.
 */
@Injectable({ providedIn: 'root' })
export class WallStore {
  readonly pinned = signal<string[]>([]);
  readonly layout = signal<WallLayout>('2x2');
  readonly page = signal(0);
  readonly overSoftCap = computed(() => this.pinned().length > WALL_SOFT_CAP);

  constructor() {
    try {
      const raw = JSON.parse(localStorage.getItem(KEY) || '{}') as Partial<Persisted>;
      if (Array.isArray(raw.pinned)) this.pinned.set(raw.pinned.filter((x) => typeof x === 'string'));
      if (raw.layout && WALL_LAYOUTS.some((l) => l.id === raw.layout)) this.layout.set(raw.layout);
    } catch {
      // defaults
    }
    effect(() => {
      const state: Persisted = { pinned: this.pinned(), layout: this.layout() };
      try {
        localStorage.setItem(KEY, JSON.stringify(state));
      } catch {
        // storage unavailable
      }
    });
  }

  isPinned(id: string): boolean {
    return this.pinned().includes(id);
  }

  pin(id: string): void {
    if (!this.isPinned(id)) this.pinned.update((l) => [...l, id]);
  }

  unpin(id: string): void {
    this.pinned.update((l) => l.filter((x) => x !== id));
  }

  togglePin(id: string): void {
    if (this.isPinned(id)) this.unpin(id);
    else this.pin(id);
  }

  /** Moves a pinned tile one place earlier (-1) or later (+1): only the user reorders. */
  move(id: string, delta: -1 | 1): void {
    const list = [...this.pinned()];
    const i = list.indexOf(id);
    const j = i + delta;
    if (i < 0 || j < 0 || j >= list.length) return;
    [list[i], list[j]] = [list[j], list[i]];
    this.pinned.set(list);
  }

  setLayout(l: WallLayout): void {
    this.layout.set(l);
    this.page.set(0);
  }

  /** Drops pins whose session no longer exists. */
  prune(known: (id: string) => boolean): void {
    const kept = this.pinned().filter(known);
    if (kept.length !== this.pinned().length) this.pinned.set(kept);
  }
}

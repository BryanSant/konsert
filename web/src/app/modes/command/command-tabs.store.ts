import { Injectable, effect, signal } from '@angular/core';

export const COMMAND_TABS_KEY = 'konsert.command.tabs.v1';

/**
 * The session tabs open next to the pinned Orchestrator tab in Command, by work item id
 * (task or chat). Which one is active comes from the URL (/command/<sid>); this only
 * remembers what is open, across reloads.
 */
@Injectable({ providedIn: 'root' })
export class CommandTabsStore {
  readonly ids = signal<string[]>(load());

  constructor() {
    effect(() => {
      try {
        localStorage.setItem(COMMAND_TABS_KEY, JSON.stringify(this.ids()));
      } catch {
        // storage unavailable: tabs last for this page only
      }
    });
  }

  add(id: string): void {
    if (!this.ids().includes(id)) this.ids.update((l) => [...l, id]);
  }

  /** Closes a tab; returns the id that should become active (a neighbour), or '' for the orchestrator. */
  close(id: string): string {
    const list = this.ids();
    const i = list.indexOf(id);
    if (i < 0) return '';
    const next = list.filter((x) => x !== id);
    this.ids.set(next);
    return next[Math.max(0, i - 1)] ?? '';
  }

  /** Drops tabs whose session no longer exists (call once the stores have loaded). */
  prune(known: (id: string) => boolean): void {
    const kept = this.ids().filter(known);
    if (kept.length !== this.ids().length) this.ids.set(kept);
  }
}

function load(): string[] {
  try {
    const v = JSON.parse(localStorage.getItem(COMMAND_TABS_KEY) || '[]') as unknown;
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [];
  } catch {
    return [];
  }
}

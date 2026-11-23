import { Injectable, computed, inject, signal } from '@angular/core';
import { FleetEvent } from '../models/control.models';
import { ControlStore } from './control.store';
import { QuestionsStore } from './questions.store';
import { WorkStore } from './work.store';

const DISMISSED_KEY = 'konsert.needsyou.dismissed.v1';
/** Vetoes stay in Needs you this long unless dismissed. */
export const VETO_WINDOW_MS = 2 * 60 * 60 * 1000;

function loadDismissed(): Record<string, number> {
  try {
    return JSON.parse(localStorage.getItem(DISMISSED_KEY) || '{}') as Record<string, number>;
  } catch {
    return {};
  }
}

/** Recent vetoes the user has not dismissed yet, newest first. */
export function recentVetoes(events: readonly FleetEvent[], dismissed: Record<string, number>, now = Date.now()): FleetEvent[] {
  return events.filter(
    (e) => e.kind === 'veto' && !dismissed[`veto:${e.id}`] && now - Date.parse(e.created_at) < VETO_WINDOW_MS,
  );
}

/**
 * Everything that wants the user, for the one Needs-you area (Command's right rail, the
 * mobile inbox): questions, open requests, vetoes and their delivery, reviews ready, and
 * the holds the user placed. Override requests have no hub API yet; they slot in here.
 */
@Injectable({ providedIn: 'root' })
export class NeedsYouStore {
  private readonly questionsStore = inject(QuestionsStore);
  private readonly control = inject(ControlStore);
  private readonly work = inject(WorkStore);

  private readonly dismissed = signal<Record<string, number>>(loadDismissed());
  /** Clock for the veto window; ticked by views that show it. */
  readonly now = signal(Date.now());

  readonly questions = computed(() => this.questionsStore.pending());
  readonly requests = computed(() => this.control.requests());
  readonly holds = computed(() => this.control.holds());
  readonly vetoes = computed(() => recentVetoes(this.control.events(), this.dismissed(), this.now()));
  readonly reviews = computed(() => this.work.reviews());
  /** Override requests (break-glass) from the orchestrator: no hub API yet. */
  readonly overrides = computed<never[]>(() => []);

  /** What the header and badges count: items that need a decision (holds are the user's own). */
  readonly count = computed(
    () => this.questions().length + this.requests().length + this.vetoes().length + this.reviews().length,
  );
  /** Items that block an agent right now (questions and requests). */
  readonly blocking = computed(() => this.questions().length + this.requests().length);
  readonly empty = computed(() => this.count() === 0 && this.holds().length === 0);

  /** An item someone asked to see (a chat's "Answer in Needs you"): the rail focuses it. */
  readonly focusRequest = signal<{ id: string; at: number } | null>(null);

  focus(id: string): void {
    this.focusRequest.set({ id, at: Date.now() });
  }

  dismissVeto(ev: FleetEvent): void {
    this.dismissed.update((d) => {
      const next: Record<string, number> = { [`veto:${ev.id}`]: Date.now() };
      // Forget dismissals older than the window so the map stays small.
      for (const [k, at] of Object.entries(d)) if (Date.now() - at < VETO_WINDOW_MS * 2) next[k] = at;
      return next;
    });
    try {
      localStorage.setItem(DISMISSED_KEY, JSON.stringify(this.dismissed()));
    } catch {
      // storage unavailable
    }
  }
}

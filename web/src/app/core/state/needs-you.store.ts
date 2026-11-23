import { Injectable, computed, inject, signal } from '@angular/core';
import { FleetEvent, UserRequest, VETO_REASONS, resourceLabel } from '../models/control.models';
import { QuestionView, isPending } from '../models/question.models';
import { ControlStore } from './control.store';
import { FleetStore } from './fleet.store';
import { QuestionsStore } from './questions.store';
import { formatSince } from './questions-logic';
import { WorkItem } from './work-logic';
import { WorkStore } from './work.store';

const DISMISSED_KEY = 'konsert.needsyou.dismissed.v1';
/** Vetoes stay in Needs you this long unless acknowledged. */
export const VETO_WINDOW_MS = 2 * 60 * 60 * 1000;
/** How long an answered item's receipt shows before the dialog moves on. */
export const ADVANCE_DELAY_MS = 1200;
/** How long "All caught up" shows before the dialog closes. */
export const CAUGHT_UP_MS = 1600;

export type NeedsYouKind = 'question' | 'request' | 'veto' | 'review' | 'override';

/** One thing that wants the user, as every list and the dialog show it. */
export interface NeedsYouEntry {
  /** q:<question id> · r:<request id> · v:<event id> · rv:<work item id> */
  key: string;
  kind: NeedsYouKind;
  /** P1 interrupt, P2 decision, P3 notify (ui-paradigms §4.8); ASK for tool approvals. */
  tag: 'P1' | 'P2' | 'P3' | 'ASK';
  title: string;
  meta: string;
  at: string;
  /** Short id shown and used as a test hook (q-…, rq-…, t-…). */
  sid: string;
  /** Still waiting on the user (answered questions keep their receipt a moment). */
  pending: boolean;
  /** The task or chat it is about. */
  sessionId?: string;
  question?: QuestionView;
  request?: UserRequest;
  veto?: FleetEvent;
  review?: WorkItem;
}

function loadDismissed(): Record<string, number> {
  try {
    return JSON.parse(localStorage.getItem(DISMISSED_KEY) || '{}') as Record<string, number>;
  } catch {
    return {};
  }
}

/** Recent vetoes the user has not acknowledged yet, newest first. */
export function recentVetoes(events: readonly FleetEvent[], dismissed: Record<string, number>, now = Date.now()): FleetEvent[] {
  return events.filter(
    (e) => e.kind === 'veto' && !dismissed[`veto:${e.id}`] && now - Date.parse(e.created_at) < VETO_WINDOW_MS,
  );
}

/** A veto event's reason label and note (from its meta). */
export function vetoDetails(v: FleetEvent): { reason: string; note: string } {
  let meta: { reason?: string; note?: string } = {};
  try {
    meta = v.meta_json ? (JSON.parse(v.meta_json) as typeof meta) : {};
  } catch {
    meta = {};
  }
  const id = meta.reason || v.action || '';
  return { reason: VETO_REASONS.find((r) => r.id === id)?.label ?? id.replace(/_/g, ' '), note: (meta.note || '').trim() };
}

/**
 * Picks where the dialog goes after `current` was dealt with: the next pending key after it
 * in the order the user has been walking (wrapping), else any pending key, else none.
 */
export function nextPendingKey(order: readonly string[], pending: readonly string[], current: string): string | null {
  const live = new Set(pending.filter((k) => k !== current));
  if (live.size === 0) return null;
  const i = order.indexOf(current);
  for (let n = 1; n <= order.length; n++) {
    const k = order[(i + n + order.length) % order.length];
    if (live.has(k)) return k;
  }
  return pending.find((k) => k !== current) ?? null;
}

/**
 * Everything that wants the user — questions, agents' requests, vetoes awaiting your
 * acknowledgement, reviews ready (override requests slot in once the hub has them) — as one
 * list of entries, and the one dialog that opens on any of them and walks the queue.
 */
@Injectable({ providedIn: 'root' })
export class NeedsYouStore {
  private readonly questionsStore = inject(QuestionsStore);
  private readonly control = inject(ControlStore);
  private readonly work = inject(WorkStore);
  private readonly fleet = inject(FleetStore);

  private readonly dismissed = signal<Record<string, number>>(loadDismissed());
  /** Clock for "blocked 12m" and the veto window; ticked by views that show it. */
  readonly now = signal(Date.now());

  readonly questions = computed(() => this.questionsStore.pending());
  readonly requests = computed(() => this.control.requests());
  readonly holds = computed(() => this.control.holds());
  readonly vetoes = computed(() => recentVetoes(this.control.events(), this.dismissed(), this.now()));
  readonly reviews = computed(() => this.work.reviews());
  /** Override requests (break-glass) from the orchestrator: no hub API yet. */
  readonly overrides = computed<never[]>(() => []);

  /** Pending entries in priority order: vetoes, questions, requests, reviews. */
  readonly entries = computed<NeedsYouEntry[]>(() => {
    const now = this.now();
    const names = this.fleet.workersById();
    const byId = this.work.byId();
    const ago = (iso: string) => `${formatSince(iso, now)} ago`;
    const out: NeedsYouEntry[] = [];
    for (const v of this.vetoes()) {
      const id = v.task_id || v.chat_id || '';
      const it = id ? byId[id] : undefined;
      const d = vetoDetails(v);
      out.push({
        key: `v:${v.id}`,
        kind: 'veto',
        tag: 'P1',
        title: it ? `Vetoed: ${it.title}` : v.summary.replace(/^The user vetoed /, 'Vetoed ').split(' — ')[0],
        meta: [it?.sid, d.reason, ago(v.created_at), 'enforced'].filter(Boolean).join(' · '),
        at: v.created_at,
        sid: it?.sid ?? `v-${v.id}`,
        pending: true,
        sessionId: id || undefined,
        veto: v,
      });
    }
    for (const q of this.questionsStore.pending()) out.push(this.questionEntry(q));
    for (const r of this.requests()) {
      out.push({
        key: `r:${r.id}`,
        kind: 'request',
        tag: 'P2',
        title: r.description,
        meta: ['request', names[r.worker_id]?.name, resourceLabel(r.resource), ago(r.created_at)].filter(Boolean).join(' · '),
        at: r.created_at,
        sid: r.sid,
        pending: true,
        sessionId: r.task_id || r.chat_id || undefined,
        request: r,
      });
    }
    for (const it of this.reviews()) {
      out.push({
        key: `rv:${it.id}`,
        kind: 'review',
        tag: 'P3',
        title: `Review: ${it.title}`,
        meta: [it.sid, it.agent, it.machine, 'ready'].filter(Boolean).join(' · '),
        at: it.finishedAt || it.updatedAt,
        sid: it.sid,
        pending: true,
        sessionId: it.id,
        review: it,
      });
    }
    return out;
  });

  readonly count = computed(() => this.entries().length);
  /** Items that block an agent right now (questions and requests). */
  readonly blocking = computed(() => this.questions().length + this.requests().length);
  readonly empty = computed(() => this.count() === 0 && this.holds().length === 0);

  private questionEntry(q: QuestionView): NeedsYouEntry {
    const who = q.orchestrator ? 'orchestrator' : q.agent || 'agent';
    return {
      key: `q:${q.id}`,
      kind: 'question',
      tag: q.kind === 'permission' ? 'ASK' : 'P2',
      title: q.prompt,
      meta: [who, q.chat_sid, q.machine_name, isPending(q) ? `blocked ${formatSince(q.blocked_since, this.now())}` : 'answered'].filter(Boolean).join(' · '),
      at: q.blocked_since,
      sid: q.sid,
      pending: isPending(q),
      sessionId: q.task_id || q.chat_id || undefined,
      question: q,
    };
  }

  /** The entry for a task or chat (its question, else its request, else its review). */
  entryFor(sessionId: string): NeedsYouEntry | undefined {
    const list = this.entries().filter((e) => e.sessionId === sessionId);
    return list.find((e) => e.kind === 'question') ?? list.find((e) => e.kind === 'request') ?? list[0];
  }

  // --- the dialog -------------------------------------------------------------------------

  /** The key the dialog shows (null: closed). Questions follow QuestionsStore's selection. */
  private readonly openKey = signal<string | null>(null);
  /** The order the user is walking (the queue when the dialog opened, plus arrivals). */
  private readonly order = signal<string[]>([]);
  /** "All caught up": shown briefly after the last item, then the dialog closes. */
  readonly caughtUp = signal(false);
  private timer: ReturnType<typeof setTimeout> | null = null;

  /** The Questions entry points (chat cards, the classic button) open the same dialog. */
  readonly isOpen = computed(() => this.openKey() !== null || this.questionsStore.panelOpen());

  readonly currentKey = computed<string | null>(() => {
    const k = this.openKey();
    if (k && !k.startsWith('q:')) return k;
    if (k || this.questionsStore.panelOpen()) {
      const id = this.questionsStore.selectedId();
      if (id) return `q:${id}`;
      return this.entries()[0]?.key ?? null;
    }
    return null;
  });

  /** The dialog's queue: the walk order (still pending, or the current one), then arrivals. */
  readonly queue = computed<NeedsYouEntry[]>(() => {
    const pending = this.entries();
    const byKey = new Map(pending.map((e) => [e.key, e]));
    const cur = this.currentKey();
    if (cur && !byKey.has(cur)) {
      const e = this.lookup(cur);
      if (e) byKey.set(cur, e);
    }
    const keys = [...this.order().filter((k) => byKey.has(k)), ...pending.map((e) => e.key).filter((k) => !this.order().includes(k))];
    if (cur && byKey.has(cur) && !keys.includes(cur)) keys.unshift(cur);
    return keys.map((k) => byKey.get(k)!);
  });

  /** The entry last shown, so a resolved request or review keeps its card while the dialog moves on. */
  private readonly shown = signal<NeedsYouEntry | null>(null);

  readonly current = computed<NeedsYouEntry | null>(() => {
    const k = this.currentKey();
    if (!k) return null;
    const live = this.queue().find((e) => e.key === k) ?? this.lookup(k);
    if (live) return live;
    const last = this.shown();
    return last?.key === k ? { ...last, pending: false } : null;
  });

  readonly position = computed(() => {
    const k = this.currentKey();
    const i = this.queue().findIndex((e) => e.key === k);
    return { index: i, total: this.queue().length };
  });

  /** An entry that is no longer pending (an answered question keeps showing its receipt). */
  private lookup(key: string): NeedsYouEntry | null {
    if (key.startsWith('q:')) {
      const q = this.questionsStore.question(key.slice(2));
      return q ? this.questionEntry(q) : null;
    }
    return null;
  }

  private remember(key: string | null): void {
    this.shown.set((key && this.entries().find((e) => e.key === key)) || null);
  }

  /** Opens the dialog on an entry (or the first one). */
  open(key?: string): void {
    this.clearTimer();
    this.caughtUp.set(false);
    const target = key ?? this.entries()[0]?.key ?? null;
    if (!this.isOpen()) this.order.set(this.entries().map((e) => e.key));
    if (target?.startsWith('q:')) this.questionsStore.open(target.slice(2));
    else this.questionsStore.threadOpen.set(false);
    this.remember(target);
    this.openKey.set(target ?? 'none');
  }

  close(): void {
    this.clearTimer();
    this.caughtUp.set(false);
    this.openKey.set(null);
    this.questionsStore.close();
  }

  /** j/k, arrows, swipes: the previous or next entry in the queue. */
  move(delta: number): void {
    const q = this.queue();
    if (q.length === 0) return;
    const i = this.position().index;
    const next = q[Math.min(q.length - 1, Math.max(0, (i < 0 ? 0 : i) + delta))];
    if (next && next.key !== this.currentKey()) this.go(next.key);
  }

  /** Shows an entry without reshuffling the walk. */
  go(key: string): void {
    this.clearTimer();
    if (key.startsWith('q:')) this.questionsStore.select(key.slice(2));
    else this.questionsStore.threadOpen.set(false);
    this.remember(key);
    this.openKey.set(key);
  }

  /**
   * The current entry was dealt with (answered, decided, snoozed, acknowledged): after its
   * receipt shows, move to the next unanswered one, or say "All caught up" and close.
   */
  advance(from = this.currentKey(), delay = ADVANCE_DELAY_MS): void {
    if (!from) return;
    this.clearTimer();
    this.timer = setTimeout(() => {
      this.timer = null;
      if (this.currentKey() !== from || !this.isOpen()) return;
      // An answer still being prepared (a side-conversation summary): wait for its receipt.
      if (this.current()?.question?.answer?.preparing) {
        this.advance(from, delay);
        return;
      }
      const next = nextPendingKey(
        this.queue().map((e) => e.key),
        this.entries().map((e) => e.key),
        from,
      );
      if (next) {
        this.go(next);
        return;
      }
      this.caughtUp.set(true);
      this.timer = setTimeout(() => this.close(), CAUGHT_UP_MS);
    }, delay);
  }

  private clearTimer(): void {
    if (this.timer) clearTimeout(this.timer);
    this.timer = null;
  }

  // --- actions on non-question entries ---------------------------------------------------

  /** Acknowledges a veto receipt (it leaves Needs you). */
  dismissVeto(ev: FleetEvent): void {
    this.dismissed.update((d) => {
      const next: Record<string, number> = { [`veto:${ev.id}`]: Date.now() };
      // Forget acknowledgements older than the window so the map stays small.
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

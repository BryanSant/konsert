import { Injectable, computed, inject, signal } from '@angular/core';
import { ShellReminder } from '../models/activity.models';
import { Hold, OverrideRequest, UserRequest, VETO_REASONS, Veto, formatDuration, requestImpact, resourceLabel } from '../models/control.models';
import { QuestionView, isPending } from '../models/question.models';
import { ControlStore } from './control.store';
import { FleetStore } from './fleet.store';
import { QuestionsStore } from './questions.store';
import { formatSince, isSnoozed } from './questions-logic';
import { WorkItem } from './work-logic';
import { WorkStore } from './work.store';

/** How long an answered item's receipt shows before the dialog moves on. */
export const ADVANCE_DELAY_MS = 1200;
/** How long "All caught up" shows before the dialog closes. */
export const CAUGHT_UP_MS = 1600;

export type NeedsYouKind = 'question' | 'request' | 'veto' | 'review' | 'override' | 'shells';

/** One thing that wants the user, as every list and the dialog show it. */
export interface NeedsYouEntry {
  /** q:<question id> · r:<request id> · v:<veto id> · o:<override id> · rv:<work item id> · sh:<session key> */
  key: string;
  kind: NeedsYouKind;
  /** P1 interrupt, P2 decision, P3 notify (ui-paradigms §4.8); ASK for tool approvals. */
  tag: 'P1' | 'P2' | 'P3' | 'ASK';
  title: string;
  meta: string;
  at: string;
  /** Short id shown and used as a test hook (q-…, rq-…, o-…, t-…). */
  sid: string;
  /** Still waiting on the user (answered questions keep their receipt a moment). */
  pending: boolean;
  /** The task or chat it is about. */
  sessionId?: string;
  /** Blocking impact (questions and open requests: same scale as sortQuestions). */
  impact?: number;
  question?: QuestionView;
  request?: UserRequest;
  veto?: Veto;
  override?: OverrideRequest;
  review?: WorkItem;
  shells?: ShellReminder;
}

/** One line for a shells reminder's list: machine, how long idle, and the policy note. */
export function shellsMeta(r: ShellReminder, machine: string | undefined, now = Date.now()): string {
  return [machine, `idle ${formatSince(r.idle_since, now)}`, r.note].filter(Boolean).join(' · ');
}

/** A veto's reason label and note. */
export function vetoDetails(v: Pick<Veto, 'reason' | 'note'>): { reason: string; note: string } {
  const id = v.reason || '';
  return { reason: VETO_REASONS.find((r) => r.id === id)?.label ?? id.replace(/_/g, ' '), note: (v.note || '').trim() };
}

/** Whether a veto went unacknowledged past its deadline (the hub says so, or the clock does). */
export function vetoEscalated(v: Veto, now = Date.now()): boolean {
  return !!v.escalated_at || (!v.acked_at && !v.dismissed_at && !!v.ack_deadline && now >= Date.parse(v.ack_deadline));
}

/** One line for what an override request asks: "Allow 1 extra session on mac for 30m". */
export function overrideTitle(o: OverrideRequest, machine?: string, hold?: Hold): string {
  const where = machine || 'the machine';
  const dur = formatDuration(o.duration_seconds);
  switch (o.scope_kind) {
    case 'machine_slots': {
      const n = o.extra_slots || 1;
      return `Allow ${n} extra session${n === 1 ? '' : 's'} on ${where} for ${dur}`;
    }
    case 'hold':
      return hold ? `Lift hold ${hold.sid} on ${where} for ${dur}` : `Lift a hold on ${where} for ${dur}`;
    case 'session':
      return `Lift the holds on a session on ${where} for ${dur}`;
    case 'machine':
      return `Lift the holds on ${where} for ${dur}`;
  }
  return `Override for ${dur}`;
}

/** What an override's scope covers, for the dialog's Scope row. */
export function overrideScope(o: OverrideRequest, machine?: string, hold?: Hold, session?: string): string {
  const where = machine || 'the machine';
  switch (o.scope_kind) {
    case 'machine_slots': {
      const n = o.extra_slots || 1;
      return `${where}: ${n} more session${n === 1 ? '' : 's'} at once than its slot limit`;
    }
    case 'hold':
      return hold ? `Hold ${hold.sid} on ${where}: ${hold.summary}` : `A hold on ${where}`;
    case 'session':
      return `The holds on ${session || 'one session'} (${where})`;
    case 'machine':
      return `Every machine and resource hold on ${where}`;
  }
  return o.scope_kind;
}

/**
 * Orders blocking queue items (questions and open requests) with the question queue's
 * conventions: active before snoozed, higher blocking impact first, then oldest first
 * (longest blocked), with key as a stable tie-breaker.
 */
export function sortBlockingEntries(list: readonly NeedsYouEntry[], now = Date.now()): NeedsYouEntry[] {
  const rank = (e: NeedsYouEntry) => (e.question && isSnoozed(e.question, now) ? 1 : 0);
  return [...list].sort((a, b) => {
    const ra = rank(a);
    const rb = rank(b);
    if (ra !== rb) return ra - rb;
    const ia = a.impact ?? 1;
    const ib = b.impact ?? 1;
    if (ia !== ib) return ib - ia;
    const d = Date.parse(a.at) - Date.parse(b.at);
    if (!Number.isNaN(d) && d !== 0) return d;
    return a.key.localeCompare(b.key);
  });
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
 * Everything that wants the user — questions, agents' requests, the commander's override
 * requests, vetoes the agent has not acknowledged yet, reviews ready — as one list of
 * entries, and the one dialog that opens on any of them and walks the queue.
 */
@Injectable({ providedIn: 'root' })
export class NeedsYouStore {
  private readonly questionsStore = inject(QuestionsStore);
  private readonly control = inject(ControlStore);
  private readonly work = inject(WorkStore);
  private readonly fleet = inject(FleetStore);

  /** Clock for "blocked 12m" and veto escalation; ticked by views that show it. */
  readonly now = signal(Date.now());

  readonly questions = computed(() => this.questionsStore.pending());
  readonly requests = computed(() => this.control.requests());
  readonly holds = computed(() => this.control.holds());
  /** Vetoes the agent has not acknowledged yet (they resolve themselves once it does). */
  readonly vetoes = computed(() => this.control.vetoes());
  readonly reviews = computed(() => this.work.reviews());
  /** Override requests (break-glass) from the commander, waiting for a decision. */
  readonly overrides = computed(() => this.control.overrides());

  /**
   * Pending entries in priority order: vetoes nobody acknowledged, questions, requests,
   * override requests, vetoes still waiting for the agent, reviews.
   */
  readonly entries = computed<NeedsYouEntry[]>(() => {
    const now = this.now();
    const names = this.fleet.workersById();
    const byId = this.work.byId();
    const holds = this.holds();
    const ago = (iso: string) => `${formatSince(iso, now)} ago`;
    const escalated: NeedsYouEntry[] = [];
    const waiting: NeedsYouEntry[] = [];
    for (const v of this.vetoes()) {
      const id = v.chat_id && !v.task_id ? v.chat_id : v.task_id || '';
      const it = id ? byId[id] : undefined;
      const d = vetoDetails(v);
      const late = vetoEscalated(v, now);
      const state = late ? 'not acknowledged' : v.delivered_at ? 'waiting for the agent' : 'sent';
      (late ? escalated : waiting).push({
        key: `v:${v.id}`,
        kind: 'veto',
        tag: late ? 'P1' : 'P3',
        title: it ? `Vetoed: ${it.title}` : v.summary.replace(/^The user vetoed /, 'Vetoed ').split(' — ')[0],
        meta: [it?.sid, d.reason, ago(v.created_at), state].filter(Boolean).join(' · '),
        at: v.created_at,
        sid: v.sid,
        pending: true,
        sessionId: id || undefined,
        veto: v,
      });
    }
    const out: NeedsYouEntry[] = [...escalated];
    const blocking: NeedsYouEntry[] = [];
    for (const q of this.questionsStore.pending()) blocking.push(this.questionEntry(q));
    for (const r of this.requests()) blocking.push(this.requestEntry(r, now, names));
    out.push(...sortBlockingEntries(blocking, now));
    for (const o of this.overrides()) {
      const machine = o.worker_id ? names[o.worker_id]?.name : undefined;
      out.push({
        key: `o:${o.id}`,
        kind: 'override',
        tag: 'P2',
        title: overrideTitle(o, machine, holds.find((h) => h.id === o.hold_id)),
        meta: ['override', 'commander', machine, formatDuration(o.duration_seconds), ago(o.created_at)].filter(Boolean).join(' · '),
        at: o.created_at,
        sid: o.sid,
        pending: true,
        sessionId: o.task_id || o.target_chat_id || undefined,
        override: o,
      });
    }
    out.push(...waiting);
    for (const r of this.control.shellReminders()) {
      out.push({
        key: `sh:${r.id}`,
        kind: 'shells',
        tag: 'P3',
        title: r.title,
        meta: shellsMeta(r, r.worker_id ? names[r.worker_id]?.name : undefined, now),
        at: r.created_at,
        sid: r.id.replace(':', '-'),
        pending: true,
        sessionId: r.task_id || r.chat_id || undefined,
        shells: r,
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
    const who = q.commander ? 'commander' : q.agent || 'agent';
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
      impact: q.impact,
      question: q,
    };
  }

  private requestEntry(
    r: UserRequest,
    now: number,
    names: Record<string, { name?: string } | undefined>,
  ): NeedsYouEntry {
    const machine = r.machine_name || names[r.worker_id]?.name;
    const who = r.commander ? 'commander' : undefined;
    return {
      key: `r:${r.id}`,
      kind: 'request',
      tag: 'P2',
      title: r.description,
      meta: ['request', who, r.chat_sid, machine, resourceLabel(r.resource), `blocked ${formatSince(r.created_at, now)}`].filter(Boolean).join(' · '),
      at: r.created_at,
      sid: r.sid,
      pending: r.status === 'open',
      sessionId: r.task_id || r.chat_id || undefined,
      impact: requestImpact(r),
      request: r,
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

  /** Dismisses a veto the agent never acknowledged (it leaves Needs you). */
  dismissVeto(v: Veto): Promise<boolean> {
    return this.control.dismissVeto(v);
  }

  /** Approves or denies an override request; the commander is told either way. */
  decideOverride(o: OverrideRequest, decision: 'approve' | 'deny', note?: string): Promise<boolean> {
    return this.control.decideOverride(o, decision, note?.trim() || undefined);
  }
}

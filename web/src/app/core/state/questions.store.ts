import { Injectable, computed, inject, signal } from '@angular/core';
import { QuestionAnswerRequest, QuestionView, isPending } from '../models/question.models';
import { ApiClientService } from '../services/api-client.service';
import { QuestionGroup, groupQuestions, moveIndex, sortQuestions } from './questions-logic';

/**
 * The fleet-wide question queue (PRD §5.8b): every open question, sorted by blocking
 * impact then age, grouped by ticket/chat. Loaded over REST and kept current by
 * fleet.update frames of kind "question". Nothing leaves the queue until it is answered
 * (or the agent no longer needs it); answered questions stay a while so their receipt
 * ("Sent … with a summary of your side conversation") can show.
 */
@Injectable({ providedIn: 'root' })
export class QuestionsStore {
  private readonly api = inject(ApiClientService);

  /** Every question known to this page, by id. */
  readonly byId = signal<Record<string, QuestionView>>({});
  /** The question shown in the panel. */
  readonly selectedId = signal<string | null>(null);
  /** The Questions panel is open. */
  readonly panelOpen = signal(false);
  /** The side conversation of the selected question is shown. */
  readonly threadOpen = signal(false);
  readonly error = signal('');
  /** Question ids with an action in flight. */
  readonly busy = signal<Record<string, boolean>>({});
  /** Clock for "blocked 12m" labels (ticks while the panel is open). */
  readonly now = signal(Date.now());

  readonly all = computed(() => sortQuestions(Object.values(this.byId()), this.now()));
  readonly pending = computed(() => this.all().filter((q) => isPending(q)));
  readonly pendingCount = computed(() => this.pending().length);
  /** Pending questions plus ones answered from this page (for their receipts). */
  readonly visible = computed(() => this.all().filter((q) => isPending(q) || this.recent().has(q.id)));
  readonly groups = computed<QuestionGroup[]>(() => groupQuestions(this.visible()));
  readonly selected = computed(() => {
    const id = this.selectedId();
    return (id && this.byId()[id]) || null;
  });

  /** Ids answered (or delegated) from this page; kept visible with their receipt. */
  private readonly recent = signal<Set<string>>(new Set());
  private timer: ReturnType<typeof setInterval> | null = null;

  question(id: string | undefined | null): QuestionView | null {
    return (id && this.byId()[id]) || null;
  }

  isBusy(id: string): boolean {
    return !!this.busy()[id];
  }

  async load(): Promise<void> {
    try {
      const res = await this.api.listQuestions();
      const next: Record<string, QuestionView> = {};
      // Keep closed questions answered from this page (their receipt) and the selection.
      for (const [id, q] of Object.entries(this.byId())) {
        if (this.recent().has(id) || id === this.selectedId()) next[id] = q;
      }
      for (const q of res.questions ?? []) {
        // A push that arrived while the request was in flight may be newer.
        const cur = this.byId()[q.id];
        next[q.id] = cur && Date.parse(cur.updated_at) > Date.parse(q.updated_at) ? cur : normalize(q);
      }
      this.byId.set(next);
      this.error.set('');
    } catch (err) {
      this.error.set(errText(err, 'Could not load questions'));
    }
  }

  /** A question pushed by the hub (fleet.update kind "question"). */
  apply(q: QuestionView | undefined | null): void {
    if (!q?.id) return;
    this.byId.update((m) => ({ ...m, [q.id]: normalize(q) }));
  }

  // --- panel and selection ---

  open(id?: string): void {
    this.panelOpen.set(true);
    this.startClock();
    if (id) this.select(id);
    else if (!this.selected() || !isPending(this.selected()!)) this.selectedId.set(this.pending()[0]?.id ?? null);
    void this.load();
  }

  close(): void {
    this.panelOpen.set(false);
    this.threadOpen.set(false);
    this.stopClock();
  }

  select(id: string | null): void {
    if (id !== this.selectedId()) this.threadOpen.set(!!(id && this.byId()[id]?.thread && !this.byId()[id]?.thread?.discarded));
    this.selectedId.set(id);
  }

  /** j/k: move the selection through the visible queue. */
  move(delta: number): void {
    const sel = this.selected();
    if (sel && !isPending(sel) && this.pending().length) {
      // From a just-answered question (its receipt sorts last), go back to the queue.
      const p = this.pending();
      this.select(delta > 0 ? p[0].id : p[p.length - 1].id);
      return;
    }
    const list = this.visible();
    const cur = list.findIndex((q) => q.id === this.selectedId());
    const i = moveIndex(cur, delta, list.length);
    this.select(i >= 0 ? list[i].id : null);
  }

  /** After answering, move on to the next pending question (the receipt stays on the old one). */
  private advanceFrom(id: string): void {
    const list = this.pending().filter((q) => q.id !== id);
    if (this.selectedId() === id && list.length > 0) {
      // Leave the receipt visible briefly, then move on.
      setTimeout(() => {
        if (this.selectedId() === id) this.select(this.pending()[0]?.id ?? id);
      }, 1500);
    }
  }

  // --- actions ---

  private async run(id: string, fallback: string, fn: () => Promise<QuestionView>): Promise<boolean> {
    if (this.isBusy(id)) return false;
    this.busy.update((b) => ({ ...b, [id]: true }));
    this.error.set('');
    try {
      this.apply(await fn());
      return true;
    } catch (err) {
      this.error.set(errText(err, fallback));
      return false;
    } finally {
      this.busy.update((b) => {
        const next = { ...b };
        delete next[id];
        return next;
      });
    }
  }

  async answer(q: QuestionView, req: QuestionAnswerRequest): Promise<boolean> {
    const ok = await this.run(q.id, 'Could not send the answer', () => this.api.answerQuestion(q.id, req));
    if (ok) {
      this.recent.update((s) => new Set(s).add(q.id));
      this.advanceFrom(q.id);
    }
    return ok;
  }

  /** Picks option `index` (0-based) of a question (keyboard 1–9). */
  pick(q: QuestionView, index: number): Promise<boolean> {
    const o = q.options?.[index];
    if (!o || !this.canAnswer(q)) return Promise.resolve(false);
    return this.answer(q, { option_id: o.id });
  }

  async ask(q: QuestionView, text: string): Promise<boolean> {
    const t = text.trim();
    if (!t) return false;
    this.threadOpen.set(true);
    return this.run(q.id, 'Could not ask the question', () => this.api.askAboutQuestion(q.id, t));
  }

  async delegate(q: QuestionView): Promise<boolean> {
    const ok = await this.run(q.id, 'Could not hand the question to the orchestrator', () => this.api.delegateQuestion(q.id));
    if (ok) this.recent.update((s) => new Set(s).add(q.id));
    return ok;
  }

  snooze(q: QuestionView, minutes: number): Promise<boolean> {
    return this.run(q.id, 'Could not snooze the question', () => this.api.snoozeQuestion(q.id, minutes));
  }

  /** Whether the user can (still) answer: pending, or an orchestrator answer to override. */
  canAnswer(q: QuestionView): boolean {
    if (isPending(q)) return true;
    return q.status === 'answered' && q.answer?.by?.actor === 'orchestrator' && (q.source === 'ask_user' || !q.answer.delivered_at);
  }

  private startClock(): void {
    if (this.timer || typeof setInterval === 'undefined') return;
    this.now.set(Date.now());
    this.timer = setInterval(() => this.now.set(Date.now()), 15_000);
  }

  private stopClock(): void {
    if (this.timer) clearInterval(this.timer);
    this.timer = null;
  }
}

function normalize(q: QuestionView): QuestionView {
  return { ...q, options: q.options ?? [], refs: q.refs ?? [], blocks: q.blocks ?? [], messages: q.messages ?? [] };
}

function errText(err: unknown, fallback: string): string {
  return err instanceof Error && err.message ? err.message : fallback;
}

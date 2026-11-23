/**
 * Pure helpers for the question queue (no Angular): ordering, grouping, time and
 * receipt texts. The store keeps the state; these are unit-tested on their own.
 */
import { QuestionAnswer, QuestionView, isPending } from '../models/question.models';

export interface QuestionGroup {
  key: string;
  label: string;
  items: QuestionView[];
}

export function isSnoozed(q: Pick<QuestionView, 'snoozed_until'>, now = Date.now()): boolean {
  return !!q.snoozed_until && Date.parse(q.snoozed_until) > now;
}

/**
 * Queue order (same as the hub): pending before closed; snoozed last among pending;
 * then blocking impact (high first), then how long it has blocked (oldest first).
 */
export function sortQuestions(list: QuestionView[], now = Date.now()): QuestionView[] {
  const rank = (q: QuestionView) => (!isPending(q) ? 2 : isSnoozed(q, now) ? 1 : 0);
  return [...list].sort((a, b) => {
    const ra = rank(a);
    const rb = rank(b);
    if (ra !== rb) return ra - rb;
    if (ra === 2) return Date.parse(b.updated_at) - Date.parse(a.updated_at);
    if (a.impact !== b.impact) return b.impact - a.impact;
    const d = Date.parse(a.blocked_since) - Date.parse(b.blocked_since);
    return d !== 0 ? d : a.id.localeCompare(b.id);
  });
}

/** Groups (by ticket, else chat) in queue order: a group sits where its first question is. */
export function groupQuestions(sorted: QuestionView[]): QuestionGroup[] {
  const groups: QuestionGroup[] = [];
  const byKey = new Map<string, QuestionGroup>();
  for (const q of sorted) {
    const key = q.group_key || `chat:${q.chat_id}`;
    let g = byKey.get(key);
    if (!g) {
      g = { key, label: q.group_label || q.chat_title || q.chat_sid || 'Questions', items: [] };
      byKey.set(key, g);
      groups.push(g);
    }
    g.items.push(q);
  }
  return groups;
}

/** "45s", "12m", "3h 5m", "2d" since an ISO time. */
export function formatSince(iso: string | undefined, now = Date.now()): string {
  if (!iso) return '';
  const s = Math.max(0, Math.round((now - Date.parse(iso)) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return m % 60 ? `${h}h ${m % 60}m` : `${h}h`;
  return `${Math.floor(h / 24)}d`;
}

/** "12m 30s" for durations in seconds (metrics). */
export function formatDuration(seconds: number): string {
  if (!seconds || seconds < 1) return '0s';
  const s = Math.round(seconds);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return s % 60 ? `${m}m ${s % 60}s` : `${m}m`;
  const h = Math.floor(m / 60);
  return m % 60 ? `${h}h ${m % 60}m` : `${h}h`;
}

/** What the answer was, for receipts ("Yes", or the free text). */
export function answerLabel(a: QuestionAnswer | undefined): string {
  if (!a) return '';
  if (a.label && a.text) return `${a.label} — ${a.text}`;
  return a.label || a.text || '';
}

/** The confirmation after answering ("Sent "Yes" with a summary of your side conversation"). */
export function sentMessage(q: QuestionView): string {
  const a = q.answer;
  if (!a) return '';
  const what = `“${answerLabel(a)}”`;
  if (a.correction) return `Sent the correction ${what} to the agent`;
  if (a.preparing) return `Sending ${what}… writing a summary of your side conversation`;
  const by = a.by?.actor === 'orchestrator' ? 'The orchestrator sent' : 'Sent';
  const summary = a.summary ? ' with a summary of your side conversation' : '';
  if (!a.delivered_at) return `${by} ${what}${summary}; the agent gets it before its next turn`;
  const how = a.delivery === 'note' ? ' (as a note before its next turn)' : '';
  return `${by} ${what}${summary}${how}`;
}

/** Index after moving by delta in a list of n (clamped). */
export function moveIndex(cur: number, delta: number, n: number): number {
  if (n <= 0) return -1;
  if (cur < 0) return delta > 0 ? 0 : n - 1;
  return Math.min(n - 1, Math.max(0, cur + delta));
}

/** Keyboard action of the question queue for a key event (null = not ours). */
export type QueueKey =
  | { kind: 'move'; delta: number }
  | { kind: 'pick'; index: number }
  | { kind: 'ask' }
  | { kind: 'delegate' };

export function queueKey(ev: Pick<KeyboardEvent, 'key' | 'metaKey' | 'ctrlKey' | 'altKey'>): QueueKey | null {
  if (ev.metaKey || ev.ctrlKey || ev.altKey) return null;
  switch (ev.key) {
    case 'j':
    case 'ArrowDown':
      return { kind: 'move', delta: 1 };
    case 'k':
    case 'ArrowUp':
      return { kind: 'move', delta: -1 };
    case '?':
      return { kind: 'ask' };
    case 'o':
      return { kind: 'delegate' };
  }
  if (/^[1-9]$/.test(ev.key)) return { kind: 'pick', index: Number(ev.key) - 1 };
  return null;
}

/** True when a key event comes from a text field (the queue's keys must not fire). */
export function fromTextField(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null;
  if (!el || typeof el.tagName !== 'string') return false;
  const tag = el.tagName.toLowerCase();
  return tag === 'input' || tag === 'textarea' || tag === 'select' || el.isContentEditable;
}

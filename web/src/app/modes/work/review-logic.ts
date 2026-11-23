import { TaskEvent } from '../../core/models/konsert.models';
import { WorkItem, parseGitSummary } from '../../core/state/work-logic';

export interface ReceiptLine {
  text: string;
  kind: 'commit' | 'diff' | 'summary' | 'milestone';
}

/**
 * Acceptance criteria as the work's prompt states them: its bullet or numbered lines, or
 * the goal itself when there are none. (Tickets with real acceptance lists replace this
 * once the hub links work to them.)
 */
export function acceptanceFrom(prompt: string | undefined, goal: string): string[] {
  const lines = (prompt ?? '')
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => /^([-*•]|\d+[.)]|\[[ x]\])\s+/i.test(l))
    .map((l) => l.replace(/^([-*•]|\d+[.)]|\[[ x]\])\s+/i, '').trim())
    .filter(Boolean);
  if (lines.length) return lines.slice(0, 12);
  return goal ? [goal] : [];
}

/** What the agent can show for its work: commits, the diff stat, milestones, its summary. */
export function receiptsFor(item: WorkItem, events: readonly TaskEvent[] = []): ReceiptLine[] {
  const out: ReceiptLine[] = [];
  const g = parseGitSummary(item.task?.git_summary_json);
  for (const c of g?.commits ?? []) out.push({ kind: 'commit', text: c });
  if (g?.diff_stat?.trim()) out.push({ kind: 'diff', text: g.diff_stat.trim().split('\n').pop()!.trim() });
  const dirty = (g?.dirty_files ?? []).map((f) => f.replace(/^\s*\S{1,2}\s+/, '').trim()).filter(Boolean);
  if (dirty.length) {
    const shown = dirty.slice(0, 6).join(', ');
    out.push({ kind: 'diff', text: `Uncommitted: ${shown}${dirty.length > 6 ? ` +${dirty.length - 6} more` : ''}` });
  }
  for (const e of events) {
    if (e.kind === 'milestone' && e.message.trim()) out.push({ kind: 'milestone', text: e.message.trim() });
  }
  const summary = item.task?.summary?.trim();
  if (summary) out.push({ kind: 'summary', text: summary.length > 400 ? `${summary.slice(0, 399)}…` : summary });
  return out.slice(-20);
}

/** The prompt for a follow-up session that applies requested changes to finished work. */
export function changeRequestPrompt(item: WorkItem, note: string): string {
  const original = item.task?.prompt?.trim() || item.goal;
  return [
    `Changes requested on ${item.sid} ("${item.title}"):`,
    note.trim(),
    '',
    'The original task was:',
    original,
  ].join('\n');
}

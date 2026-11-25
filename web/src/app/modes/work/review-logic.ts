import { TaskEvent } from '../../core/models/syre.models';
import { WorkItem, parseGitSummary } from '../../core/state/work-logic';

export interface ReceiptLine {
  text: string;
  kind: 'commit' | 'diff' | 'summary' | 'milestone';
}

// Acceptance criteria come from the hub (GET /api/tasks/{id}/acceptance: the linked ka
// ticket's items, else the prompt's bullets); requested changes are delivered by the hub
// (POST /api/tasks/{id}/review).

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

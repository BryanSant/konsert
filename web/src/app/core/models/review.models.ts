/**
 * Review decisions and Expedite (KON-BM8JR): kept on the hub, one per task or chat, and
 * pushed to every client as fleet.update kind "review". Mirrors internal/store/reviews.go
 * and internal/controller/reviews.go.
 */

export type ReviewDecision = 'approved' | 'changes_requested';

/** What became of a decision on the task's pull request (gh on the owning machine). */
export type GitHubReviewStatus = '' | 'sent' | 'failed' | 'no_github' | 'no_gh' | 'no_pr' | 'offline';

export interface Review {
  id: string;
  sid: string;
  task_id?: string;
  chat_id?: string;
  decision?: ReviewDecision | '';
  note?: string;
  decided_by?: string;
  decided_at?: string;
  github_status?: GitHubReviewStatus;
  github_message?: string;
  github_url?: string;
  follow_up_task_id?: string;
  expedite: boolean;
  expedited_at?: string;
  created_at: string;
  updated_at: string;
}

/** POST /api/tasks/{id}/review */
export interface ReviewResult {
  review: Review;
  follow_up_task_id?: string;
  follow_up_task_sid?: string;
  /** How requested changes reached the agent. */
  delivered?: 'steered' | 'follow_up';
}

export interface AcceptanceItem {
  text: string;
  done: boolean;
}

/** GET /api/tasks/{id}/acceptance: the linked ka ticket's items, else the prompt's bullets. */
export interface Acceptance {
  source: 'ticket' | 'prompt' | 'goal';
  ticket_id?: string;
  ticket_title?: string;
  items: AcceptanceItem[];
}

/** "task:<id>" / "chat:<id>" */
export function reviewKey(r: Pick<Review, 'task_id' | 'chat_id'>): string {
  return r.task_id ? `task:${r.task_id}` : `chat:${r.chat_id ?? ''}`;
}

/** The line shown after a decision: what happened on GitHub. */
export function githubLine(r: Review | undefined): string {
  if (!r?.github_status) return '';
  const what = r.decision === 'changes_requested' ? 'Changes requested' : 'Approved';
  switch (r.github_status) {
    case 'sent':
      return `${what} on GitHub${r.github_url ? `: ${r.github_url}` : '.'}`;
    case 'failed':
      return `GitHub refused it: ${r.github_message || 'gh failed'}`;
    case 'no_github':
      return 'Not sent to GitHub: the checkout has no GitHub remote.';
    case 'no_gh':
      return 'Not sent to GitHub: gh is not installed on that machine.';
    case 'no_pr':
      return 'Not sent to GitHub: there is no pull request for this branch.';
    case 'offline':
      return `Not sent to GitHub: ${r.github_message || 'the machine is offline.'}`;
  }
  return '';
}

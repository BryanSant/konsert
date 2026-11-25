/**
 * Review decisions, delivery outcomes, PR/MR tracking and Expedite (SYR-H50YW): kept on
 * the hub, one per task or chat, and pushed to every client as fleet.update kind "review".
 * Mirrors internal/store/reviews.go and internal/controller/delivery_modes.go.
 */

export type ReviewDecision = 'approved' | 'changes_requested';

/** Delivery mode for a repo/project/global setting ('' = inherit / auto default). */
export type DeliveryMode = '' | 'direct' | 'pr_merge' | 'pr_open' | 'keep_branch';

/** Merge style for direct and PR/MR merges ('' = inherit / default squash). */
export type MergeStyle = '' | 'squash' | 'merge' | 'rebase';

/** Outcome of delivering an approved task (or preparing its PR/MR). */
export type DeliveryStatus =
  | ''
  | 'delivered'
  | 'pr_open'
  | 'auto_merge'
  | 'manual'
  | 'conflict'
  | 'closed'
  | 'failed'
  | 'offline';

export type PRState = '' | 'open' | 'merged' | 'closed';
export type ChecksState = '' | 'none' | 'pending' | 'passed' | 'failed';

export interface Review {
  id: string;
  sid: string;
  task_id?: string;
  chat_id?: string;
  decision?: ReviewDecision | '';
  note?: string;
  decided_by?: string;
  decided_at?: string;
  delivery_mode?: DeliveryMode;
  delivery_status?: DeliveryStatus;
  delivery_message?: string;
  merge_method?: MergeStyle;
  merge_sha?: string;
  pr_number?: number;
  pr_url?: string;
  pr_state?: PRState;
  pr_checks?: ChecksState;
  pr_auto_merge?: boolean;
  compare_url?: string;
  pr_remote_url?: string;
  follow_up_task_id?: string;
  expedite: boolean;
  expedited_at?: string;
  created_at: string;
  updated_at: string;
}

/** GET /api/tasks/{id}/delivery: what Approve will do for a task. */
export interface DeliveryPlan {
  mode: DeliveryMode;
  merge_style: MergeStyle;
  has_remote: boolean;
  remote_url?: string;
  default_branch: string;
  branch?: string;
  can_push: boolean;
  fork?: string;
  has_forge: boolean;
  approve_label: string;
  pr_number?: number;
  pr_url?: string;
  pr_state?: PRState;
  pr_checks?: ChecksState;
  pr_auto_merge?: boolean;
  compare_url?: string;
}

/** POST /api/tasks/{id}/review */
export interface ReviewResult {
  review: Review;
  follow_up_task_id?: string;
  follow_up_task_sid?: string;
  /** How requested changes (or conflict notes) reached the agent. */
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

export const DELIVERY_MODE_OPTIONS: ReadonlyArray<{ value: DeliveryMode; label: string }> = [
  { value: '', label: 'Auto (PR/MR if remote, else direct)' },
  { value: 'pr_merge', label: 'PR/MR + merge on approve' },
  { value: 'pr_open', label: 'Approve opens PR/MR' },
  { value: 'direct', label: 'Direct merge' },
  { value: 'keep_branch', label: 'Keep branch only' },
];

export const MERGE_STYLE_OPTIONS: ReadonlyArray<{ value: MergeStyle; label: string }> = [
  { value: 'squash', label: 'Squash (default)' },
  { value: 'merge', label: 'Merge commit' },
  { value: 'rebase', label: 'Rebase' },
];

/** "task:<id>" / "chat:<id>" */
export function reviewKey(r: Pick<Review, 'task_id' | 'chat_id'>): string {
  return r.task_id ? `task:${r.task_id}` : `chat:${r.chat_id ?? ''}`;
}

/** Human-readable delivery outcome shown after a decision or PR preparation. */
export function deliveryOutcomeLine(r: Review | undefined): string {
  if (!r) return '';
  if (r.delivery_message) return r.delivery_message;
  switch (r.delivery_status) {
    case 'delivered':
      return r.pr_number ? `PR #${r.pr_number} merged.` : 'Delivered.';
    case 'pr_open':
      return r.pr_number ? `Delivered: PR #${r.pr_number} open.` : 'Delivered: PR open.';
    case 'auto_merge':
      return r.pr_number ? `Auto-merge enabled on PR #${r.pr_number} when checks pass.` : 'Auto-merge enabled when checks pass.';
    case 'manual':
      return 'Branch pushed; open a PR/MR by hand (no forge credential).';
    case 'conflict':
      return 'Merge conflict; sent back to the agent to resolve.';
    case 'closed':
      return r.pr_number ? `PR #${r.pr_number} closed without merging.` : 'PR closed without merging.';
    case 'offline':
      return 'Owning machine is offline.';
    case 'failed':
      return 'Delivery failed.';
  }
  return '';
}

/** Compact status summary for a tracked PR/MR (state, checks, auto-merge). */
export function prStatusLine(r: Pick<Review, 'pr_number' | 'pr_state' | 'pr_checks' | 'pr_auto_merge'> | undefined): string {
  if (!r?.pr_number) return '';
  const parts: string[] = [`PR #${r.pr_number}`];
  if (r.pr_state) parts.push(r.pr_state);
  if (r.pr_checks && r.pr_checks !== 'none') parts.push(`checks: ${r.pr_checks}`);
  if (r.pr_auto_merge && r.pr_state !== 'merged') parts.push('auto-merge');
  return parts.join(' · ');
}

/** Plain-English description of what Approve will do (server plan or local fallback). */
export function approveActionLabel(
  plan: DeliveryPlan | null,
  fallback?: { mode?: DeliveryMode; mergeStyle?: MergeStyle; hasRemote?: boolean; defaultBranch?: string; branch?: string; review?: Review },
): string {
  if (plan?.approve_label) return plan.approve_label;
  if (!fallback) return '';
  const hasRemote = !!fallback.hasRemote;
  const mode: DeliveryMode = fallback.mode || (hasRemote ? 'pr_merge' : 'direct');
  const style: MergeStyle = fallback.mergeStyle || 'squash';
  const base = fallback.defaultBranch || 'main';
  const verb = style === 'merge' ? 'merge' : style === 'rebase' ? 'rebase-merge' : 'squash-merge';
  switch (mode) {
    case 'direct':
      return hasRemote ? `Approve → ${verb} into ${base} and push` : `Approve → ${verb} into ${base} (local only)`;
    case 'pr_merge':
      if (fallback.review?.pr_number) return `Approve → merge PR #${fallback.review.pr_number} (${style}) when checks pass`;
      return `Approve → push branch, open PR and merge (${style}) when checks pass`;
    case 'pr_open':
      return `Approve → push branch and open a PR against ${base}`;
    case 'keep_branch':
      return fallback.branch ? `Approve → mark accepted and keep branch ${fallback.branch}` : 'Approve → mark accepted and keep branch';
  }
  return 'Approve';
}

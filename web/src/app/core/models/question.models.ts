/**
 * Questions (PRD §5.8b): every question an agent asks — ask_user and the agents' own
 * question and permission prompts — as hub objects in one fleet-wide queue. Mirrors
 * internal/store/questions.go and controller.QuestionView.
 */

export type QuestionStatus = 'open' | 'answered' | 'delegated' | 'expired' | 'cancelled';

export interface QuestionOption {
  id: string;
  label: string;
  consequence?: string;
  recommended?: boolean;
  /** Permission prompts: allow_once, allow_always, reject_once, ... */
  kind?: string;
}

export interface QuestionRef {
  kind: 'ticket' | 'pr' | 'file' | 'task' | 'chat' | 'machine' | 'question' | (string & {});
  ref: string;
  title?: string;
  status?: string;
  excerpt?: string;
  url?: string;
  resolved: boolean;
}

export interface QuestionCause {
  actor: string;
  source?: string;
  actor_id?: string;
  confidence?: string;
}

export interface QuestionAnswer {
  option_id?: string;
  label?: string;
  text?: string;
  summary?: string;
  by: QuestionCause;
  at: string;
  delivery?: 'tool' | 'worker' | 'note' | (string & {});
  delivered_at?: string;
  correction?: boolean;
  preparing?: boolean;
}

export interface QuestionDelegation {
  state: 'pending' | 'decided' | 'delivered' | 'overridden' | 'failed' | (string & {});
  requested_at: string;
  requested_by: QuestionCause;
  auto?: boolean;
  chat_id?: string;
  sent_at?: string;
  option_id?: string;
  label?: string;
  text?: string;
  rationale?: string;
  decided_at?: string;
  deliver_at?: string;
  note?: string;
}

export interface QuestionDraft {
  option_id?: string;
  label?: string;
  text?: string;
}

export interface QuestionThread {
  chat_id: string;
  answerer: 'fork' | 'commander' | (string & {});
  agent?: string;
  account_id?: string;
  worker_id?: string;
  status: 'working' | 'idle' | 'failed' | (string & {});
  note?: string;
  draft?: QuestionDraft;
  started_at: string;
  discarded?: boolean;
}

export interface QuestionMessage {
  question_id: string;
  seq: number;
  role: 'user' | 'answerer' | 'system' | (string & {});
  text: string;
  created_at: string;
}

export interface Question {
  id: string;
  sid: string;
  chat_id: string;
  task_id?: string;
  worker_id?: string;
  agent?: string;
  account_id?: string;
  kind: 'question' | 'permission' | (string & {});
  source: 'ask_user' | 'elicitation' | (string & {});
  elicitation_id?: string;
  prompt: string;
  context?: string;
  why_now?: string;
  options?: QuestionOption[];
  allow_text: boolean;
  recommendation?: string;
  rationale?: string;
  category?: string;
  blocks?: string[];
  group_key?: string;
  refs?: QuestionRef[];
  status: QuestionStatus;
  blocked_since: string;
  snoozed_until?: string;
  abandoned?: boolean;
  ask_count: number;
  rejections?: number;
  answer?: QuestionAnswer;
  delegation?: QuestionDelegation;
  thread?: QuestionThread;
  created_at: string;
  updated_at: string;
  closed_at?: string;
}

/** A question with what the UI shows around it (GET /api/questions). */
export interface QuestionView extends Question {
  chat_sid?: string;
  chat_title?: string;
  machine_name?: string;
  account_label?: string;
  commander?: boolean;
  impact: number;
  group_label?: string;
  messages: QuestionMessage[];
  thread_account_label?: string;
  thread_machine_name?: string;
}

export interface QuestionAnswerRequest {
  option_id?: string;
  text?: string;
}

export interface AgentQuestionMetrics {
  agent: string;
  asked: number;
  clarified: number;
  rejected: number;
  delegated: number;
  unclear: number;
  unclear_rate: number;
}

export interface QuestionMetrics {
  days: number;
  asked: number;
  answered: number;
  answered_with_clarification: number;
  answered_without_clarification: number;
  delegated: number;
  delegation_overrides: number;
  rejected: number;
  open: number;
  median_blocked_seconds: number;
  p90_blocked_seconds: number;
  total_blocked_seconds: number;
  oldest_open_blocked_seconds: number;
  agents: AgentQuestionMetrics[];
}

export interface QuestionSettings {
  auto_delegate: string[];
  delegation_grace_seconds: number;
}

/** Question categories users can let the commander decide (settings). */
export const DELEGATABLE_CATEGORIES: ReadonlyArray<{ id: string; label: string; hint: string }> = [
  { id: 'permission', label: 'Tool approvals', hint: "The agents' own permission prompts (run a command, edit a file)." },
  { id: 'routine', label: 'Routine decisions', hint: 'Questions agents tag as routine (naming, small refactors).' },
];

export function isPending(q: Pick<Question, 'status'>): boolean {
  return q.status === 'open' || q.status === 'delegated';
}

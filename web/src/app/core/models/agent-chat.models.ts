/**
 * Agent chat on subscriptions: TypeScript mirror of internal/protocol/agentchat.go.
 * Field names are the Go JSON tags; times are RFC 3339 strings; `omitempty` fields are
 * optional. Keep this file in lockstep with the Go protocol.
 */

/** Event kinds (AgentChatEvent.kind). */
export const AGENT_EVENT = {
  sessionReady: 'session_ready',
  status: 'status',
  messageDelta: 'message_delta',
  message: 'message',
  thoughtDelta: 'thought_delta',
  toolCall: 'tool_call',
  plan: 'plan',
  elicitation: 'elicitation',
  elicitationResolved: 'elicitation_resolved',
  usage: 'usage',
  rateLimit: 'rate_limit',
  turnEnd: 'turn_end',
  error: 'error',
} as const;
export type AgentEventKind = (typeof AGENT_EVENT)[keyof typeof AGENT_EVENT] | (string & {});

/** Chat statuses (ChatStatus* in Go): the chat window's status pill. */
export type ChatStatus =
  | 'starting'
  | 'idle'
  | 'working'
  | 'needs_you'
  | 'done'
  | 'failed'
  | 'cancelled';

export interface AgentChatEvent {
  chat_id: string;
  turn_id?: string;
  seq: number;
  kind: AgentEventKind;
  at: string;

  status?: ChatStatus | string;
  agent_session_id?: string;
  message_id?: string;
  text?: string;

  tool_call?: AgentToolCall;
  plan?: AgentPlanItem[];
  elicitation?: AgentElicitation;
  usage?: AgentTokenUsage;
  rate_limit?: AgentRateLimit;
  error?: AgentChatError;

  /** On turn_end: end_turn, max_tokens, refusal, cancelled, error. */
  stop_reason?: string;

  model?: string;
  effort?: string;
  approval_mode?: string;
}

export type ToolKind = 'read' | 'edit' | 'execute' | 'search' | 'fetch' | 'think' | 'other' | (string & {});
export type ToolStatus = 'pending' | 'running' | 'completed' | 'failed' | (string & {});

export interface AgentToolCall {
  id: string;
  name: string;
  kind?: ToolKind;
  title?: string;
  status?: ToolStatus;
  input?: string;
  output?: string;
  paths?: string[];
}

export interface AgentPlanItem {
  text: string;
  status?: 'pending' | 'in_progress' | 'completed' | (string & {});
}

export type ElicitationKind = 'permission' | 'question' | 'auth';

export interface AgentElicitation {
  id: string;
  kind: ElicitationKind | (string & {});
  title: string;
  detail?: string;
  tool_call?: AgentToolCall;
  options?: AgentElicitationOption[];
  allow_text?: boolean;
}

export interface AgentElicitationOption {
  id: string;
  label: string;
  /** allow_once, allow_always, reject_once, reject_always, choice */
  kind?: string;
}

export interface AgentTokenUsage {
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_create_tokens?: number;
  cost_usd?: number;
}

export interface AgentRateLimit {
  /** five_hour, seven_day, seven_day_opus, monthly, daily, ... */
  window: string;
  used_percent?: number;
  resets_at?: string;
  /** ok, warning, exhausted */
  state?: 'ok' | 'warning' | 'exhausted' | (string & {});
}

export type AgentErrorKind =
  | 'auth'
  | 'usage_limit'
  | 'rate_limit'
  | 'exit'
  | 'not_installed'
  | 'model'
  | 'timeout'
  | 'protocol'
  | 'other';

export interface AgentChatError {
  kind: AgentErrorKind | (string & {});
  message: string;
  exit_code?: number;
  stderr?: string;
  resets_at?: string;
  window?: string;
  relogin?: AgentRelogin;
  retryable?: boolean;
}

export interface AgentRelogin {
  command: string;
  env?: Record<string, string>;
  url?: string;
  note?: string;
}

// --- Capabilities -------------------------------------------------------------------

export interface AgentCapabilitiesReport {
  accounts: AgentAccount[];
}

export type AuthState = 'ok' | 'logged_out' | 'expiring' | 'unknown';

export interface AgentAccount {
  id: string;
  agent: string;
  label: string;
  is_default: boolean;
  version?: string;

  auth: AgentAuthStatus;
  plan?: string;
  models?: AgentModel[];
  models_source?: string;
  default_model?: string;
  approval_modes?: AgentApprovalMode[];
  default_approval_mode?: string;
  usage?: AgentRateLimit[];
  usage_note?: string;
  checked_at: string;
  chat_supported: boolean;
}

export interface AgentAuthStatus {
  state: AuthState | (string & {});
  method?: string;
  email?: string;
  expires_at?: string;
  detail?: string;
  relogin?: AgentRelogin;
}

export interface AgentModel {
  id: string;
  display_name: string;
  description?: string;
  is_default?: boolean;
  efforts?: AgentEffort[];
  default_effort?: string;
  context_tokens?: number;
}

export interface AgentEffort {
  id: string;
  label: string;
  short?: string;
}

export interface AgentApprovalMode {
  id: string;
  label: string;
  description?: string;
  /** 0 read-only/plan, 1 asks, 2 accepts edits, 3 auto/approve-for-me, 4 full access. */
  risk: number;
  /** hand, edit, shield-check, shield-alert, eye */
  icon?: string;
}

/** An account as the hub reports it: the AgentAccount plus the worker that owns it. */
export interface WorkerAgentAccount extends AgentAccount {
  worker_id: string;
}

// --- Hub chat sessions (browser-facing) ---------------------------------------------

/** Fields the PWA sends when creating an agent chat. */
export interface AgentChatCreateRequest {
  title: string;
  worker_id: string;
  agent: string;
  account_id: string;
  model?: string;
  effort?: string;
  approval_mode?: string;
  cwd?: string;
  fleet_tools?: boolean;
}

/** PATCH /api/chats/{id}: rename or change session settings. */
export interface AgentChatPatch {
  title?: string;
  model?: string;
  effort?: string;
  approval_mode?: string;
  account_id?: string;
  cwd?: string;
}

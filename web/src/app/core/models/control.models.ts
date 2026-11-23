/**
 * Human control and attribution (PRD §5.8a): causes, fleet events, holds, open requests,
 * Pause everything. Mirrors internal/store/control.go.
 */

export type ActorKind = 'user' | 'orchestrator' | 'agent' | 'scheduler' | 'system' | 'unknown';
export type Confidence = 'certain' | 'high' | 'likely' | 'unknown';

/** Who caused a state change, through which path, and how sure Konsert is. */
export interface Cause {
  actor: ActorKind | string;
  /** ui, cli, terminal, ssh for users; mcp, worker, reaper, ... for others. */
  source?: string;
  /** The user, or the chat/task id of an agent. */
  actor_id?: string;
  confidence: Confidence | string;
  request_id?: string;
  command_id?: string;
  hold_id?: string;
}

export type FleetEventKind = 'status' | 'action' | 'hold' | 'request' | 'veto' | 'notice';

export interface FleetEvent {
  id: number;
  created_at: string;
  kind: FleetEventKind | string;
  action?: string;
  cause: Cause;
  worker_id?: string;
  task_id?: string;
  chat_id?: string;
  from_status?: string;
  to_status?: string;
  summary: string;
  meta_json?: string;
}

export interface FleetEventsResponse {
  events: FleetEvent[];
  next_cursor?: number;
}

export interface FleetEventQuery {
  actor?: string;
  worker_id?: string;
  task_id?: string;
  chat_id?: string;
  notable?: boolean;
  before_id?: number;
  after_id?: number;
  limit?: number;
}

export interface ControlResource {
  kind: string;
  name?: string;
  port?: number;
}

export type HoldScope = 'session' | 'machine' | 'resource';

export interface Hold {
  id: string;
  sid: string;
  scope_kind: HoldScope | string;
  worker_id?: string;
  task_id?: string;
  chat_id?: string;
  resource?: ControlResource;
  summary: string;
  cause: Cause;
  created_at: string;
  released_at?: string;
  released_by?: string;
  release_note?: string;
}

export type UserRequestStatus = 'open' | 'done' | 'declined' | 'canceled';

export interface UserRequest {
  id: string;
  sid: string;
  chat_id?: string;
  task_id?: string;
  worker_id: string;
  resource?: ControlResource;
  description: string;
  status: UserRequestStatus | string;
  created_at: string;
  resolved_at?: string;
  resolved_by?: string;
  resolution?: string;
  command_id?: string;
}

export interface FleetPause {
  paused: boolean;
  paused_at?: string;
  paused_by?: string;
}

export interface FleetPauseResult extends FleetPause {
  affected?: number;
}

export type VetoReason = 'wrong_approach' | 'wrong_machine' | 'not_now' | 'dangerous' | 'other';

export const VETO_REASONS: ReadonlyArray<{ id: VetoReason; label: string }> = [
  { id: 'wrong_approach', label: 'Wrong approach' },
  { id: 'wrong_machine', label: 'Wrong machine' },
  { id: 'not_now', label: 'Not now' },
  { id: 'dangerous', label: 'Dangerous' },
  { id: 'other', label: 'Other' },
];

export interface ChatActionResult {
  ok: boolean;
  chat_id: string;
  status: string;
}

export interface TaskActionResult {
  ok: boolean;
  task_id: string;
  task_sid: string;
  status: string;
  hold_id?: string;
}

export interface WorkerControlResult {
  ok: boolean;
  worker_id: string;
  drained?: boolean;
  affected?: number;
}

const SOURCE_LABELS: Record<string, string> = {
  ui: 'the Konsert UI',
  cli: 'the CLI',
  terminal: 'a Konsert terminal',
  ssh: 'SSH',
  mcp: 'MCP',
};

/** Who did it, from the viewer's perspective ("You", "The orchestrator", ...). */
export function actorLabel(c: Cause | undefined | null): string {
  switch (c?.actor) {
    case 'user':
      return 'You';
    case 'orchestrator':
      return 'The orchestrator';
    case 'agent':
      return 'An agent';
    case 'scheduler':
      return 'The scheduler';
    case 'system':
      return 'Konsert';
    default:
      return 'Unknown';
  }
}

/** "via the Konsert UI" etc.; empty when the source is unknown. */
export function sourceLabel(c: Cause | undefined | null): string {
  const s = c?.source;
  if (!s) return '';
  return `via ${SOURCE_LABELS[s] ?? s}`;
}

/** Past-tense verb for a status a cause led to. */
function statusVerb(status: string): string {
  switch (status) {
    case 'canceled':
    case 'cancelled':
      return 'Canceled';
    case 'suspended':
      return 'Suspended';
    case 'failed':
      return 'Failed';
    case 'completed':
    case 'done':
      return 'Finished';
    case 'queued':
      return 'Queued';
    case 'running':
    case 'working':
      return 'Started';
    default:
      return status
        ? status.charAt(0).toUpperCase() + status.slice(1).replace(/_/g, ' ')
        : 'Changed';
  }
}

/** One line such as "Canceled by you via the Konsert UI" (empty without a cause). */
export function causeLine(status: string, c: Cause | undefined | null): string {
  if (!c?.actor) return '';
  const who =
    c.actor === 'user' ? 'you' : actorLabel(c).replace(/^The /, 'the ').replace(/^An /, 'an ');
  const via = sourceLabel(c);
  const unsure = c.confidence && c.confidence !== 'certain' ? ` (${c.confidence})` : '';
  return `${statusVerb(status)} by ${who}${via ? ' ' + via : ''}${unsure}`;
}

/** Short description of a hold/request resource ("service nginx", "port 80"). */
export function resourceLabel(r: ControlResource | undefined | null): string {
  if (!r) return '';
  if (r.port) return `${r.kind || 'port'} ${r.port}`;
  return [r.kind, r.name].filter(Boolean).join(' ');
}

/** Merges events by id, newest (highest id) first. */
export function mergeFleetEvents(
  existing: FleetEvent[],
  incoming: FleetEvent[],
  cap = 500,
): FleetEvent[] {
  const byId = new Map<number, FleetEvent>();
  for (const ev of existing) byId.set(ev.id, ev);
  for (const ev of incoming) byId.set(ev.id, ev);
  return [...byId.values()].sort((a, b) => b.id - a.id).slice(0, cap);
}

/**
 * Whether an event belongs in the "Your actions" timeline: anything the user caused, plus
 * how agents and Konsert responded (holds, requests, vetoes, notices). Routine status
 * transitions of agents are left out.
 */
export function isTimelineEvent(ev: FleetEvent): boolean {
  if (ev.cause?.actor === 'user') return true;
  return ev.kind !== 'status';
}

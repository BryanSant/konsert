/**
 * Live-work data: session activity (KON-NAYVC), machine heat (KON-RJ8P7) and project
 * narration (KON-H5BXK). Wire shapes mirror internal/protocol/activity.go and
 * internal/controller/heat.go; see docs/design/activity.md.
 */

/** What a session is doing right now. */
export type ActivityState =
  | 'thinking'
  | 'working'
  | 'speaking'
  | 'asking'
  | 'blocked'
  | 'queued'
  | 'done'
  | 'error';

/** Where an activity came from, from the most to the least precise. */
export type ActivitySource = 'stream' | 'session_log' | 'hook' | 'passive' | 'hub' | 'status';

/** One session's live activity (fleet.update kind "activity"; GET /api/fleet `activity`). */
export interface SessionActivity {
  chat_id?: string;
  task_id?: string;
  worker_id?: string;
  state: ActivityState;
  /** The running tool (working). */
  tool?: string;
  /** A short human line: the tool call's title, the question asked, the error. */
  label?: string;
  /** Output rate while speaking (estimated, ~4 characters per token). */
  tokens_per_sec?: number;
  /** Best-effort state from a weak signal (session log, hooks, process inspection). */
  coarse?: boolean;
  source?: ActivitySource | string;
  /** When the current state began. */
  since: string;
  /** When this was derived. */
  at: string;
}

export type HeatLevel = 'cool' | 'warm' | 'hot';
export type HeatCause = 'cpu' | 'mem' | 'gpu' | 'slots';

/** A machine's smoothed 0..1 load score (Worker.heat). */
export interface WorkerHeat {
  /** EWMA-smoothed score (~30 s time constant). */
  score: number;
  /** Latest unsmoothed score. */
  raw: number;
  level: HeatLevel;
  /** The component dominating the raw score. */
  cause: HeatCause;
  components?: Partial<Record<HeatCause, number>>;
  /** The scheduler considers the machine full (active tasks >= max_concurrent). */
  full?: boolean;
  updated_at: string;
}

/** Heat thresholds (documented in docs/design/activity.md). */
export const HEAT_WARM_AT = 0.4;
export const HEAT_HOT_AT = 0.75;

/** Which bucket a session counts in for a project's counts. */
export type WorkBucket = 'running' | 'queued' | 'needs_you' | 'blocked' | 'done';

export interface WorkCounts {
  running: number;
  queued: number;
  needs_you: number;
  blocked: number;
  done: number;
  /** Of done: the ones that failed. */
  failed: number;
}

/** Where a narration line came from. */
export type NarrationSource = 'orchestrator' | 'activity' | 'title' | 'ticket' | 'prompt' | 'agent' | 'counts' | 'project';

/** One task or chat in the live-work views. */
export interface WorkItem {
  kind: 'task' | 'chat';
  id: string;
  sid: string;
  workerId: string;
  agent: string;
  /** Short name: task title, ticket title, the prompt's first line or the chat title. */
  title: string;
  /** The one-line "now": the orchestrator's narration, else the live tool label, else facts. */
  now: string;
  nowSource: NarrationSource;
  ticketId?: string;
  ticketTitle?: string;
  activity: SessionActivity;
  bucket: WorkBucket;
  /** A session started outside Konsert (observed). */
  observed: boolean;
  createdAt: string;
}

/** A project (or "Other work") with its sessions and counts. */
export interface ProjectGroup {
  /** Project id, or OTHER_WORK_KEY. */
  key: string;
  projectId: string | null;
  name: string;
  /** The orchestrator's headline, else a line made from facts; never blank. */
  headline: string;
  headlineSource: NarrationSource;
  counts: WorkCounts;
  items: WorkItem[];
}

export const OTHER_WORK_KEY = 'other';
export const OTHER_WORK_NAME = 'Other work';

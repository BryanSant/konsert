/**
 * Live-work data: session activity (SYR-NAYVC), machine heat (SYR-RJ8P7) and project
 * narration (SYR-H5BXK). Wire shapes mirror internal/protocol/activity.go and
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
  /**
   * A short human line: the tool call's raw title ("Run: cd /x && go test ./..."; calls
   * running at once joined with " · "), the question asked, the error.
   */
  label?: string;
  /**
   * The readable now line of a working session ("running tests", "editing wc.py"), made
   * by the hub (internal/nowline) with paths shortened against the checkout. Shown
   * instead of the label, which stays available as the line's hover title.
   */
  summary?: string;
  /** Output rate while speaking (estimated, ~4 characters per token). */
  tokens_per_sec?: number;
  /** Best-effort state from a weak signal (session log, hooks, process inspection). */
  coarse?: boolean;
  source?: ActivitySource | string;
  /** The parent only waits on its sub-agents (every running tool call starts one). */
  waiting?: boolean;
  /**
   * Shell processes under the session's agent (from its process tree, background ones
   * included; they keep counting after the session ended until they exit), how many run
   * a command, and up to 12 of them in detail.
   */
  shells?: number;
  shells_busy?: number;
  shell_list?: ShellInfo[];
  /** Running sub-agents (claude Task calls), each with its own state. */
  subagents?: SubagentActivity[];
  /** When the current state began. */
  since: string;
  /** When this was derived. */
  at: string;
}

/** One shell process of a session (SessionActivity.shell_list). */
export interface ShellInfo {
  pid: number;
  /** The command line, cleaned of agent wrappers. */
  command: string;
  /** Its readable line ("running tests"), from the hub. */
  summary?: string;
  started_at: string;
  /** Runs a command (a child process, or CPU use). */
  busy?: boolean;
  /** When busy last changed. */
  since: string;
  /** No longer under the agent (the session ended) but still running. */
  dangling?: boolean;
}

/** What Syre does about shells a finished session left running (SYR-DCEZV). */
export type ShellPolicyMode = 'remind' | 'ask' | 'stop';

export interface ShellPolicySettings {
  mode: ShellPolicyMode;
  idle_minutes: number;
  /** Project id → mode; missing or '' = the global mode. */
  projects: Record<string, ShellPolicyMode | ''>;
}

/** The Needs-you item for a session that left shells running. */
export interface ShellReminder {
  /** "task:<id>" or "chat:<id>". */
  id: string;
  task_id?: string;
  chat_id?: string;
  worker_id?: string;
  /** "2 shells left running by Fix login". */
  title: string;
  session_title: string;
  shells: number;
  shell_list?: ShellInfo[];
  idle_since: string;
  /** The agent can still be asked to clean up. */
  can_ask: boolean;
  mode: ShellPolicyMode;
  /** Why Syre reminds instead of acting. */
  note?: string;
  created_at: string;
  /** In a fleet.update: the item is gone. */
  resolved?: boolean;
}

/** The states a sub-agent reports. */
export type SubagentState = 'thinking' | 'working' | 'speaking' | 'asking';

/** One running sub-agent (SessionActivity.subagents). */
export interface SubagentActivity {
  /** The parent's tool call id that started it. */
  id: string;
  title?: string;
  state: SubagentState;
  /** The tool call it runs, or its question. */
  label?: string;
  since: string;
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

/** One downsampled heat bucket (GET /api/fleet/heat); null = no data (offline, no history). */
export interface HeatSample {
  /** Mean smoothed score over the bucket. */
  score: number;
  /** Highest smoothed score in the bucket. */
  max: number;
  /** The cause at the highest score. */
  cause: HeatCause | string;
}

/**
 * Heat history per machine over a range (GET /api/fleet/heat?range=12h&points=48). 12h reads
 * raw samples; 24h, 3d and 1w read hourly roll-ups, so they have at most one point per hour
 * (24h gives 24 points): use the length of `samples`, not the points asked for.
 */
export interface HeatHistory {
  range: string;
  start: string;
  end: string;
  step_seconds: number;
  /** The history tier the points come from. */
  tier?: 'raw' | 'hourly';
  /** How long machine history is kept, in plain words. */
  retention?: string;
  workers: Array<{ worker_id: string; samples: Array<HeatSample | null> }>;
}

/** Shown where machine history appears (mirrors VitalsRetentionText on the hub). */
export const MACHINE_HISTORY_RETENTION =
  'Machine history keeps every sample for 12 hours, hourly summaries for 30 days and daily summaries for a year.';

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
export type NarrationSource = 'commander' | 'activity' | 'title' | 'ticket' | 'prompt' | 'agent' | 'counts' | 'project';

/** One task or chat in the live-work views. */
export interface WorkItem {
  kind: 'task' | 'chat';
  id: string;
  sid: string;
  workerId: string;
  agent: string;
  /** Short name: task title, ticket title, the prompt's first line or the chat title. */
  title: string;
  /** The one-line "now": the commander's narration, else the live tool label, else facts. */
  now: string;
  nowSource: NarrationSource;
  /** The full tool line behind a summarised now (hover title); absent when not working. */
  nowTitle?: string;
  ticketId?: string;
  ticketTitle?: string;
  activity: SessionActivity;
  bucket: WorkBucket;
  /** A session started outside Syre (observed). */
  observed: boolean;
  createdAt: string;
}

/** A project (or "Other work") with its sessions and counts. */
export interface ProjectGroup {
  /** Project id, or OTHER_WORK_KEY. */
  key: string;
  projectId: string | null;
  name: string;
  /** The commander's headline, else a line made from facts; never blank. */
  headline: string;
  headlineSource: NarrationSource;
  counts: WorkCounts;
  items: WorkItem[];
}

export const OTHER_WORK_KEY = 'other';
export const OTHER_WORK_NAME = 'Other work';

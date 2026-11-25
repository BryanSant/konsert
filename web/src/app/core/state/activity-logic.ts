import {
  ActivityState,
  HEAT_HOT_AT,
  HEAT_WARM_AT,
  HeatLevel,
  NarrationSource,
  OTHER_WORK_KEY,
  OTHER_WORK_NAME,
  ProjectGroup,
  SessionActivity,
  WorkBucket,
  WorkCounts,
  WorkItem,
} from '../models/activity.models';
import { ChatSession, Project, Repo, Task } from '../models/syre.models';

/** Pure live-work logic (activity fallbacks, buckets, narration, grouping); see work.store.ts. */

const TERMINAL_TASK = new Set(['completed', 'failed', 'canceled']);

export function activityKey(a: Pick<SessionActivity, 'chat_id' | 'task_id'>): string {
  return a.chat_id ? `chat:${a.chat_id}` : `task:${a.task_id ?? ''}`;
}

/** Newer wins: true when `next` should replace `prev`. */
export function isNewerActivity(prev: SessionActivity | undefined, next: SessionActivity): boolean {
  if (!prev) return true;
  return Date.parse(next.at || '') >= Date.parse(prev.at || '');
}

/** A coarse activity from a task's status, for tasks with no reported activity. */
export function activityFromTaskStatus(t: Task): SessionActivity {
  let state: ActivityState;
  let label: string | undefined;
  switch (t.status) {
    case 'queued':
      state = 'queued';
      label = 'Waiting for a machine';
      break;
    case 'assigned':
      state = 'queued';
      label = 'Starting';
      break;
    case 'running':
      state = 'thinking';
      break;
    case 'waiting_input':
      state = 'asking';
      label = 'Waiting for input';
      break;
    case 'suspended':
      state = 'blocked';
      label = t.suspended_by === 'pause' ? 'Paused (Pause everything)' : 'Suspended';
      break;
    case 'failed':
      state = 'error';
      label = firstLine(t.summary) || 'Failed';
      break;
    case 'canceled':
      state = 'done';
      label = 'Cancelled';
      break;
    default: // completed, idle
      state = 'done';
  }
  const at = t.completed_at || t.started_at || t.created_at || '';
  return { task_id: t.id, worker_id: t.worker_id, state, label, coarse: true, source: 'status', since: at, at };
}

/** A coarse activity from a chat's status pill, for chats with no reported activity. */
export function activityFromChatStatus(c: ChatSession): SessionActivity {
  let state: ActivityState;
  let label: string | undefined;
  switch (c.status) {
    case 'starting':
      state = 'queued';
      label = 'Starting the agent';
      break;
    case 'working':
      state = 'thinking';
      break;
    case 'needs_you':
      state = 'asking';
      label = 'Waiting for you';
      break;
    case 'suspended':
      state = 'blocked';
      label = 'Suspended';
      break;
    case 'failed':
      state = 'error';
      label = 'Failed';
      break;
    case 'cancelled':
      state = 'done';
      label = 'Cancelled';
      break;
    default: // idle, done, ''
      state = 'done';
  }
  const at = c.updated_at || c.created_at || '';
  return { chat_id: c.id, worker_id: c.worker_id, state, label, coarse: true, source: 'status', since: at, at };
}

/** The bucket a session counts in. A terminal task status wins over a stale activity. */
export function bucketOf(a: SessionActivity, taskStatus?: string): WorkBucket {
  if (taskStatus && TERMINAL_TASK.has(taskStatus)) return 'done';
  switch (a.state) {
    case 'asking':
      return 'needs_you';
    case 'blocked':
      return 'blocked';
    case 'queued':
      return 'queued';
    case 'done':
    case 'error':
      return 'done';
    default:
      return 'running';
  }
}

export function emptyCounts(): WorkCounts {
  return { running: 0, queued: 0, needs_you: 0, blocked: 0, done: 0, failed: 0 };
}

export function heatLevelOf(score: number): HeatLevel {
  if (score > HEAT_HOT_AT) return 'hot';
  if (score >= HEAT_WARM_AT) return 'warm';
  return 'cool';
}

export function firstLine(s: string | undefined | null, max = 120): string {
  const line = (s ?? '')
    .split('\n')
    .map((l) => l.trim())
    .find((l) => l.length > 0);
  if (!line) return '';
  const one = line.replace(/\s+/g, ' ');
  return one.length > max ? one.slice(0, max - 1) + '…' : one;
}

/** A task's short name: its title, else the ticket title, else the prompt's first line. */
export function taskTitle(t: Task): { text: string; source: NarrationSource } {
  if (t.title?.trim()) return { text: firstLine(t.title), source: 'title' };
  if (t.ticket_title?.trim()) {
    return { text: t.ticket_id ? `${t.ticket_id} ${firstLine(t.ticket_title)}` : firstLine(t.ticket_title), source: 'ticket' };
  }
  const p = firstLine(t.prompt);
  if (p) return { text: p, source: 'prompt' };
  return { text: `${t.agent || 'agent'} ${t.sid || ''}`.trim(), source: 'agent' };
}

/**
 * A task's one-line "now": the commander's narration, else the live tool label (or
 * question / error), else its title. Never blank.
 */
export function taskNow(t: Task, a: SessionActivity): { text: string; source: NarrationSource } {
  if (t.narration?.trim()) return { text: firstLine(t.narration), source: 'commander' };
  const live = liveLine(a);
  if (live) return { text: live, source: 'activity' };
  return taskTitle(t);
}

/**
 * The full tool line of a working session (the raw label, e.g. "Run: cd /x && go test
 * ./..."), for the now line's hover title; undefined otherwise.
 */
export function nowTitle(a: SessionActivity | undefined): string | undefined {
  if (!a || a.state !== 'working') return undefined;
  const label = (a.label ?? '').trim();
  return label || undefined;
}

/** A chat's one-line "now": the live line, else its title, else the agent. */
export function chatNow(c: ChatSession, a: SessionActivity): { text: string; source: NarrationSource } {
  const live = liveLine(a);
  if (live) return { text: live, source: 'activity' };
  if (c.title?.trim()) return { text: firstLine(c.title), source: 'title' };
  return { text: `${c.agent || 'agent'} chat`, source: 'agent' };
}

/**
 * The activity's own line, when it says something specific: the hub's summary of the
 * running tool ("running tests"), else its raw label; the question; the error.
 */
function liveLine(a: SessionActivity): string {
  const label = firstLine(a.label);
  switch (a.state) {
    case 'working':
      return firstLine(a.summary) || label || (a.tool ? firstLine(a.tool) : '');
    case 'asking':
    case 'error':
    case 'blocked':
      return label;
    default:
      return '';
  }
}

/** Repos matched by longest path prefix on the same worker (for chats and observed sessions). */
export function projectForPath(repos: Repo[], workerId: string | undefined, path: string | undefined): string | null {
  const p = trimSlash(path);
  if (!p || !workerId) return null;
  let best: Repo | null = null;
  for (const r of repos) {
    if (r.worker_id !== workerId) continue;
    const rp = trimSlash(r.path);
    if (!rp) continue;
    if ((p === rp || p.startsWith(rp + '/') || p.startsWith(rp + '\\')) && (!best || rp.length > trimSlash(best.path).length)) {
      best = r;
    }
  }
  return best ? best.project_id : null;
}

function trimSlash(p: string | undefined): string {
  return (p ?? '').trim().replace(/[\\/]+$/, '');
}

export interface GroupInputs {
  projects: Project[];
  repos: Repo[];
  tasks: Task[];
  chats: ChatSession[];
  activityOf: (key: string) => SessionActivity | undefined;
}

/**
 * Groups tasks and chats by project with counts and narration. Sessions with no project
 * (or the default project) go under "Other work"; chats and observed sessions are placed by
 * their cwd when it maps to a project's repo. Scratch terminals, shells, the commander
 * chat itself and API-backend chats are not work items.
 */
export function groupWork(inp: GroupInputs): ProjectGroup[] {
  const real = new Map(inp.projects.filter((p) => !p.is_default).map((p) => [p.id, p]));
  const items = new Map<string, WorkItem[]>();
  const add = (key: string, it: WorkItem) => {
    const list = items.get(key);
    if (list) list.push(it);
    else items.set(key, [it]);
  };
  const place = (projectId: string | null | undefined): string =>
    projectId && real.has(projectId) ? projectId : OTHER_WORK_KEY;

  for (const t of inp.tasks) {
    if (t.origin === 'scratch' || t.agent === 'shell') continue;
    const observed = t.origin === 'external';
    let pid: string | null | undefined = t.project_id;
    if (!pid || !real.has(pid)) {
      // Observed sessions (and tasks dispatched to a path) map by their directory.
      pid = projectForPath(inp.repos, t.worker_id, t.effective_dir || t.workdir) ?? pid;
    }
    const a = inp.activityOf(`task:${t.id}`) ?? activityFromTaskStatus(t);
    const title = taskTitle(t);
    const now = taskNow(t, a);
    add(place(pid), {
      kind: 'task',
      id: t.id,
      sid: t.sid,
      workerId: t.worker_id,
      agent: t.agent,
      title: title.text,
      now: now.text,
      nowSource: now.source,
      nowTitle: nowTitle(a),
      ticketId: t.ticket_id || undefined,
      ticketTitle: t.ticket_title || undefined,
      activity: a,
      bucket: bucketOf(a, t.status),
      observed,
      createdAt: t.created_at,
    });
  }
  for (const c of inp.chats) {
    if (c.fleet_tools || (c.backend && c.backend !== 'agent')) continue;
    const a = inp.activityOf(`chat:${c.id}`) ?? activityFromChatStatus(c);
    const now = chatNow(c, a);
    add(place(projectForPath(inp.repos, c.worker_id, c.cwd)), {
      kind: 'chat',
      id: c.id,
      sid: c.sid,
      workerId: c.worker_id ?? '',
      agent: c.agent ?? '',
      title: firstLine(c.title) || `${c.agent || 'agent'} chat`,
      now: now.text,
      nowSource: now.source,
      nowTitle: nowTitle(a),
      activity: a,
      bucket: bucketOf(a),
      observed: false,
      createdAt: c.created_at,
    });
  }

  const groups: ProjectGroup[] = [];
  for (const [key, list] of items) {
    list.sort(byAttention);
    const counts = emptyCounts();
    for (const it of list) {
      counts[it.bucket]++;
      if (it.activity.state === 'error') counts.failed++;
    }
    const project = key === OTHER_WORK_KEY ? null : (real.get(key) ?? null);
    const name = project?.name ?? OTHER_WORK_NAME;
    const head = headlineFor(project, list, counts);
    groups.push({ key, projectId: project?.id ?? null, name, headline: head.text, headlineSource: head.source, counts, items: list });
  }
  // Projects needing the user first, then the busiest; "Other work" last.
  groups.sort((a, b) => {
    if ((a.key === OTHER_WORK_KEY) !== (b.key === OTHER_WORK_KEY)) return a.key === OTHER_WORK_KEY ? 1 : -1;
    if (a.counts.needs_you !== b.counts.needs_you) return b.counts.needs_you - a.counts.needs_you;
    if (a.counts.running !== b.counts.running) return b.counts.running - a.counts.running;
    return a.name.localeCompare(b.name);
  });
  return groups;
}

const BUCKET_RANK: Record<WorkBucket, number> = { needs_you: 0, running: 1, blocked: 2, queued: 3, done: 4 };

function byAttention(a: WorkItem, b: WorkItem): number {
  const r = BUCKET_RANK[a.bucket] - BUCKET_RANK[b.bucket];
  if (r !== 0) return r;
  return Date.parse(b.createdAt || '') - Date.parse(a.createdAt || '') || a.id.localeCompare(b.id);
}

/** The project's headline: the commander's, else the most pressing item, else counts. */
export function headlineFor(project: Project | null, items: WorkItem[], counts: WorkCounts): { text: string; source: NarrationSource } {
  if (project?.headline?.trim()) return { text: firstLine(project.headline, 140), source: 'commander' };
  const lead = items.find((i) => i.bucket === 'needs_you') ?? items.find((i) => i.bucket === 'running');
  if (lead) {
    const src: NarrationSource = lead.ticketTitle ? 'ticket' : 'title';
    const what = lead.ticketTitle ? `${lead.ticketId ? lead.ticketId + ' ' : ''}${lead.ticketTitle}` : lead.title;
    return { text: firstLine(lead.bucket === 'needs_you' ? `Needs you: ${what}` : `Working on ${what}`, 140), source: src };
  }
  const parts: string[] = [];
  if (counts.queued) parts.push(`${counts.queued} queued`);
  if (counts.blocked) parts.push(`${counts.blocked} blocked`);
  if (counts.done) parts.push(`${counts.done} done`);
  if (parts.length) return { text: parts.join(' · '), source: 'counts' };
  return { text: project ? project.name : 'Sessions not tied to a project', source: 'project' };
}

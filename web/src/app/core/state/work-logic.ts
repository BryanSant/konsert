import { Hold } from '../models/control.models';
import { ChatSession, DEFAULT_PROJECT_ID, GitSummary, Project, Repo, Task, Worker } from '../models/konsert.models';
import { QuestionView, isPending } from '../models/question.models';

/**
 * The mode UI's one view of "a piece of work": a task (agent CLI or shell in a PTY) or an
 * agent chat, with where it runs, what it is for and what state it is in. Pure functions
 * here; WorkStore wires them to the stores. The activity/heat/narration work replaces the
 * `now` and grouping inputs later without touching the views.
 */

/** Board column (Work mode); null = not on the board (old, idle). */
export type WorkColumn = 'queued' | 'running' | 'needs_you' | 'review' | 'done';

/** The control annunciator (ui-paradigms §4.9) plus lifecycle labels for finished work. */
export type ControlLabel = 'AUTO' | 'ORCH' | 'ASKING' | 'HELD' | 'SUSPENDED' | 'YOU' | 'STALE' | 'QUEUED' | 'DONE' | 'FAILED' | 'STOPPED';

export type WorkSessionKind = 'chat' | 'terminal' | 'agent';

export interface WorkItem {
  id: string;
  sid: string;
  kind: 'task' | 'chat';
  /** chat: agent chat transcript; agent: agent CLI in a PTY; terminal: a shell. */
  sessionKind: WorkSessionKind;
  title: string;
  agent: string;
  workerId: string;
  machine: string;
  projectKey: string;
  projectName: string;
  status: string;
  column: WorkColumn | null;
  control: ControlLabel;
  /** Running, queued, waiting on the user, held or suspended. */
  live: boolean;
  /** The machine it runs on stopped reporting while the work was live. */
  stale: boolean;
  /** Started outside Konsert (the user's own agent session, seen on the machine): read-only. */
  observed: boolean;
  origin: string;
  goal: string;
  now: string;
  questions: number;
  hold?: Hold;
  /** Finished with commits or a diff: something to review. */
  hasChanges: boolean;
  createdAt: string;
  updatedAt: string;
  finishedAt?: string;
  task?: Task;
  chat?: ChatSession;
}

export interface WorkInputs {
  tasks: readonly Task[];
  chats: readonly ChatSession[];
  workers: Record<string, Worker>;
  projects: readonly Project[];
  repos: readonly Repo[];
  questions: readonly QuestionView[];
  holds: readonly Hold[];
  /** Task ids the user has control of (Take Control). */
  interactive: (taskId: string) => boolean;
  /** Review decisions: id -> approved/rejected. */
  reviewed: Record<string, string>;
  /** Chat status as the transcript knows it (fresher than the session row). */
  chatStatus?: (chatId: string) => string | undefined;
  /** What a chat is doing right now (the running tool), if known. */
  chatNow?: (chatId: string) => string;
  now?: number;
}

const TASK_TERMINAL = new Set(['completed', 'failed', 'canceled']);
const TASK_LIVE = new Set(['queued', 'assigned', 'running', 'idle', 'waiting_input', 'suspended']);
const CHAT_LIVE = new Set(['starting', 'working', 'needs_you', 'suspended']);
const OBSERVED_LIVE = new Set(['running', 'waiting_input', 'suspended']);

/** Whether a task takes one of its machine's slots (observed sessions never do). */
export function occupiesSlot(t: Pick<Task, 'status' | 'origin'>): boolean {
  if (t.origin === 'external' || t.origin === 'scratch') return false;
  return t.status === 'assigned' || t.status === 'running' || t.status === 'waiting_input' || t.status === 'idle';
}

export function startOfToday(now = Date.now()): number {
  const d = new Date(now);
  d.setHours(0, 0, 0, 0);
  return d.getTime();
}

export function basename(p: string | undefined): string {
  if (!p) return '';
  const clean = p.replace(/[\\/]+$/, '');
  const i = Math.max(clean.lastIndexOf('/'), clean.lastIndexOf('\\'));
  return i >= 0 ? clean.slice(i + 1) : clean;
}

export function firstLine(text: string | undefined, max = 90): string {
  const line = (text ?? '').split('\n').map((l) => l.trim()).find((l) => l) ?? '';
  return line.length > max ? `${line.slice(0, max - 1)}…` : line;
}

export function lastLine(text: string | undefined, max = 120): string {
  const lines = (text ?? '').split('\n').map((l) => l.trim()).filter((l) => l);
  const line = lines[lines.length - 1] ?? '';
  return line.length > max ? `${line.slice(0, max - 1)}…` : line;
}

export function parseGitSummary(json: string | undefined): GitSummary | null {
  if (!json) return null;
  try {
    const g = JSON.parse(json) as GitSummary;
    return g && typeof g === 'object' ? g : null;
  } catch {
    return null;
  }
}

export function hasGitChanges(g: GitSummary | null): boolean {
  return !!g && ((g.commits?.length ?? 0) > 0 || !!g.diff_stat?.trim() || (g.dirty_files?.length ?? 0) > 0);
}

/** The project a task belongs to: its project, else its repo, else its directory. */
export function taskProject(t: Task, projects: readonly Project[], repos: readonly Repo[]): { key: string; name: string } {
  if (t.project_id && t.project_id !== DEFAULT_PROJECT_ID) {
    const p = projects.find((x) => x.id === t.project_id);
    if (p) return { key: `p:${p.id}`, name: p.name };
  }
  if (t.repo_id) {
    const r = repos.find((x) => x.id === t.repo_id);
    if (r) return { key: `d:${r.name}`, name: r.name };
  }
  const dir = basename(t.effective_dir || t.workdir);
  if (dir && dir !== '~') return { key: `d:${dir}`, name: dir };
  return { key: 'none', name: 'No project' };
}

export function chatProject(c: ChatSession): { key: string; name: string } {
  const dir = basename(c.cwd);
  if (dir && dir !== '~') return { key: `d:${dir}`, name: dir };
  return { key: 'chats', name: 'Chats' };
}

function isShell(t: Task): boolean {
  return t.agent === 'shell' || t.session_kind === 'terminal';
}

function taskNow(t: Task, w: Worker | undefined): string {
  switch (t.status) {
    case 'queued':
      return 'Queued: waiting for a free slot';
    case 'assigned':
      return `Starting on ${w?.name ?? 'its machine'}`;
    case 'waiting_input':
      return 'Waiting for your input';
    case 'suspended':
      return t.suspended_by === 'pause' ? 'Paused by Pause everything' : 'Suspended: processes frozen';
    case 'completed':
      return lastLine(t.summary) || 'Finished';
    case 'failed':
      return lastLine(t.summary) || `Failed${t.exit_code != null ? ` (exit ${t.exit_code})` : ''}`;
    case 'canceled':
      return 'Canceled';
  }
  return lastLine(t.summary) || (isShell(t) ? 'Shell open' : 'Working');
}

function chatNowText(status: string, running: string): string {
  switch (status) {
    case 'starting':
      return 'Starting';
    case 'working':
      return running || 'Working';
    case 'needs_you':
      return 'Waiting for your answer';
    case 'suspended':
      return 'Suspended: processes frozen';
    case 'failed':
      return 'The last turn failed';
    case 'cancelled':
      return 'The last turn was stopped';
  }
  return 'Idle: waiting for your next message';
}

/** Every piece of work the mode UI shows, newest first. */
export function buildWorkItems(inp: WorkInputs): WorkItem[] {
  const now = inp.now ?? Date.now();
  const today = startOfToday(now);
  const qByTask = new Map<string, number>();
  const qByChat = new Map<string, number>();
  for (const q of inp.questions) {
    if (!isPending(q)) continue;
    if (q.task_id) qByTask.set(q.task_id, (qByTask.get(q.task_id) ?? 0) + 1);
    else if (q.chat_id) qByChat.set(q.chat_id, (qByChat.get(q.chat_id) ?? 0) + 1);
  }
  const holdFor = (taskId: string, chatId: string) =>
    inp.holds.find((h) => h.scope_kind === 'session' && ((taskId && h.task_id === taskId) || (chatId && h.chat_id === chatId)));

  const out: WorkItem[] = [];
  for (const t of inp.tasks) {
    if (t.origin === 'scratch') continue;
    const w = inp.workers[t.worker_id];
    const questions = qByTask.get(t.id) ?? 0;
    const hold = holdFor(t.id, '');
    const finished = TASK_TERMINAL.has(t.status);
    const observed = t.origin === 'external';
    // An observed session sitting at its prompt is the user's own, not work in flight.
    const live = observed ? OBSERVED_LIVE.has(t.status) : TASK_LIVE.has(t.status);
    const stale = live && t.status !== 'queued' && !!w && w.status !== 'online';
    const git = parseGitSummary(t.git_summary_json);
    const hasChanges = t.status === 'completed' && hasGitChanges(git);
    const finishedAt = t.completed_at || undefined;
    const reviewed = inp.reviewed[t.id];
    let column: WorkColumn | null = null;
    if (t.status === 'queued' || t.status === 'assigned') column = 'queued';
    else if (t.status === 'waiting_input' || (live && questions > 0)) column = 'needs_you';
    else if (live) column = 'running';
    else if (observed) column = null;
    else if (hasChanges && !reviewed) column = 'review';
    else if (finished && Date.parse(finishedAt || t.created_at) >= today) column = 'done';
    let control: ControlLabel;
    if (finished) control = t.status === 'failed' ? 'FAILED' : t.status === 'canceled' ? 'STOPPED' : 'DONE';
    else if (inp.interactive(t.id)) control = 'YOU';
    else if (stale) control = 'STALE';
    else if (t.status === 'suspended') control = 'SUSPENDED';
    else if (hold) control = 'HELD';
    else if (t.status === 'waiting_input' || questions > 0) control = 'ASKING';
    else if (t.status === 'queued') control = 'QUEUED';
    else if (t.origin === 'orchestrator' || t.parent_chat_id) control = 'ORCH';
    else control = 'AUTO';
    const proj = taskProject(t, inp.projects, inp.repos);
    const goal = firstLine(t.prompt) || (isShell(t) ? 'Terminal' : `${t.agent} session`);
    out.push({
      id: t.id,
      sid: t.sid,
      kind: 'task',
      sessionKind: isShell(t) ? 'terminal' : 'agent',
      title: firstLine(t.prompt, 60) || (isShell(t) ? `${w?.name ?? 'shell'} terminal` : `${t.agent} · ${proj.name}`),
      agent: t.agent,
      workerId: t.worker_id,
      machine: w?.name ?? '',
      projectKey: proj.key,
      projectName: proj.name,
      status: t.status,
      column,
      control,
      live,
      stale,
      observed,
      origin: t.origin || 'orchestrator',
      goal,
      now: taskNow(t, w),
      questions,
      hold,
      hasChanges,
      createdAt: t.created_at,
      updatedAt: t.completed_at || t.started_at || t.created_at,
      finishedAt,
      task: t,
    });
  }

  for (const c of inp.chats) {
    // Fleet-tools chats are the orchestrator conversation (its own pinned tab).
    if (c.fleet_tools || !(c.backend === 'agent' || (!c.backend && c.agent))) continue;
    const status = inp.chatStatus?.(c.id) || c.status || 'idle';
    const w = c.worker_id ? inp.workers[c.worker_id] : undefined;
    const questions = qByChat.get(c.id) ?? 0;
    const hold = holdFor('', c.id);
    const live = CHAT_LIVE.has(status) || questions > 0;
    const stale = live && !!w && w.status !== 'online';
    const updated = c.updated_at || c.created_at;
    let column: WorkColumn | null = null;
    if (status === 'needs_you' || questions > 0) column = 'needs_you';
    else if (CHAT_LIVE.has(status)) column = 'running';
    else if (Date.parse(updated) >= today) column = 'done';
    let control: ControlLabel;
    if (stale) control = 'STALE';
    else if (status === 'suspended') control = 'SUSPENDED';
    else if (hold) control = 'HELD';
    else if (status === 'needs_you' || questions > 0) control = 'ASKING';
    else if (status === 'failed') control = 'FAILED';
    else if (!live) control = 'DONE';
    else control = 'AUTO';
    const proj = chatProject(c);
    out.push({
      id: c.id,
      sid: c.sid,
      kind: 'chat',
      sessionKind: 'chat',
      title: c.title || `${c.agent} chat`,
      agent: c.agent || 'agent',
      workerId: c.worker_id || '',
      machine: w?.name ?? '',
      projectKey: proj.key,
      projectName: proj.name,
      status,
      column,
      control,
      live,
      stale,
      observed: false,
      origin: 'direct',
      goal: c.title || 'Chat',
      now: chatNowText(status, inp.chatNow?.(c.id) ?? ''),
      questions,
      hold,
      hasChanges: false,
      createdAt: c.created_at,
      updatedAt: updated,
      finishedAt: live ? undefined : updated,
      chat: c,
    });
  }
  return out.sort((a, b) => (b.updatedAt || '').localeCompare(a.updatedAt || ''));
}

export interface ProjectGroup {
  key: string;
  name: string;
  items: WorkItem[];
  running: number;
  queued: number;
  asking: number;
}

const LIVE_ORDER: Record<ControlLabel, number> = {
  ASKING: 0,
  STALE: 1,
  HELD: 2,
  SUSPENDED: 3,
  YOU: 4,
  ORCH: 5,
  AUTO: 6,
  QUEUED: 7,
  FAILED: 8,
  STOPPED: 9,
  DONE: 10,
};

/** Live work grouped by project (busiest first); inside a group by attention, then age. */
export function groupByProject(items: readonly WorkItem[]): ProjectGroup[] {
  const map = new Map<string, ProjectGroup>();
  for (const it of items) {
    let g = map.get(it.projectKey);
    if (!g) {
      g = { key: it.projectKey, name: it.projectName, items: [], running: 0, queued: 0, asking: 0 };
      map.set(it.projectKey, g);
    }
    g.items.push(it);
    if (it.column === 'queued') g.queued++;
    else if (it.control === 'ASKING') g.asking++;
    else g.running++;
  }
  for (const g of map.values()) {
    g.items.sort((a, b) => LIVE_ORDER[a.control] - LIVE_ORDER[b.control] || (a.createdAt || '').localeCompare(b.createdAt || ''));
  }
  return [...map.values()].sort((a, b) => b.items.length - a.items.length || a.name.localeCompare(b.name));
}

/** "desk", "desk 3/4", "mac FULL 4/4", "wsl empty", "gpu-box offline 4m" for the fleet strip. */
export function machineLoadLabel(w: Worker, running: number, now = Date.now()): { text: string; tone: 'muted' | 'warn' | 'stale' } {
  if (w.status !== 'online') {
    const since = w.last_seen_at ? Math.max(0, Math.round((now - Date.parse(w.last_seen_at)) / 60000)) : 0;
    const ago = since >= 60 * 24 ? `${Math.floor(since / 1440)}d` : since >= 60 ? `${Math.floor(since / 60)}h` : `${since}m`;
    return { text: `${w.name} offline${since ? ` ${ago}` : ''}`, tone: 'stale' };
  }
  const cap = w.max_concurrent || 0;
  if (w.drained) return { text: `${w.name} draining ${running}/${cap || '∞'}`, tone: 'muted' };
  if (running === 0) return { text: `${w.name} empty`, tone: 'muted' };
  if (cap && running >= cap) return { text: `${w.name} FULL ${running}/${cap}`, tone: 'warn' };
  return { text: `${w.name} ${running}/${cap || '∞'}`, tone: 'muted' };
}

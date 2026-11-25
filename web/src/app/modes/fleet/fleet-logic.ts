import { HeatHistory, HeatSample, ProjectGroup, ShellInfo, SubagentActivity, WorkerHeat } from '../../core/models/activity.models';
import { Task, Worker } from '../../core/models/syre.models';
import { heatLevelOf } from '../../core/state/activity-logic';
import { WorkItem, occupiesSlot } from '../../core/state/work-logic';
import { SpinnerDetail, SpinnerState, activityPhrase, heatSolid, spinnerDetailOf, spinnerStateOf } from '../../shared/spinner-logic';

/** Fleet mode (signed-off "machines first" design): pure helpers; see fleet-mode.component.ts. */

export type HeatRange = '12h' | '24h' | '3d' | '1w';

export const HEAT_RANGES: ReadonlyArray<{ id: HeatRange; label: string; title: string; ms: number }> = [
  { id: '12h', label: '12h', title: '12 hours', ms: 12 * 3600_000 },
  { id: '24h', label: '24h', title: '24 hours', ms: 24 * 3600_000 },
  { id: '3d', label: '3d', title: '3 days', ms: 3 * 86400_000 },
  { id: '1w', label: '1w', title: 'week', ms: 7 * 86400_000 },
];

/** Buckets in a machine's heat strip. */
export const HEAT_STRIP_POINTS = 48;

/** A finished session stays on its machine's row this long. */
export const FLEET_DONE_MS = 5 * 60_000;

export type MachineBadge = 'RECONNECTING' | 'OFFLINE' | 'STARTING' | 'DRAINING' | 'FULL' | 'IDLE' | 'OK';

/** A machine's state badge (grey baseline; amber only for FULL). */
export function machineBadge(w: Worker, running: number): MachineBadge {
  if (w.reconnecting_after_sleep) return 'RECONNECTING';
  if (w.status !== 'online') return w.launch_state === 'starting' ? 'STARTING' : 'OFFLINE';
  if (w.drained) return 'DRAINING';
  if (w.max_concurrent && running >= w.max_concurrent) return 'FULL';
  if (running === 0) return 'IDLE';
  return 'OK';
}

/** Megabytes with one decimal ("12.3 MB"). */
function mb(bytes: number): string {
  return `${(bytes / 1048576).toFixed(1)} MB`;
}

/**
 * What a machine row says about its outbound spool, when there is something to say: the
 * replay after an outage, a backlog not yet sent, a full spool. Undefined otherwise.
 */
export function spoolNote(w: Worker, now: number): string | undefined {
  const s = w.spool;
  if (!s) return undefined;
  if (s.full) return `spool full (${s.full_reason || 'over budget'}): vitals and activity are no longer kept`;
  if (s.replaying && s.replay_total) return `replaying ${s.replay_sent ?? 0} of ${s.replay_total} records from the outage`;
  if (s.oldest_unsent_at) {
    const age = now - Date.parse(s.oldest_unsent_at);
    if (age > 30_000) return `${s.records} records waiting to be sent (oldest ${Math.round(age / 60_000)} min) · ${mb(s.bytes)}`;
  }
  return undefined;
}

/** The spool in one line for machine details: size of budget, backlog, oldest unsent. */
export function spoolSummary(w: Worker, now: number): string | undefined {
  const s = w.spool;
  if (!s) return undefined;
  const parts = [`${mb(s.bytes)} of ${mb(s.budget_bytes)}`];
  parts.push(s.records ? `${s.records} records not yet acknowledged` : 'everything acknowledged');
  if (s.oldest_unsent_at) parts.push(`oldest unsent ${Math.max(0, Math.round((now - Date.parse(s.oldest_unsent_at)) / 1000))} s ago`);
  if (s.replaying && s.replay_total) parts.push(`replay ${s.replay_sent ?? 0}/${s.replay_total}`);
  return parts.join(' · ');
}

/** "45s", "5m", "2h": a short wait. */
export function shortWait(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  return `${Math.round(s / 3600)}h`;
}

/**
 * What an offline machine row says about its syred: starting, failed, or (SYR-JAX1E)
 * "syred down — restarting (attempt 3, next in 5m): <last error>" while the hub
 * restarts a daemon that died, and "unreachable over SSH" while it re-dials a host.
 */
export function offlineText(w: Worker, now: number): string {
  const r = w.restart;
  const err = (w.launch_state === 'failed' && w.launch_error) || r?.error || '';
  const tail = err ? `: ${err}` : '';
  if (w.launch_state === 'starting') {
    return r?.attempt ? `syred down — restarting (attempt ${r.attempt})…` : 'starting syred…';
  }
  if (r?.state === 'down') {
    // A logon task cannot be restarted while its user is signed out: say why.
    if (w.facts?.start_mode === 'user_session') return `syred down${tail}. ${windowsStartText(w)}`;
    if (!r.attempt) return `syred down${tail}`;
    const next = r.next_at ? Date.parse(r.next_at) - now : 0;
    const when = next > 0 ? `next in ${shortWait(next)}` : 'next soon';
    return `syred down — restarting (attempt ${r.attempt}, ${when})${tail}`;
  }
  if (r?.state === 'unreachable') return `unreachable over SSH — re-dialing${r.error ? `: ${r.error}` : ''}`;
  if (w.launch_state === 'failed') return `could not start: ${w.launch_error || 'see Details'}`;
  if (w.facts?.start_mode === 'user_session') return windowsStartText(w);
  return 'offline: not reporting';
}

/**
 * When a Windows machine's syred starts (SYR-224K5), from what the daemon last
 * reported: a logon task only runs while its user is signed in, so a headless machine
 * needs an S4U task or a service. Empty for other systems or when unknown.
 */
export function windowsStartText(w: Worker): string {
  if (w.os !== 'windows') return '';
  const who = w.ssh_target ? w.ssh_target.split('@')[0] : 'its user';
  switch (w.facts?.start_mode) {
    case 'background_task':
      return 'Windows: syred starts at boot and runs whether or not anyone is signed in (S4U task).';
    case 'service':
      return 'Windows: syred runs as a Windows service.';
    case 'user_session':
      return (
        `Windows: syred starts only when ${who} signs in (logon task). For headless use, ` +
        'remove and re-add the machine over SSH as an administrator (that registers an S4U task), ' +
        'or run `syred install` from an elevated terminal on it, or `syred install --service`.'
      );
  }
  return '';
}

/** "macOS · arm64 · 10c / 16G" */
/** The transport a machine row shows: how its syred reaches the hub. */
export function transportLabel(w: Worker): string {
  return w.transport === 'ssh' ? 'ssh' : w.transport === 'local' ? 'local' : 'outbound';
}

/** The transport's icon (the badge shrinks to it on narrow screens): key, monitor, globe. */
export function transportIcon(w: Worker): 'key' | 'monitor' | 'globe' {
  return w.transport === 'ssh' ? 'key' : w.transport === 'local' ? 'monitor' : 'globe';
}

/** One line on what the transport means, for its tooltip and Details. */
export function transportHint(w: Worker): string {
  switch (w.transport) {
    case 'ssh':
      return `The hub attaches over SSH (${w.ssh_target || w.name}) with its own key; no credential`;
    case 'local':
      return "The hub's own machine: the hub attaches to syred's local socket";
    default:
      return 'syred connects out to the hub with its machine credential';
  }
}

export function machineSpec(w: Worker): string {
  const os = w.facts?.os_name || (w.os === 'darwin' ? 'macOS' : w.os);
  const cores = w.facts?.cpu_cores ? `${w.facts.cpu_cores}c` : '';
  const ram = w.facts?.ram_total_mb ? `${Math.round(w.facts.ram_total_mb / 1024)}G` : '';
  const size = [cores, ram].filter(Boolean).join(' / ');
  const virt = w.facts?.is_wsl ? 'WSL' : w.facts?.is_container ? 'container' : w.facts?.is_vm ? 'VM' : '';
  return [os, w.arch, size, virt].filter(Boolean).join(' · ');
}

/** "hot · cpu 88% · mem 71%" (the dominant cause first), or why there is none. */
export function heatText(w: Worker, heat: WorkerHeat | undefined): string {
  if (w.status !== 'online') return 'no heat data while offline';
  if (!heat) return 'no heat data yet';
  const comps = heat.components ?? {};
  const order = [heat.cause, ...(['cpu', 'mem', 'gpu', 'slots'] as const).filter((c) => c !== heat.cause)];
  const parts = order
    .filter((c) => comps[c as keyof typeof comps] != null)
    .slice(0, 2)
    .map((c) => `${c} ${Math.round((comps[c as keyof typeof comps] ?? 0) * 100)}%`);
  if (!parts.length) parts.push(heat.cause);
  return `${heatLevelOf(heat.score)} ${Math.round(heat.score * 100)}% · ${parts.join(' · ')}`;
}

/** Project chip colours (stable per project key, readable on dark cards). */
const PROJECT_COLORS = ['#c792ea', '#7fd1c7', '#f0b86e', '#82aaff', '#f78c6c', '#c3e88d', '#ff9cac', '#89ddff'];

export function projectColor(key: string): string {
  if (!key || key === 'other') return '#8f9199';
  let h = 0;
  for (let i = 0; i < key.length; i++) h = (h * 31 + key.charCodeAt(i)) >>> 0;
  return PROJECT_COLORS[h % PROJECT_COLORS.length];
}

export interface MachineSession {
  key: string;
  id: string;
  sid: string;
  title: string;
  now: string;
  /** The full tool line behind a summarised now (hover title). */
  nowTitle?: string;
  agent: string;
  project: string;
  projectColor: string;
  state: SpinnerState;
  coarse: boolean;
  tps?: number;
  done: boolean;
  item?: WorkItem;
  kind: 'task' | 'chat';
  /** Shells and sub-agents for the spinner. */
  detail: SpinnerDetail | null;
  /** The shells in detail (up to 12; detail.shells is the exact count). */
  shellList: ShellInfo[];
  subagents: SubagentActivity[];
}

/** A shell row on a Fleet session card. */
export interface ShellRow {
  pid: number;
  text: string;
  /** The full command (hover). */
  title: string;
  busy: boolean;
  dangling: boolean;
  /** "41s" (running a command for), "idle", or "idle 3m". */
  age: string;
  startedAt: string;
}

/** "41s", "12m", "3h", "2d". */
export function shortAge(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.round(m / 60);
  if (h < 48) return `${h}h`;
  return `${Math.round(h / 24)}d`;
}

/** The shell rows of a session card: busy ones show how long the command runs, idle ones "idle". */
export function shellRows(list: readonly ShellInfo[], now: number): ShellRow[] {
  return list.map((s) => ({
    pid: s.pid,
    text: s.command,
    title: `${s.summary && s.summary !== s.command ? `${s.summary}: ` : ''}${s.command} (pid ${s.pid}${s.dangling ? ', left running' : ''})`,
    busy: !!s.busy,
    dangling: !!s.dangling,
    age: s.busy ? shortAge(now - (Date.parse(s.since) || now)) : 'idle',
    startedAt: s.started_at,
  }));
}

/** "❯ 3 shells · 2 busy" / "❯ 2 shells left running" ('' when none). */
export function shellChip(d: SpinnerDetail | null, done: boolean): string {
  if (!d?.shells) return '';
  const n = `${d.shells} ${d.shells === 1 ? 'shell' : 'shells'}`;
  if (done) return `❯ ${n} left running`;
  return `❯ ${n}${d.shellsBusy ? ` · ${d.shellsBusy} busy` : ''}`;
}

/** "◉ 2 sub-agents" ('' when none). */
export function subagentChip(d: SpinnerDetail | null): string {
  const n = d?.subs.length ?? 0;
  return n ? `◉ ${n} ${n === 1 ? 'sub-agent' : 'sub-agents'}` : '';
}

/** A sub-agent row's text: "S3 client tests · running go test" (title, then what it does). */
export function subagentText(s: SubagentActivity): string {
  const doing = s.label || (s.state === 'speaking' ? 'writing' : s.state);
  return [s.title || 'sub-agent', doing].filter(Boolean).join(' · ');
}

/**
 * Whether the session's agent can be asked to stop a shell, and why not: a finished or
 * suspended task can't (its agent is gone or frozen); a chat always can (an idle chat gets
 * a follow-up message, a chat mid-turn gets it when the turn ends).
 */
export function askAvailability(s: Pick<MachineSession, 'kind' | 'done' | 'state'>): { ok: boolean; why: string } {
  if (s.state === 'held') return { ok: false, why: 'The session is suspended: resume it first, or kill the shell' };
  if (s.kind === 'task' && s.done) return { ok: false, why: 'The session has ended, so its agent can’t be asked: kill the shell instead' };
  if (s.kind === 'chat' && !s.done) return { ok: true, why: 'Ask the agent to stop it (sent when its current turn ends)' };
  return { ok: true, why: s.kind === 'chat' ? 'Ask the agent to stop it (a follow-up message)' : 'Ask the agent to stop it (typed into its session)' };
}

export interface MachineRow {
  w: Worker;
  running: number;
  badge: MachineBadge;
  heat: WorkerHeat | undefined;
  heatText: string;
  spec: string;
  sessions: MachineSession[];
  /** Free slots (capacity minus slots in use), for an online machine that takes work. */
  free: number;
  /** "cpu 45% · mem 71% · load 3.2": the whole machine, agents and everything else. */
  load: string;
  /** The next queued task placed on this machine, if any. */
  next: string;
  /** Sessions it hosts that are suspended (Suspend ↔ Resume). */
  suspended: number;
}

export interface RowInputs {
  workers: readonly Worker[];
  tasks: readonly Task[];
  groups: readonly ProjectGroup[];
  items: Record<string, WorkItem>;
  allItems: readonly WorkItem[];
  heat: Record<string, WorkerHeat>;
  now: number;
}

/** One row per machine with the sessions running on it (inside-out view of the projects). */
export function machineRows(inp: RowInputs): MachineRow[] {
  const running = new Map<string, number>();
  for (const t of inp.tasks) if (occupiesSlot(t)) running.set(t.worker_id, (running.get(t.worker_id) ?? 0) + 1);
  const byWorker = new Map<string, MachineSession[]>();
  const push = (wid: string, s: MachineSession) => {
    const list = byWorker.get(wid);
    if (list) list.push(s);
    else byWorker.set(wid, [s]);
  };
  const next = new Map<string, string>();
  for (const g of inp.groups) {
    for (const it of g.items) {
      if (!it.workerId) continue;
      const mi = inp.items[it.id];
      if (it.bucket === 'queued') {
        if (!next.has(it.workerId)) next.set(it.workerId, it.title);
        continue;
      }
      if (it.bucket === 'done') {
        if (it.observed) continue;
        const at = Date.parse(mi?.finishedAt || it.activity.since || '') || 0;
        // Shells left running (or background sub-agents) keep the card until they end.
        if (inp.now - at >= FLEET_DONE_MS && !(it.activity.shells ?? 0) && !it.activity.subagents?.length) continue;
      }
      const state = spinnerStateOf(it.activity, mi?.control);
      const doneAgo = inp.now - (Date.parse(mi?.finishedAt || it.activity.since || '') || inp.now);
      const now =
        it.nowSource === 'commander' || it.nowSource === 'activity'
          ? it.now
          : activityPhrase(state, it.activity.tokens_per_sec, it.activity.summary || it.activity.label, doneAgo) || it.now;
      push(it.workerId, {
        key: `${it.kind}:${it.id}`,
        id: it.id,
        sid: it.sid,
        title: it.title,
        now,
        nowTitle: it.nowTitle,
        agent: it.agent || mi?.agent || 'agent',
        project: g.name,
        projectColor: projectColor(g.key),
        state,
        coarse: !!it.activity.coarse && it.observed,
        tps: it.activity.state === 'speaking' ? it.activity.tokens_per_sec : undefined,
        done: it.bucket === 'done',
        item: mi,
        kind: it.kind,
        detail: spinnerDetailOf(it.activity),
        shellList: it.activity.shell_list ?? [],
        subagents: it.activity.subagents ?? [],
      });
    }
  }
  for (const mi of inp.allItems) {
    if (mi.sessionKind !== 'terminal' || !mi.live || !mi.workerId) continue;
    const you = mi.control === 'YOU';
    push(mi.workerId, {
      key: `task:${mi.id}`,
      id: mi.id,
      sid: mi.sid,
      title: mi.title,
      now: you ? 'you have control' : 'shell open',
      agent: 'shell',
      project: 'Yours',
      projectColor: projectColor('other'),
      state: you ? 'you' : 'working',
      coarse: !you,
      done: false,
      item: mi,
      kind: 'task',
      detail: null,
      shellList: [],
      subagents: [],
    });
  }
  const suspended = new Map<string, number>();
  for (const t of inp.tasks) if (t.status === 'suspended') suspended.set(t.worker_id, (suspended.get(t.worker_id) ?? 0) + 1);

  return inp.workers.map((w) => {
    const n = running.get(w.id) ?? 0;
    const heat = w.status === 'online' ? inp.heat[w.id] : undefined;
    const m = w.metrics;
    const load =
      w.status === 'online' && m
        ? [
            `cpu ${Math.round(Math.min(100, m.cpu_percent))}%`,
            m.mem_total_mb ? `mem ${Math.round((m.mem_used_mb / m.mem_total_mb) * 100)}%` : '',
            m.gpu_percent != null ? `gpu ${Math.round(m.gpu_percent)}%` : '',
            m.load_avg ? `load ${m.load_avg.toFixed(1)}` : '',
          ]
            .filter(Boolean)
            .join(' · ')
        : '';
    const takesWork = w.status === 'online' && !w.drained;
    return {
      w,
      running: n,
      badge: machineBadge(w, n),
      heat,
      heatText: heatText(w, heat),
      spec: machineSpec(w),
      sessions: (byWorker.get(w.id) ?? []).sort((a, b) => Number(a.done) - Number(b.done)),
      free: takesWork ? Math.max(0, (w.max_concurrent || 0) - n) : 0,
      load,
      next: next.get(w.id) ?? '',
      suspended: suspended.get(w.id) ?? 0,
    };
  });
}

export interface StripCell {
  /** CSS colour, or null for no data (drawn hatched). */
  color: string | null;
  sample: HeatSample | null;
}

/**
 * A machine's heat strip from the history (all no-data when the hub has none). The hub may
 * return fewer points than asked (hourly ranges have at most one per hour): the strip has as
 * many cells as the history has points.
 */
export function heatStrip(history: HeatHistory | null, workerId: string, points = HEAT_STRIP_POINTS): StripCell[] {
  const row = history?.workers.find((x) => x.worker_id === workerId);
  const samples = row?.samples ?? [];
  const n = history?.workers.length ? history.workers[0].samples.length : points;
  return Array.from({ length: n }, (_, i) => {
    const s = samples[i] ?? null;
    return { color: s ? heatSolid(s.max) : null, sample: s };
  });
}

/** "desk heat over the last 12 hours: mostly warm, peak hot 91% (cpu); no data for 25%". */
export function describeStrip(name: string, cells: readonly StripCell[], rangeTitle: string): string {
  const have = cells.filter((c) => c.sample);
  if (!have.length) return `${name}: no heat history for the last ${rangeTitle}`;
  const counts = { cool: 0, warm: 0, hot: 0 };
  let peak = have[0].sample!;
  for (const c of have) {
    counts[heatLevelOf(c.sample!.max)]++;
    if (c.sample!.max > peak.max) peak = c.sample!;
  }
  const mostly = (Object.keys(counts) as Array<keyof typeof counts>).reduce((a, b) => (counts[b] > counts[a] ? b : a));
  const gap = cells.length - have.length;
  const parts = [`${name} heat over the last ${rangeTitle}: mostly ${mostly}, peak ${heatLevelOf(peak.max)} ${Math.round(peak.max * 100)}% (${peak.cause})`];
  if (gap) parts.push(`no data for ${Math.round((gap / cells.length) * 100)}% of it`);
  return parts.join('; ');
}

/** Axis labels at the start, middle and end of the range. */
export function stripTicks(start: number, end: number, range: HeatRange): Array<{ left: number; label: string }> {
  const out: Array<{ left: number; label: string }> = [];
  for (let i = 0; i <= 2; i++) {
    const at = start + ((end - start) * i) / 2;
    const d = new Date(at);
    const label =
      i === 2
        ? 'now'
        : range === '12h' || range === '24h'
          ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false })
          : d.toLocaleDateString([], { weekday: 'short', hour: '2-digit', hour12: false });
    out.push({ left: i * 50, label });
  }
  return out;
}

export interface QueueEntry {
  id: string;
  sid: string;
  title: string;
  /** Why it waits, one fact per machine-or-reason ("mac FULL (4/4)", "desk ✗ draining"). */
  reasons: Array<{ text: string; tone: 'warn' | 'muted' }>;
}

/**
 * Why each queued task waits, from what the browser knows: a task placed on a machine
 * waits for that machine (offline, draining, full); an unplaced one waits for any machine
 * with the agent and a free slot. The scheduler's own selector (OS, tools, tags) isn't
 * exposed, so those constraints show as "no free machine with <agent>".
 */
export function queueReasons(tasks: readonly Task[], workers: readonly Worker[], titleOf: (t: Task) => string): QueueEntry[] {
  const running = new Map<string, number>();
  for (const t of tasks) if (occupiesSlot(t)) running.set(t.worker_id, (running.get(t.worker_id) ?? 0) + 1);
  const byId = new Map(workers.map((w) => [w.id, w]));
  const why = (w: Worker): { text: string; tone: 'warn' | 'muted' } | null => {
    if (w.status !== 'online') return { text: `${w.name} offline`, tone: 'muted' };
    if (w.drained) return { text: `${w.name} draining`, tone: 'muted' };
    const n = running.get(w.id) ?? 0;
    const cap = w.max_concurrent || 1;
    if (n >= cap) {
      const hot = w.heat && heatLevelOf(w.heat.score) === 'hot' && w.heat.cause !== 'slots' ? ' and hot' : '';
      return { text: `${w.name} FULL (${n}/${cap})${hot}`, tone: 'warn' };
    }
    return null;
  };
  const out: QueueEntry[] = [];
  for (const t of tasks) {
    if (t.status !== 'queued' || t.origin === 'scratch') continue;
    const reasons: QueueEntry['reasons'] = [];
    const placed = t.worker_id ? byId.get(t.worker_id) : undefined;
    if (placed) {
      reasons.push(why(placed) ?? { text: `placed on ${placed.name}: starting soon`, tone: 'muted' });
    } else {
      const able = workers.filter((w) => !w.agents?.length || w.agents.includes(t.agent));
      if (!able.length) reasons.push({ text: `no machine has ${t.agent}`, tone: 'warn' });
      for (const w of able) {
        const r = why(w);
        if (r) reasons.push(r);
      }
      if (!reasons.length) reasons.push({ text: `a free machine with ${t.agent} should take it shortly`, tone: 'muted' });
      for (const w of workers) if (!able.includes(w)) reasons.push({ text: `${w.name} ✗ no ${t.agent}`, tone: 'muted' });
    }
    out.push({ id: t.id, sid: t.sid, title: titleOf(t), reasons });
  }
  return out;
}

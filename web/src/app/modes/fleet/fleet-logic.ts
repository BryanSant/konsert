import { HeatHistory, HeatSample, ProjectGroup, WorkerHeat } from '../../core/models/activity.models';
import { Task, Worker } from '../../core/models/konsert.models';
import { heatLevelOf } from '../../core/state/activity-logic';
import { WorkItem, occupiesSlot } from '../../core/state/work-logic';
import { SpinnerState, heatSolid, spinnerStateOf } from '../../shared/spinner-logic';

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

export type MachineBadge = 'OFFLINE' | 'STARTING' | 'DRAINING' | 'FULL' | 'IDLE' | 'OK';

/** A machine's state badge (grey baseline; amber only for FULL). */
export function machineBadge(w: Worker, running: number): MachineBadge {
  if (w.status !== 'online') return w.launch_state === 'starting' ? 'STARTING' : 'OFFLINE';
  if (w.drained) return 'DRAINING';
  if (w.max_concurrent && running >= w.max_concurrent) return 'FULL';
  if (running === 0) return 'IDLE';
  return 'OK';
}

/** "macOS · arm64 · 10c / 16G" */
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
  agent: string;
  project: string;
  projectColor: string;
  state: SpinnerState;
  coarse: boolean;
  tps?: number;
  done: boolean;
  item?: WorkItem;
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
        if (inp.now - at >= FLEET_DONE_MS) continue;
      }
      push(it.workerId, {
        key: `${it.kind}:${it.id}`,
        id: it.id,
        sid: it.sid,
        title: it.title,
        now: it.now,
        agent: it.agent || mi?.agent || 'agent',
        project: g.name,
        projectColor: projectColor(g.key),
        state: spinnerStateOf(it.activity, mi?.control),
        coarse: !!it.activity.coarse && it.observed,
        tps: it.activity.state === 'speaking' ? it.activity.tokens_per_sec : undefined,
        done: it.bucket === 'done',
        item: mi,
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

/** A machine's heat strip from the history (all no-data when the hub has none). */
export function heatStrip(history: HeatHistory | null, workerId: string, points = HEAT_STRIP_POINTS): StripCell[] {
  const row = history?.workers.find((x) => x.worker_id === workerId);
  const samples = row?.samples ?? [];
  return Array.from({ length: points }, (_, i) => {
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

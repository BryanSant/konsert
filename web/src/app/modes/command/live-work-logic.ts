import { OTHER_WORK_KEY, OTHER_WORK_NAME, ProjectGroup, WorkItem as ActivityItem, WorkerHeat } from '../../core/models/activity.models';
import { Worker } from '../../core/models/syre.models';
import { WorkItem } from '../../core/state/work-logic';
import { SpinnerDetail, SpinnerMachine, SpinnerState, activityPhrase, spinnerDetailOf, spinnerMachine, spinnerStateOf, withDetail } from '../../shared/spinner-logic';

/**
 * Command's "Live work" story (signed-off design): per project its name, the commander's
 * headline, a segmented progress bar, "N in flight on M machines · K queued", the in-flight
 * lines, the queue collapsed, and done lines that fade out over a few minutes. Pure; the
 * component wires it to WorkStore / FleetStore.
 */

/** Done lines stay this long, fading out (ms). */
export const DONE_FADE_MS = 5 * 60_000;

export interface StoryLine {
  key: string;
  id: string;
  sid: string;
  kind: 'task' | 'chat';
  title: string;
  now: string;
  /** The full tool line behind a summarised now (hover title). */
  nowTitle?: string;
  /** The narration line was written by the commander. */
  nowByCommander: boolean;
  state: SpinnerState;
  coarse: boolean;
  tps?: number;
  agent: string;
  machines: SpinnerMachine[];
  /** Shells and sub-agents (compact badges on the spinner). */
  detail: SpinnerDetail | null;
  /** 1 = fully visible; done lines fade toward 0.35 before they drop out. */
  opacity: number;
  observed: boolean;
  terminal: boolean;
  /** The mode-UI item (to open its tab); absent for items the tab strip can't show. */
  item?: WorkItem;
}

export interface StoryProject {
  key: string;
  name: string;
  headline: string;
  headlineByCommander: boolean;
  /** Segment counts for the bar: done (recent), running (incl. held), needs you, queued. */
  bar: { done: number; running: number; needsYou: number; queued: number };
  total: number;
  summary: string;
  inFlight: StoryLine[];
  queued: StoryLine[];
  done: StoryLine[];
}

export interface StoryInputs {
  groups: readonly ProjectGroup[];
  /** Mode-UI items by id (control label, terminals, finished time). */
  items: Record<string, WorkItem>;
  /** Every mode-UI item (for live terminals, which aren't live-work items). */
  allItems: readonly WorkItem[];
  workers: Record<string, Worker>;
  heat: Record<string, WorkerHeat>;
  now: number;
  /** Start of today: the bar's "done" counts what finished today. */
  today: number;
}

function machinesFor(workerId: string, inp: StoryInputs): SpinnerMachine[] {
  const w = workerId ? inp.workers[workerId] : undefined;
  if (!w) return [];
  return [spinnerMachine(w.name, w.status === 'online' ? inp.heat[w.id] : null)];
}

function doneAt(it: ActivityItem, mi: WorkItem | undefined): number {
  return Date.parse(mi?.finishedAt || it.activity.since || it.activity.at || it.createdAt || '') || 0;
}

function lineFor(it: ActivityItem, inp: StoryInputs): StoryLine {
  const mi = inp.items[it.id];
  const state = spinnerStateOf(it.activity, mi?.control);
  let opacity = 1;
  if (it.bucket === 'done') {
    const age = Math.max(0, inp.now - doneAt(it, mi));
    opacity = Math.max(0.35, 0.75 - (0.4 * age) / DONE_FADE_MS);
  }
  const tps = it.activity.state === 'speaking' ? it.activity.tokens_per_sec : undefined;
  let now = it.now;
  if (state === 'queued' && !it.workerId && it.nowSource !== 'commander') now = it.activity.label || 'Waiting for a machine';
  else if (it.nowSource !== 'commander' && it.nowSource !== 'activity') now = activityPhrase(state, tps, it.activity.summary || it.activity.label, inp.now - doneAt(it, mi)) || now;
  const detail = spinnerDetailOf(it.activity);
  if (it.nowSource !== 'commander') now = withDetail(now, state, detail);
  return {
    key: `${it.kind}:${it.id}`,
    id: it.id,
    sid: it.sid,
    kind: it.kind,
    title: it.title,
    now,
    nowTitle: it.nowTitle,
    nowByCommander: it.nowSource === 'commander',
    state,
    coarse: !!it.activity.coarse && it.observed,
    tps: it.activity.state === 'speaking' ? it.activity.tokens_per_sec : undefined,
    agent: it.agent || mi?.agent || '',
    machines: machinesFor(it.workerId, inp),
    detail,
    opacity,
    observed: it.observed,
    terminal: false,
    item: mi,
  };
}

function terminalLine(mi: WorkItem, inp: StoryInputs): StoryLine {
  const you = mi.control === 'YOU';
  return {
    key: `task:${mi.id}`,
    id: mi.id,
    sid: mi.sid,
    kind: 'task',
    title: mi.title,
    now: you ? 'you have control' : mi.stale ? 'machine not reporting' : 'shell open',
    nowByCommander: false,
    state: you ? 'you' : mi.control === 'SUSPENDED' || mi.control === 'HELD' ? 'held' : 'working',
    coarse: !you,
    agent: 'terminal',
    machines: machinesFor(mi.workerId, inp),
    detail: null,
    opacity: 1,
    observed: false,
    terminal: true,
    item: mi,
  };
}

function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`;
}

/** The story for every project with something in flight, queued or just done. */
export function buildStory(inp: StoryInputs): StoryProject[] {
  const out: StoryProject[] = [];
  let other: StoryProject | null = null;
  for (const g of inp.groups) {
    const inFlight: StoryLine[] = [];
    const queued: StoryLine[] = [];
    const done: StoryLine[] = [];
    let doneToday = 0;
    for (const it of g.items) {
      if (it.bucket === 'done') {
        const at = doneAt(it, inp.items[it.id]);
        // Only work Syre ran counts toward the bar (an idle observed session isn't "done").
        if (at >= inp.today && !it.observed) doneToday++;
        // Shells still running after the end keep the line until they exit.
        const running = (it.activity.shells ?? 0) > 0 || !!it.activity.subagents?.length;
        if ((inp.now - at < DONE_FADE_MS || running) && !it.observed) done.push(lineFor(it, inp));
        continue;
      }
      if (it.bucket === 'queued') queued.push(lineFor(it, inp));
      else inFlight.push(lineFor(it, inp));
    }
    if (!inFlight.length && !queued.length && !done.length) continue;
    const needsYou = inFlight.filter((l) => l.state === 'asking').length;
    const p: StoryProject = {
      key: g.key,
      name: g.name,
      headline: g.headline,
      headlineByCommander: g.headlineSource === 'commander',
      bar: { done: doneToday, running: inFlight.length - needsYou, needsYou, queued: queued.length },
      total: 0,
      summary: '',
      inFlight,
      queued,
      done,
    };
    if (g.key === OTHER_WORK_KEY) other = p;
    else out.push(p);
  }

  // Your terminals live under "Other work" (they aren't work items, but they are live).
  const terms = inp.allItems.filter((i) => i.sessionKind === 'terminal' && i.live).map((i) => terminalLine(i, inp));
  if (terms.length) {
    other ??= {
      key: OTHER_WORK_KEY,
      name: OTHER_WORK_NAME,
      headline: '',
      headlineByCommander: false,
      bar: { done: 0, running: 0, needsYou: 0, queued: 0 },
      total: 0,
      summary: '',
      inFlight: [],
      queued: [],
      done: [],
    };
    other.inFlight.push(...terms);
    other.bar.running += terms.length;
  }
  if (other) {
    if (!other.headlineByCommander) other.headline = 'Sessions you started yourself, plus your terminals';
    out.push(other);
  }

  for (const p of out) {
    const b = p.bar;
    p.total = b.done + b.running + b.needsYou + b.queued;
    const machines = new Set(p.inFlight.map((l) => l.machines[0]?.name).filter(Boolean));
    const parts: string[] = [];
    if (p.inFlight.length) parts.push(`${p.inFlight.length} in flight${machines.size ? ` on ${plural(machines.size, 'machine')}` : ''}`);
    if (b.queued) parts.push(`${b.queued} queued`);
    if (b.needsYou) parts.push(`${b.needsYou} need${b.needsYou === 1 ? 's' : ''} you`);
    if (b.done) parts.push(`${b.done} done today`);
    p.summary = parts.join(' · ') || 'nothing running';
  }
  return out;
}

/** "▸ 7 queued · next: Eviction policy" */
export function queuedLabel(p: StoryProject): string {
  const next = p.queued[0];
  return `${p.queued.length} queued${next ? ` · next: ${next.title}` : ''}`;
}

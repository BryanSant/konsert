import { ActivityState, HeatCause, SessionActivity, SubagentState, WorkerHeat } from '../core/models/activity.models';
import { heatLevelOf } from '../core/state/activity-logic';
import { ControlLabel } from '../core/state/work-logic';

/**
 * The live-work spinner (signed-off design, style A): the avatar fill is the machine and how
 * hot it runs, the ring around it is what the agent is doing. Pure helpers here; the
 * component is shared/activity-spinner.component.ts.
 */

/**
 * The ring states: the eight activity states, with blocked split into held and "you", and
 * working split off as waiting when the parent only waits on its sub-agents.
 */
export type SpinnerState = 'thinking' | 'working' | 'waiting' | 'speaking' | 'asking' | 'held' | 'you' | 'queued' | 'idle' | 'done' | 'error';

/** Shells and sub-agents drawn around the ring (badge and orbiting dots). */
export interface SpinnerDetail {
  shells: number;
  shellsBusy: number;
  /** Sub-agent states, in start order. */
  subs: SubagentState[];
}

/** At most this many sub-agent dots orbit the ring; the rest show as "+N". */
export const MAX_DOTS = 4;

/** One machine as the avatar shows it. */
export interface SpinnerMachine {
  name: string;
  /** 0..1, or null when unknown (offline, no heartbeat yet). */
  heat: number | null;
  cause?: HeatCause | string;
}

/** The word shown next to (and spoken for) each state; colour is never the only signal. */
export const SPINNER_WORDS: Record<SpinnerState, string> = {
  thinking: 'thinking',
  working: 'working',
  waiting: 'waiting on sub-agents',
  speaking: 'writing',
  asking: 'asks you',
  held: 'held',
  you: 'you have control',
  queued: 'queued',
  idle: 'idle',
  done: 'done',
  error: 'error',
};

/** Text colour of the line next to a spinner, by state (mockup LC table, ≥ 4.5:1 on chrome). */
export const SPINNER_LABEL_COLORS: Record<SpinnerState, string> = {
  thinking: '#c9bcff',
  working: '#c9cbd1',
  waiting: '#9a9ca4',
  speaking: '#a8e8c2',
  asking: '#e9c27f',
  held: '#9ec5ff',
  you: '#9ec5ff',
  queued: '#8f9199',
  idle: '#9a9ca4',
  done: '#8f9199',
  error: '#f3a3a3',
};

/**
 * The ring state for a session from its activity and its control annunciator. A sub-agent
 * asking turns the whole ring amber (asking); a parent that only waits on its sub-agents
 * shows a plain grey ring (waiting) and lets the dots carry the motion.
 */
export function spinnerStateOf(activity: Pick<SessionActivity, 'state' | 'waiting' | 'subagents'> | undefined, control?: ControlLabel): SpinnerState {
  if (control === 'YOU') return 'you';
  if (control === 'HELD' || control === 'SUSPENDED') return 'held';
  const s: ActivityState = activity?.state ?? 'done';
  if (s === 'blocked') return 'held';
  if (s === 'done' || s === 'error' || s === 'queued') return s;
  if (activity?.subagents?.some((x) => x.state === 'asking')) return 'asking';
  if (s === 'working' && activity?.waiting) return 'waiting';
  return s;
}

/** The shells and sub-agents of a session for its spinner; null when it has none. */
export function spinnerDetailOf(a: Pick<SessionActivity, 'state' | 'shells' | 'shells_busy' | 'subagents'> | undefined | null): SpinnerDetail | null {
  if (!a) return null;
  const shells = a.shells ?? 0;
  // After the turn only background sub-agents remain (syred drops the others).
  const subs = (a.subagents ?? []).map((s) => s.state);
  if (!shells && !subs.length) return null;
  return { shells, shellsBusy: Math.min(a.shells_busy ?? 0, shells), subs };
}

/**
 * The counts a live-work line mentions: "3 shells · 2 sub-agents" (with "left running"
 * once the session is done). '' when none.
 */
export function detailPhrase(d: SpinnerDetail | null | undefined, done = false): string {
  if (!d) return '';
  const parts: string[] = [];
  if (d.shells) parts.push(`${d.shells} ${d.shells === 1 ? 'shell' : 'shells'}${done ? ' left running' : ''}`);
  if (d.subs.length) parts.push(`${d.subs.length} ${d.subs.length === 1 ? 'sub-agent' : 'sub-agents'}`);
  return parts.join(' · ');
}

/**
 * A line's now text with its shells and sub-agents: a generic phrase ("thinking…",
 * "waiting on sub-agents") gives way to the counts, anything specific keeps them after it.
 */
export function withDetail(now: string, state: SpinnerState, d: SpinnerDetail | null | undefined): string {
  const counts = detailPhrase(d, state === 'done' || state === 'error');
  if (!counts || state === 'asking') return now;
  if (!now || now === 'thinking…' || now === 'waiting on sub-agents' || now === 'working') return counts;
  return `${now} · ${counts}`;
}

/** "3 shells (2 busy) · 2 sub-agents" ('' when none); counts appear only when non-zero. */
export function describeDetail(d: SpinnerDetail | null | undefined): string {
  if (!d) return '';
  const parts: string[] = [];
  if (d.shells) parts.push(`${d.shells} ${d.shells === 1 ? 'shell' : 'shells'}${d.shellsBusy ? ` (${d.shellsBusy} busy)` : ''}`);
  if (d.subs.length) parts.push(`${d.subs.length} ${d.subs.length === 1 ? 'sub-agent' : 'sub-agents'}`);
  return parts.join(' · ');
}

/** The orbiting dots: up to MAX_DOTS, evenly spread, plus how many more there are. */
export function orbitDots(subs: readonly SubagentState[]): { dots: Array<{ state: SubagentState; angle: number }>; more: number } {
  const shown = subs.slice(0, MAX_DOTS);
  const step = shown.length ? 360 / shown.length : 0;
  return { dots: shown.map((state, k) => ({ state, angle: Math.round(k * step + 30) })), more: Math.max(0, subs.length - MAX_DOTS) };
}

/** Two-letter machine initials: "mini-pc" → "mp", "desk" → "dk", "mac" → "mc". */
export function machineInitials(name: string): string {
  const clean = (name || '').trim().toLowerCase();
  if (!clean) return '··';
  const parts = clean.split(/[^a-z0-9]+/).filter(Boolean);
  if (parts.length >= 2) return parts[0][0] + parts[1][0];
  const w = parts[0] ?? clean;
  return w.length <= 2 ? w : w[0] + w[w.length - 1];
}

/** Heat gradient stops and drift period (seconds) by score: cool blue/teal → amber → red. */
const HEAT_STOPS: ReadonlyArray<[number, [string, string, string], number]> = [
  [0.2, ['#1e3a8a', '#1d4ed8', '#0e7490'], 14],
  [0.4, ['#1d4ed8', '#0e7490', '#0f766e'], 12],
  [0.6, ['#0f766e', '#0e7490', '#a16207'], 10],
  [0.75, ['#a16207', '#b45309', '#0f766e'], 8],
  [0.9, ['#c2410c', '#dc2626', '#b45309'], 6],
  [1.01, ['#dc2626', '#9f1239', '#ea580c'], 4.5],
];

function stopFor(h: number): [number, [string, string, string], number] {
  return HEAT_STOPS.find(([t]) => h < t) ?? HEAT_STOPS[HEAT_STOPS.length - 1];
}

/** The drifting gradient for a heat score (the animation itself is in CSS). */
export function heatFill(h: number | null): { background: string; drift: string } {
  if (h == null) return { background: '#3a3c42', drift: '0s' };
  const [, c, s] = stopFor(h);
  return { background: `linear-gradient(120deg, ${c[0]}, ${c[1]}, ${c[2]}, ${c[0]})`, drift: `${s}s` };
}

/** One solid colour for a heat score (heat strip cells); null = no data. */
export function heatSolid(h: number | null): string | null {
  if (h == null) return null;
  return stopFor(h)[1][1];
}

/** "cool" / "warm" / "hot" text colour (≥ 4.5:1). */
export function heatTextColor(h: number | null): string {
  if (h == null) return 'var(--sy-text-muted)';
  const l = heatLevelOf(h);
  return l === 'cool' ? '#9ec5ff' : l === 'warm' ? '#e9c27f' : '#f3a3a3';
}

/** "desk · warm 55% · cpu" (or "desk · no heat data"). */
export function describeMachine(m: SpinnerMachine): string {
  if (m.heat == null) return `${m.name} · no heat data`;
  return `${m.name} · ${heatLevelOf(m.heat)} ${Math.round(m.heat * 100)}%${m.cause ? ` · ${m.cause}` : ''}`;
}

/**
 * The hover text and accessible name: "desk · warm 55% · cpu — thinking · 3 shells (2
 * busy) · 2 sub-agents" (the machine, then what the agent does).
 */
export function describeSpinner(
  state: SpinnerState,
  machines: readonly SpinnerMachine[],
  opts: { coarse?: boolean; tps?: number; agent?: string; detail?: SpinnerDetail | null } = {},
): string {
  let word = SPINNER_WORDS[state];
  if (state === 'speaking' && opts.tps) word += ` ${Math.round(opts.tps)} tok/s`;
  if (opts.coarse) word += ' (observed, coarse)';
  const who = opts.agent ? `${opts.agent} ` : '';
  const what = [`${who}${word}`.trim(), describeDetail(opts.detail)].filter(Boolean).join(' · ');
  if (!machines.length) return state === 'queued' ? `${what} · not placed yet` : what;
  return `${machines.map(describeMachine).join(' + ')} — ${what}`;
}

/** The machine as the avatar shows it, from a worker and its heat. */
export function spinnerMachine(name: string, heat: WorkerHeat | undefined | null): SpinnerMachine {
  return { name, heat: heat ? heat.score : null, cause: heat?.cause };
}

/** Speaking ripple period: faster with more output (1.5 s idle → 0.5 s at 80 tok/s). */
export function rippleSeconds(tps: number | undefined): number {
  const t = Math.max(0, Math.min(tps ?? 40, 80));
  return Math.round((1.5 - t / 80) * 100) / 100;
}

/**
 * What a line says when nothing more specific is known (no narration, no tool label): the
 * activity itself, instead of repeating the title. '' keeps the caller's text.
 */
export function activityPhrase(state: SpinnerState, tps: number | undefined, label: string | undefined, agoMs: number): string {
  switch (state) {
    case 'thinking':
      return 'thinking…';
    case 'waiting':
      return 'waiting on sub-agents';
    case 'speaking':
      return tps ? `writing · ${Math.round(tps)} tok/s` : 'writing…';
    case 'working':
      return label || 'working';
    case 'idle': {
      const m = Math.max(0, Math.round(agoMs / 60_000));
      return m ? `idle · last turn ${m}m ago` : 'idle';
    }
    case 'done': {
      const m = Math.max(0, Math.round(agoMs / 60_000));
      return `${label && label !== 'Ready' ? `${label} · ` : ''}done ${m ? `${m}m ago` : 'just now'}`;
    }
    case 'held':
      return label || 'held';
    case 'asking':
      // A sub-agent's question already names who asks.
      return label ? (label.startsWith('Sub-agent') ? label : `asks you: ${label}`) : 'asks you';
    default:
      return '';
  }
}

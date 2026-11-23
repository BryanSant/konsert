import { ActivityState, HeatCause, SessionActivity, WorkerHeat } from '../core/models/activity.models';
import { heatLevelOf } from '../core/state/activity-logic';
import { ControlLabel } from '../core/state/work-logic';

/**
 * The live-work spinner (signed-off design, style A): the avatar fill is the machine and how
 * hot it runs, the ring around it is what the agent is doing. Pure helpers here; the
 * component is shared/activity-spinner.component.ts.
 */

/** The ring states: the eight activity states, with blocked split into held and "you". */
export type SpinnerState = 'thinking' | 'working' | 'speaking' | 'asking' | 'held' | 'you' | 'queued' | 'done' | 'error';

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
  speaking: 'writing',
  asking: 'asks you',
  held: 'held',
  you: 'you have control',
  queued: 'queued',
  done: 'done',
  error: 'error',
};

/** Text colour of the line next to a spinner, by state (mockup LC table, ≥ 4.5:1 on chrome). */
export const SPINNER_LABEL_COLORS: Record<SpinnerState, string> = {
  thinking: '#c9bcff',
  working: '#c9cbd1',
  speaking: '#a8e8c2',
  asking: '#e9c27f',
  held: '#9ec5ff',
  you: '#9ec5ff',
  queued: '#8f9199',
  done: '#8f9199',
  error: '#f3a3a3',
};

/** The ring state for a session from its activity and its control annunciator. */
export function spinnerStateOf(activity: Pick<SessionActivity, 'state'> | undefined, control?: ControlLabel): SpinnerState {
  if (control === 'YOU') return 'you';
  if (control === 'HELD' || control === 'SUSPENDED') return 'held';
  const s: ActivityState = activity?.state ?? 'done';
  if (s === 'blocked') return 'held';
  return s;
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
  if (h == null) return 'var(--ks-text-muted)';
  const l = heatLevelOf(h);
  return l === 'cool' ? '#9ec5ff' : l === 'warm' ? '#e9c27f' : '#f3a3a3';
}

/** "desk · warm 55% · cpu" (or "desk · no heat data"). */
export function describeMachine(m: SpinnerMachine): string {
  if (m.heat == null) return `${m.name} · no heat data`;
  return `${m.name} · ${heatLevelOf(m.heat)} ${Math.round(m.heat * 100)}%${m.cause ? ` · ${m.cause}` : ''}`;
}

/** The hover text and accessible name: "working · desk · warm 55% · cpu". */
export function describeSpinner(state: SpinnerState, machines: readonly SpinnerMachine[], opts: { coarse?: boolean; tps?: number; agent?: string } = {}): string {
  let word = SPINNER_WORDS[state];
  if (state === 'speaking' && opts.tps) word += ` ${Math.round(opts.tps)} tok/s`;
  if (opts.coarse) word += ' (observed, coarse)';
  const who = opts.agent ? `${opts.agent} ` : '';
  const where = machines.length ? machines.map(describeMachine).join(' + ') : state === 'queued' ? 'not placed yet' : '';
  return [`${who}${word}`.trim(), where].filter(Boolean).join(' · ');
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

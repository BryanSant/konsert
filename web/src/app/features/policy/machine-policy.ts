import { MachinePolicy } from '../../core/models/syre.models';

/** Machine policy in the PWA (SYR-V4RER): pure helpers for the editor and the audit log. */

export const WORK_CLASSES = [
  { id: 'chat', label: 'Agent chats' },
  { id: 'task', label: 'Tasks and terminals' },
  { id: 'ticket', label: 'Ticket runs' },
] as const;

export const DAYS = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'] as const;

export interface QuietWindowDraft {
  start: string;
  end: string;
  days: string[];
}

/** The editor's fields: numbers as strings so an empty box means "no limit". */
export interface PolicyDraft {
  maxSessions: string;
  maxLoad: string;
  reserveCpu: string;
  minFreeRamMb: string;
  reserveRamMb: string;
  quietHours: QuietWindowDraft[];
  timeZone: string;
  allowedWork: string[];
}

export function browserTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  } catch {
    return 'UTC';
  }
}

/** IANA zones for the picker (the browser's list, else just the browser's zone and UTC). */
export function timeZones(): string[] {
  const own = browserTimeZone();
  try {
    const list =
      (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf?.(
        'timeZone',
      ) ?? [];
    const all = new Set([own, 'UTC', ...list]);
    return [...all];
  } catch {
    return [own, 'UTC'];
  }
}

const num = (v: number | undefined): string => (v ? String(v) : '');

export function draftFromPolicy(p: MachinePolicy | undefined | null): PolicyDraft {
  return {
    maxSessions: num(p?.max_sessions),
    maxLoad: num(p?.max_load),
    reserveCpu: num(p?.reserve_cpu),
    minFreeRamMb: num(p?.min_free_ram_mb),
    reserveRamMb: num(p?.reserve_ram_mb),
    quietHours: (p?.quiet_hours ?? []).map((w) => ({
      start: w.start,
      end: w.end,
      days: [...(w.days ?? [])],
    })),
    timeZone: p?.time_zone || browserTimeZone(),
    allowedWork: [...(p?.allowed_work ?? [])],
  };
}

/**
 * The policy to PUT, keeping fields the editor does not show (role), or the first problem
 * found before asking the hub (which validates again).
 */
export function policyFromDraft(
  d: PolicyDraft,
  keep: MachinePolicy | undefined | null,
): { policy?: MachinePolicy; error?: string } {
  const out: MachinePolicy = { ...(keep ?? {}) };
  const fields: [keyof MachinePolicy, string, string, boolean][] = [
    ['max_sessions', d.maxSessions, 'Max sessions', true],
    ['max_load', d.maxLoad, 'Max load average', false],
    ['reserve_cpu', d.reserveCpu, 'Cores kept for you', false],
    ['min_free_ram_mb', d.minFreeRamMb, 'Free RAM', true],
    ['reserve_ram_mb', d.reserveRamMb, 'Reserved RAM', true],
  ];
  for (const [key, raw, label, whole] of fields) {
    const s = raw.trim();
    if (!s) {
      delete out[key];
      continue;
    }
    const n = Number(s);
    if (!Number.isFinite(n) || n < 0 || (whole && !Number.isInteger(n)))
      return { error: `${label} must be ${whole ? 'a whole number' : 'a number'}, 0 or more` };
    if (n === 0) delete out[key];
    else (out as Record<string, unknown>)[key] = n;
  }
  const hhmm = /^([01]\d|2[0-3]):[0-5]\d$/;
  const windows = d.quietHours.filter((w) => w.start || w.end);
  for (const w of windows) {
    if (!hhmm.test(w.start) || !hhmm.test(w.end))
      return { error: 'Quiet hours need a start and an end as HH:MM' };
    if (w.start === w.end) return { error: 'A quiet window needs different start and end times' };
  }
  if (windows.length) {
    out.quiet_hours = windows.map((w) =>
      w.days.length && w.days.length < 7
        ? { start: w.start, end: w.end, days: DAYS.filter((x) => w.days.includes(x)) }
        : { start: w.start, end: w.end },
    );
    out.time_zone = d.timeZone || browserTimeZone();
  } else {
    delete out.quiet_hours;
    delete out.time_zone;
  }
  const work = WORK_CLASSES.map((c) => c.id).filter((id) => d.allowedWork.includes(id));
  if (work.length && work.length < WORK_CLASSES.length) out.allowed_work = work;
  else delete out.allowed_work;
  return { policy: out };
}

function quietText(p: MachinePolicy): string {
  if (!p.quiet_hours?.length) return 'none';
  return (
    p.quiet_hours
      .map((w) => `${w.start}–${w.end}${w.days?.length ? ' ' + w.days.join(',') : ''}`)
      .join('; ') + (p.time_zone ? ` (${p.time_zone})` : '')
  );
}

const LABELS: [string, (p: MachinePolicy) => string][] = [
  ['Role', (p) => p.role || 'dedicated'],
  ['Max sessions', (p) => (p.max_sessions ? String(p.max_sessions) : 'no limit')],
  ['Max load', (p) => (p.max_load ? String(p.max_load) : 'no limit')],
  ['Cores kept', (p) => (p.reserve_cpu ? String(p.reserve_cpu) : 'none')],
  ['Free RAM', (p) => (p.min_free_ram_mb ? `${p.min_free_ram_mb} MB` : 'none')],
  ['Reserved RAM', (p) => (p.reserve_ram_mb ? `${p.reserve_ram_mb} MB` : 'none')],
  ['Quiet hours', quietText],
  ['Allowed work', (p) => (p.allowed_work?.length ? p.allowed_work.join(', ') : 'all')],
];

/** "Max sessions: no limit → 2" for each field a machine.policy audit entry changed. */
export function policyChangeLines(metaJson: string | undefined): string[] {
  if (!metaJson) return [];
  let meta: { before?: MachinePolicy; after?: MachinePolicy };
  try {
    meta = JSON.parse(metaJson);
  } catch {
    return [];
  }
  const before = meta.before ?? {};
  const after = meta.after ?? {};
  const out: string[] = [];
  for (const [label, fmt] of LABELS) {
    const a = fmt(before);
    const b = fmt(after);
    if (a !== b) out.push(`${label}: ${a} → ${b}`);
  }
  return out;
}

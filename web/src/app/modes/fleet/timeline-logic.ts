import { Task, Worker } from '../../core/models/konsert.models';

export type TimelineRange = '12h' | '24h' | '3d' | '1w';

export const TIMELINE_RANGES: ReadonlyArray<{ id: TimelineRange; label: string; title: string; ms: number }> = [
  { id: '12h', label: '12h', title: '12 hours', ms: 12 * 3600_000 },
  { id: '24h', label: '24h', title: '24 hours', ms: 24 * 3600_000 },
  { id: '3d', label: '3d', title: '3 days', ms: 3 * 86400_000 },
  { id: '1w', label: '1w', title: 'week', ms: 7 * 86400_000 },
];

export interface TimelineSegment {
  /** Left edge and width as percentages of the range. */
  left: number;
  width: number;
  kind: 'work' | 'nodata';
  /** 0..1: concurrent sessions over the machine's capacity (work segments). */
  load: number;
  /** Concurrent sessions in this segment. */
  sessions: number;
}

export interface MachineTimeline {
  workerId: string;
  name: string;
  segments: TimelineSegment[];
  /** Milliseconds with any agent work in the range. */
  busyMs: number;
  /** Milliseconds without data (offline) at the end of the range. */
  noDataMs: number;
}

const ENDED = new Set(['completed', 'failed', 'canceled']);

/**
 * One machine's state over [start, end]: when agent sessions ran there (and how many at
 * once), and the stretch with no data since it stopped reporting. Built from the tasks
 * the hub keeps (its most recent ones), so very old ranges can be partial.
 */
export function machineTimeline(w: Worker, tasks: readonly Task[], start: number, end: number): MachineTimeline {
  const span = Math.max(1, end - start);
  const offlineFrom = w.status !== 'online' && w.last_seen_at ? Math.max(start, Date.parse(w.last_seen_at)) : end;
  // Concurrency steps from the task intervals clipped to the range (and to when data stops).
  const edges: Array<[number, number]> = [];
  for (const t of tasks) {
    if (t.worker_id !== w.id || !t.started_at) continue;
    const s = Date.parse(t.started_at);
    const e = t.completed_at ? Date.parse(t.completed_at) : ENDED.has(t.status) ? s : offlineFrom;
    const a = Math.max(s, start);
    const b = Math.min(e, offlineFrom, end);
    if (!(b > a)) continue;
    edges.push([a, 1], [b, -1]);
  }
  edges.sort((x, y) => x[0] - y[0] || x[1] - y[1]);
  const cap = Math.max(1, w.max_concurrent || 1);
  const segments: TimelineSegment[] = [];
  let depth = 0;
  let prev = start;
  let busyMs = 0;
  for (const [at, d] of edges) {
    if (at > prev && depth > 0) {
      segments.push(seg(prev, at, depth, cap, start, span));
      busyMs += at - prev;
    }
    depth += d;
    prev = Math.max(prev, at);
  }
  const merged = mergeAdjacent(segments);
  let noDataMs = 0;
  if (offlineFrom < end) {
    noDataMs = end - offlineFrom;
    merged.push({ left: ((offlineFrom - start) / span) * 100, width: (noDataMs / span) * 100, kind: 'nodata', load: 0, sessions: 0 });
  }
  return { workerId: w.id, name: w.name, segments: merged, busyMs, noDataMs };
}

function seg(a: number, b: number, depth: number, cap: number, start: number, span: number): TimelineSegment {
  return { left: ((a - start) / span) * 100, width: ((b - a) / span) * 100, kind: 'work', load: Math.min(1, depth / cap), sessions: depth };
}

function mergeAdjacent(list: TimelineSegment[]): TimelineSegment[] {
  const out: TimelineSegment[] = [];
  for (const s of list) {
    const last = out[out.length - 1];
    if (last && last.kind === s.kind && last.sessions === s.sessions && Math.abs(last.left + last.width - s.left) < 1e-9) {
      last.width += s.width;
    } else {
      out.push({ ...s });
    }
  }
  return out;
}

/** "3h 20m", "45m", "2d 4h" */
export function formatSpan(ms: number): string {
  const m = Math.round(ms / 60000);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return m % 60 ? `${h}h ${m % 60}m` : `${h}h`;
  const d = Math.floor(h / 24);
  return h % 24 ? `${d}d ${h % 24}h` : `${d}d`;
}

/** Axis labels at the start, quarters and end of the range. */
export function axisTicks(start: number, end: number, range: TimelineRange): Array<{ left: number; label: string }> {
  const out: Array<{ left: number; label: string }> = [];
  for (let i = 0; i <= 4; i++) {
    const at = start + ((end - start) * i) / 4;
    const d = new Date(at);
    const label =
      i === 4
        ? 'now'
        : range === '12h' || range === '24h'
          ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false })
          : d.toLocaleDateString([], { weekday: 'short', hour: '2-digit', hour12: false });
    out.push({ left: i * 25, label });
  }
  return out;
}

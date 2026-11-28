import { DecimalPipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, computed, effect, inject, input, signal } from '@angular/core';
import { MachineLoad, MetricSeries, ProcessLoad, SessionLoad, Worker } from '../core/models/syre.models';
import { ApiClientService } from '../core/services/api-client.service';
import { FleetStore } from '../core/state/fleet.store';

/** History ranges (GET /api/metrics). */
export const LOAD_RANGES = ['1h', '12h', '24h', '3d', '1w'] as const;
export type LoadRange = (typeof LOAD_RANGES)[number];

/** One point of a two-series chart: agent and other at time t. */
export interface LoadPoint {
  t: number;
  agent: number;
  other: number;
}

/** Chart geometry (viewBox units). */
const W = 600;
const H = 132;
const PAD = { left: 40, right: 56, top: 8, bottom: 20 };

/** Joins the agent and other series by time (a missing side reads 0). */
export function joinSeries(series: MetricSeries[], workerId: string, field: 'cpu_avg' | 'rss_avg_mb'): LoadPoint[] {
  const byT = new Map<number, LoadPoint>();
  for (const s of series) {
    const side = s.subject === `agent:${workerId}` ? 'agent' : s.subject === `other:${workerId}` ? 'other' : null;
    if (!side) continue;
    for (const p of s.points) {
      const t = new Date(p.t).getTime();
      const pt = byT.get(t) ?? { t, agent: 0, other: 0 };
      pt[side] = p[field];
      byT.set(t, pt);
    }
  }
  return [...byT.values()].sort((a, b) => a.t - b.t);
}

/** A "nice" axis maximum at or above v (1, 2, 2.5, 5 × 10^n). */
export function niceMax(v: number): number {
  if (v <= 0) return 1;
  const e = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) {
    if (m * e >= v) return m * e;
  }
  return 10 * e;
}

export function formatMB(mb: number): string {
  return mb >= 1024 ? `${(mb / 1024).toFixed(mb >= 10240 ? 0 : 1)} GB` : `${Math.round(mb)} MB`;
}

export function formatIdle(s: number | undefined, source?: string): string {
  if (s === undefined || s === null) return 'No one signed in at this machine';
  if (s < 60) return source === 'logind' ? 'In use (desktop not idle)' : 'In use right now';
  if (s < 3600) return `Idle ${Math.round(s / 60)} min`;
  if (s < 86400) return `Idle ${Math.round(s / 3600)} h`;
  return `Idle ${Math.round(s / 86400)} days`;
}

interface Chart {
  title: string;
  unit: (v: number) => string;
  points: LoadPoint[];
  max: number;
}

/**
 * Machine details → Resources (SYR-V2GGM): what is using the machine now, agent work
 * versus everything else, per session, the busiest other processes (which the user can
 * stop), and the history of agent and other load (CPU and memory, one chart each:
 * never two scales on one axis).
 */
@Component({
  selector: 'sy-machine-resources',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  styles: [
    `
      :host {
        --series-agent: #338ced; /* categorical slot 1 (dark), validated on #141518 */
        --series-other: #e76624; /* slot 2 */
        --grid: rgba(255, 255, 255, 0.07);
        --axis-text: #8a8c94;
      }
    `,
  ],
  template: `
    <section class="rounded-lg bg-[#141518] p-3 space-y-3 text-[11.5px]" data-testid="machine-resources" aria-labelledby="machine-resources-title">
      <div class="flex items-center justify-between gap-2">
        <span id="machine-resources-title" class="text-[10.5px] font-semibold uppercase tracking-wider text-[#8a8c94]">Resources</span>
        <span class="text-[10.5px] text-[#86888f]" data-testid="machine-idle">{{ idleText() }}</span>
      </div>

      @if (load(); as l) {
        <div class="grid grid-cols-2 gap-2" data-testid="machine-split">
          @for (m of splits(); track m.label) {
            <div class="space-y-1">
              <div class="text-[#a1a3ab]">{{ m.label }}</div>
              <div class="flex h-2 w-full gap-[2px] overflow-hidden rounded-full bg-white/[0.05]" role="img" [attr.aria-label]="m.aria">
                <div class="h-full rounded-l-full" [style.width.%]="m.agentPct" style="background:var(--series-agent)"></div>
                <div class="h-full rounded-r-full" [style.width.%]="m.otherPct" style="background:var(--series-other)"></div>
              </div>
              <div class="flex justify-between text-[10.5px] text-[#d4d4d8]">
                <span><span class="inline-block h-2 w-2 rounded-full align-middle" style="background:var(--series-agent)"></span> Agents {{ m.agentText }}</span>
                <span>Other {{ m.otherText }} <span class="inline-block h-2 w-2 rounded-full align-middle" style="background:var(--series-other)"></span></span>
              </div>
            </div>
          }
        </div>
        @if (l.unreported) {
          <p class="m-0 text-[10.5px] text-[#86888f]">{{ l.unreported }} processes of other users aren't counted (macOS reports them only to root).</p>
        }

        @if (sessions().length) {
          <table class="w-full text-left text-[11px]" data-testid="machine-sessions">
            <caption class="sr-only">Sessions on this machine</caption>
            <thead class="text-[10px] uppercase tracking-wider text-[#8a8c94]">
              <tr><th class="py-1 font-medium">Session</th><th class="py-1 text-right font-medium">CPU</th><th class="py-1 text-right font-medium">Memory</th><th class="py-1 text-right font-medium">Peak</th><th class="py-1 text-right font-medium">Procs</th></tr>
            </thead>
            <tbody>
              @for (s of sessions(); track sessionKey(s)) {
                <tr class="border-t border-white/5">
                  <td class="py-1 pr-2 text-[#d4d4d8] truncate max-w-[180px]">{{ sessionLabel(s) }}</td>
                  <td class="py-1 text-right font-mono-ide text-[#ececed]">{{ s.cpu_percent | number: '1.0-0' }}%</td>
                  <td class="py-1 text-right font-mono-ide text-[#ececed]">{{ mb(s.rss_mb) }}</td>
                  <td class="py-1 text-right font-mono-ide text-[#a1a3ab]">{{ mb(s.peak_rss_mb) }}</td>
                  <td class="py-1 text-right font-mono-ide text-[#a1a3ab]">{{ s.procs }}@if (s.orphans) {<span title="Kept by lineage: they outlived their parent or session"> ({{ s.orphans }} left over)</span>}</td>
                </tr>
              }
            </tbody>
          </table>
        }

        @if (topOther().length) {
          <div class="space-y-1" data-testid="machine-top-other">
            <div class="text-[10px] uppercase tracking-wider text-[#8a8c94]">Busiest other processes</div>
            <ul class="m-0 list-none space-y-1 p-0">
              @for (p of topOther(); track p.pid) {
                <li class="flex items-center gap-2">
                  <span class="min-w-0 flex-1 truncate text-[#d4d4d8]">{{ p.name }} <span class="font-mono-ide text-[#86888f]">{{ p.pid }}</span></span>
                  <span class="font-mono-ide text-[#ececed]">{{ p.cpu_percent | number: '1.0-0' }}%</span>
                  <span class="w-16 text-right font-mono-ide text-[#a1a3ab]">{{ mb(p.rss_mb) }}</span>
                  @if (confirmPid() === p.pid) {
                    <button type="button" class="sy-btn sy-btn-sm sy-btn-danger" data-testid="process-kill-confirm" [disabled]="killing()" (click)="kill(p)">Stop {{ p.name }}</button>
                    <button type="button" class="sy-btn sy-btn-sm" (click)="confirmPid.set(null)">Cancel</button>
                  } @else {
                    <button type="button" class="sy-btn sy-btn-sm" data-testid="process-kill" [attr.aria-label]="'Stop ' + p.name + ' (pid ' + p.pid + ')'" (click)="confirmPid.set(p.pid)">Stop</button>
                  }
                </li>
              }
            </ul>
            @if (killNote()) {
              <p class="m-0 text-[10.5px]" [class.text-[#f09595]]="killFailed()" [class.text-[#a1a3ab]]="!killFailed()" role="status" data-testid="process-kill-note">{{ killNote() }}</p>
            }
          </div>
        }
      } @else {
        <p class="m-0 text-[#86888f]">No load sample yet: the machine reports one with its next heartbeat.</p>
      }

      <div class="space-y-2" data-testid="machine-history">
        <div class="flex items-center justify-between gap-2">
          <div class="flex items-center gap-3 text-[10.5px] text-[#d4d4d8]" aria-label="Legend">
            <span><span class="mr-1 inline-block h-[2px] w-3 align-middle" style="background:var(--series-agent)"></span>Agents</span>
            <span><span class="mr-1 inline-block h-[2px] w-3 align-middle" style="background:var(--series-other)"></span>Other</span>
          </div>
          <div class="flex gap-1" role="group" aria-label="History range">
            @for (r of ranges; track r) {
              <button type="button" class="sy-btn sy-btn-sm" [attr.aria-pressed]="range() === r" [class.sy-btn-primary]="range() === r" [attr.data-testid]="'load-range-' + r" (click)="range.set(r)">{{ r }}</button>
            }
          </div>
        </div>
        @if (historyError()) {
          <p class="m-0 text-[#f09595]" role="alert">{{ historyError() }}</p>
        } @else if (!charts()[0].points.length) {
          <p class="m-0 text-[#86888f]" data-testid="machine-history-empty">No history for this range yet.</p>
        } @else {
          @for (c of charts(); track c.title; let ci = $index) {
            <figure class="m-0 space-y-1" [attr.data-testid]="'load-chart-' + ci">
              <figcaption class="text-[10.5px] text-[#a1a3ab]">{{ c.title }}</figcaption>
              <div class="relative">
                <svg [attr.viewBox]="'0 0 ' + w + ' ' + h" class="block w-full" role="img" [attr.aria-label]="c.title + ', agents and other, ' + range()"
                  (pointermove)="hover(ci, $event)" (pointerleave)="hoverAt.set(null)">
                  @for (g of grid(c); track g.y) {
                    <line [attr.x1]="pad.left" [attr.x2]="w - pad.right" [attr.y1]="g.y" [attr.y2]="g.y" stroke="var(--grid)" />
                    <text [attr.x]="pad.left - 6" [attr.y]="g.y + 3" text-anchor="end" font-size="9" fill="var(--axis-text)">{{ g.label }}</text>
                  }
                  @for (tk of xTicks(c); track tk.x) {
                    <text [attr.x]="tk.x" [attr.y]="h - 5" text-anchor="middle" font-size="9" fill="var(--axis-text)">{{ tk.label }}</text>
                  }
                  <path [attr.d]="path(c, 'other')" fill="none" stroke="var(--series-other)" stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />
                  <path [attr.d]="path(c, 'agent')" fill="none" stroke="var(--series-agent)" stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />
                  <!-- Direct labels at the line ends -->
                  @for (lbl of endLabels(c); track lbl.text) {
                    <text [attr.x]="w - pad.right + 6" [attr.y]="lbl.y + 3" font-size="9.5" fill="#d4d4d8">{{ lbl.text }}</text>
                  }
                  @if (hoverAt(); as hv) {
                    @if (hv.chart === ci) {
                      <line [attr.x1]="x(c, hv.i)" [attr.x2]="x(c, hv.i)" [attr.y1]="pad.top" [attr.y2]="h - pad.bottom" stroke="#d4d4d8" stroke-opacity="0.5" />
                      <circle [attr.cx]="x(c, hv.i)" [attr.cy]="y(c, c.points[hv.i].other)" r="4" fill="var(--series-other)" stroke="#141518" stroke-width="2" />
                      <circle [attr.cx]="x(c, hv.i)" [attr.cy]="y(c, c.points[hv.i].agent)" r="4" fill="var(--series-agent)" stroke="#141518" stroke-width="2" />
                    }
                  }
                </svg>
                @if (hoverAt(); as hv) {
                  @if (hv.chart === ci) {
                    <div class="pointer-events-none absolute top-0 rounded-md bg-black/80 px-2 py-1 text-[10.5px] text-[#ececed] shadow" data-testid="load-tooltip"
                      [style.left.%]="tooltipLeft(c, hv.i)">
                      <div class="text-[#a1a3ab]">{{ timeLabel(c.points[hv.i].t) }}</div>
                      <div><strong>{{ c.unit(c.points[hv.i].agent) }}</strong> agents</div>
                      <div><strong>{{ c.unit(c.points[hv.i].other) }}</strong> other</div>
                    </div>
                  }
                }
              </div>
            </figure>
          }
          <details class="text-[10.5px]">
            <summary class="cursor-pointer text-[#a1a3ab]">Show as a table</summary>
            <table class="mt-1 w-full text-left font-mono-ide" data-testid="load-table">
              <thead class="text-[#8a8c94]"><tr><th>Time</th><th class="text-right">Agent CPU</th><th class="text-right">Other CPU</th><th class="text-right">Agent mem</th><th class="text-right">Other mem</th></tr></thead>
              <tbody>
                @for (row of tableRows(); track row.t) {
                  <tr class="text-[#d4d4d8]"><td>{{ timeLabel(row.t) }}</td><td class="text-right">{{ row.agentCpu }}</td><td class="text-right">{{ row.otherCpu }}</td><td class="text-right">{{ row.agentMem }}</td><td class="text-right">{{ row.otherMem }}</td></tr>
                }
              </tbody>
            </table>
          </details>
        }
      </div>
    </section>
  `,
  imports: [DecimalPipe],
})
export class MachineResourcesComponent {
  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);

  readonly worker = input.required<Worker>();
  readonly ranges = LOAD_RANGES;
  readonly range = signal<LoadRange>('12h');
  readonly series = signal<MetricSeries[]>([]);
  readonly historyError = signal('');
  readonly hoverAt = signal<{ chart: number; i: number } | null>(null);
  readonly confirmPid = signal<number | null>(null);
  readonly killing = signal(false);
  readonly killNote = signal('');
  readonly killFailed = signal(false);

  readonly w = W;
  readonly h = H;
  readonly pad = PAD;

  readonly load = computed<MachineLoad | undefined>(() => this.worker().metrics?.load);
  readonly sessions = computed(() => [...(this.load()?.sessions ?? [])].sort((a, b) => b.cpu_percent - a.cpu_percent));
  readonly topOther = computed(() => this.load()?.top_other ?? []);
  readonly idleText = computed(() => formatIdle(this.load()?.user_idle_s, this.load()?.user_idle_source));

  readonly splits = computed(() => {
    const l = this.load();
    if (!l) return [];
    const cores = Math.max(1, this.worker().facts?.cpu_cores ?? 1);
    const totalCPU = cores * 100;
    const totalMem = Math.max(1, this.worker().metrics?.mem_total_mb ?? l.agent_rss_mb + l.other_rss_mb);
    const pct = (v: number, total: number) => Math.min(100, (100 * v) / total);
    return [
      {
        label: `CPU (${cores} cores)`,
        agentPct: pct(l.agent_cpu_percent, totalCPU),
        otherPct: pct(l.other_cpu_percent, totalCPU),
        agentText: `${(l.agent_cpu_percent / 100).toFixed(1)} cores`,
        otherText: `${(l.other_cpu_percent / 100).toFixed(1)} cores`,
        aria: `CPU: agents ${(l.agent_cpu_percent / 100).toFixed(1)} cores, other ${(l.other_cpu_percent / 100).toFixed(1)} of ${cores}`,
      },
      {
        label: `Memory (${formatMB(totalMem)})`,
        agentPct: pct(l.agent_rss_mb, totalMem),
        otherPct: pct(l.other_rss_mb, totalMem),
        agentText: formatMB(l.agent_rss_mb),
        otherText: formatMB(l.other_rss_mb),
        aria: `Memory: agents ${formatMB(l.agent_rss_mb)}, other ${formatMB(l.other_rss_mb)} of ${formatMB(totalMem)}`,
      },
    ];
  });

  readonly charts = computed<Chart[]>(() => {
    const id = this.worker().id;
    const cpu = joinSeries(this.series(), id, 'cpu_avg');
    const mem = joinSeries(this.series(), id, 'rss_avg_mb');
    return [
      { title: 'CPU (100% = one core)', unit: (v) => `${Math.round(v)}%`, points: cpu, max: niceMax(Math.max(0, ...cpu.flatMap((p) => [p.agent, p.other]))) },
      { title: 'Memory', unit: (v) => formatMB(v), points: mem, max: niceMax(Math.max(0, ...mem.flatMap((p) => [p.agent, p.other]))) },
    ];
  });

  readonly tableRows = computed(() => {
    const [cpu, mem] = this.charts();
    const memByT = new Map(mem.points.map((p) => [p.t, p]));
    return cpu.points.map((p) => ({
      t: p.t,
      agentCpu: `${Math.round(p.agent)}%`,
      otherCpu: `${Math.round(p.other)}%`,
      agentMem: formatMB(memByT.get(p.t)?.agent ?? 0),
      otherMem: formatMB(memByT.get(p.t)?.other ?? 0),
    }));
  });

  constructor() {
    effect(() => {
      const id = this.worker().id;
      const r = this.range();
      void this.loadHistory(id, r);
    });
  }

  private async loadHistory(id: string, r: LoadRange): Promise<void> {
    try {
      const res = await this.api.getMetrics([`agent:${id}`, `other:${id}`], r);
      if (this.worker().id === id && this.range() === r) {
        this.series.set(res.series ?? []);
        this.historyError.set('');
      }
    } catch (err) {
      this.historyError.set(err instanceof Error ? err.message : 'Could not load the history');
    }
  }

  mb(v: number): string {
    return formatMB(v);
  }

  sessionKey(s: SessionLoad): string {
    return s.task_id ?? s.chat_id ?? s.external_id ?? '';
  }

  sessionLabel(s: SessionLoad): string {
    if (s.task_id) {
      const t = this.fleet.tasksById()[s.task_id];
      return t?.title || t?.ticket_title || t?.sid || s.task_id;
    }
    if (s.chat_id) return `Chat ${s.chat_id.slice(0, 8)}`;
    return `Observed ${s.external_id?.slice(0, 8) ?? ''}`;
  }

  x(c: Chart, i: number): number {
    const n = c.points.length;
    if (n <= 1) return PAD.left;
    const t0 = c.points[0].t;
    const span = c.points[n - 1].t - t0 || 1;
    return PAD.left + ((c.points[i].t - t0) / span) * (W - PAD.left - PAD.right);
  }

  y(c: Chart, v: number): number {
    return H - PAD.bottom - (v / c.max) * (H - PAD.top - PAD.bottom);
  }

  path(c: Chart, side: 'agent' | 'other'): string {
    return c.points.map((p, i) => `${i ? 'L' : 'M'}${this.x(c, i).toFixed(1)},${this.y(c, p[side]).toFixed(1)}`).join('');
  }

  grid(c: Chart): { y: number; label: string }[] {
    return [0, 0.5, 1].map((f) => ({ y: this.y(c, f * c.max), label: c.unit(f * c.max) }));
  }

  xTicks(c: Chart): { x: number; label: string }[] {
    const n = c.points.length;
    if (!n) return [];
    const idx = n === 1 ? [0] : [0, Math.floor((n - 1) / 2), n - 1];
    return idx.map((i) => ({ x: this.x(c, i), label: this.timeLabel(c.points[i].t, true) }));
  }

  endLabels(c: Chart): { y: number; text: string }[] {
    const last = c.points[c.points.length - 1];
    if (!last) return [];
    let ya = this.y(c, last.agent);
    let yo = this.y(c, last.other);
    if (Math.abs(ya - yo) < 11) {
      // Keep the two labels apart: the higher value goes on top.
      const mid = (ya + yo) / 2;
      [ya, yo] = last.agent >= last.other ? [mid - 6, mid + 6] : [mid + 6, mid - 6];
    }
    return [
      { y: ya, text: 'Agents' },
      { y: yo, text: 'Other' },
    ];
  }

  hover(chart: number, e: PointerEvent): void {
    const c = this.charts()[chart];
    const svg = e.currentTarget as SVGSVGElement;
    const rect = svg.getBoundingClientRect();
    if (!c.points.length || !rect.width) return;
    const vx = ((e.clientX - rect.left) / rect.width) * W;
    let best = 0;
    for (let i = 1; i < c.points.length; i++) {
      if (Math.abs(this.x(c, i) - vx) < Math.abs(this.x(c, best) - vx)) best = i;
    }
    this.hoverAt.set({ chart, i: best });
  }

  /** The tooltip sits beside the crosshair (right of it, or left near the right edge),
   * never over the points it describes. */
  tooltipLeft(c: Chart, i: number): number {
    const xp = (this.x(c, i) / W) * 100;
    return xp < 62 ? xp + 2 : xp - 26;
  }

  timeLabel(t: number, short = false): string {
    const d = new Date(t);
    const long = this.range() === '3d' || this.range() === '1w';
    if (short) {
      return long ? d.toLocaleDateString(undefined, { weekday: 'short', hour: '2-digit' }) : d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
    }
    return d.toLocaleString(undefined, { weekday: 'short', hour: '2-digit', minute: '2-digit' });
  }

  async kill(p: ProcessLoad): Promise<void> {
    this.killing.set(true);
    try {
      const res = await this.api.killProcess(this.worker().id, p.pid, p.started_at);
      this.killFailed.set(!res.killed);
      this.killNote.set(res.killed ? `${p.name} (pid ${p.pid}) ${res.forced ? 'was killed: it ignored the request to stop' : 'stopped'}.` : res.error || 'Not stopped');
    } catch (err) {
      this.killFailed.set(true);
      this.killNote.set(err instanceof Error ? err.message : 'Not stopped');
    } finally {
      this.killing.set(false);
      this.confirmPid.set(null);
    }
  }
}

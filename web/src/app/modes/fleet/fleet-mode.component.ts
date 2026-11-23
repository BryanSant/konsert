import { ChangeDetectionStrategy, Component, DestroyRef, computed, inject, signal } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { ActivatedRoute, Router } from '@angular/router';
import { map } from 'rxjs';
import { Worker } from '../../core/models/konsert.models';
import { ControlStore } from '../../core/state/control.store';
import { NeedsYouStore } from '../../core/state/needs-you.store';
import { NeedsYouItemComponent } from '../../features/needs-you/needs-you-item.component';
import { FleetStore } from '../../core/state/fleet.store';
import { occupiesSlot } from '../../core/state/work-logic';
import { IconComponent } from '../../shared/icon.component';
import { TooltipDirective } from '../../shared/tooltip.directive';
import { ModeUiStore } from '../mode-ui.store';
import { TIMELINE_RANGES, TimelineRange, axisTicks, formatSpan, machineTimeline } from './timeline-logic';


/** A machine's state badge (grey baseline; amber only for FULL). */
export function machineBadge(w: Worker, running: number): 'OFFLINE' | 'STARTING' | 'DRAINING' | 'FULL' | 'EMPTY' | 'OK' {
  if (w.status !== 'online') return w.launch_state === 'starting' ? 'STARTING' : 'OFFLINE';
  if (w.drained) return 'DRAINING';
  if (w.max_concurrent && running >= w.max_concurrent) return 'FULL';
  if (running === 0) return 'EMPTY';
  return 'OK';
}

/**
 * 3 Fleet, first cut: the machines and the state timeline with its range picker (12 h
 * default, 24 h, 3 days, 1 week). The machine-first "inside-out" layout waits for design
 * sign-off.
 */
@Component({
  selector: 'ks-fleet-mode',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective, NeedsYouItemComponent],
  host: { class: 'block h-full overflow-y-auto' },
  template: `
    <div class="px-4 py-3 flex flex-col gap-4 max-w-[1400px]">
      @if (needsYou.count() > 0) {
        <section aria-labelledby="ks-fleet-needs-you" data-testid="fleet-needs-you">
          <div class="flex items-center gap-2 mb-2">
            <h2 id="ks-fleet-needs-you" class="m-0 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--ks-amber)]">Needs you · {{ needsYou.count() }}</h2>
            @if (needsYou.count() > 3) {
              <button type="button" class="ks-btn ks-btn-sm text-[var(--ks-blue)]" (click)="needsYou.open()">Go through all</button>
            }
          </div>
          <ul class="m-0 p-0 list-none grid gap-1.5 md:grid-cols-3">
            @for (e of needsYou.entries().slice(0, 3); track e.key) {
              <li><ks-needs-you-item [entry]="e" /></li>
            }
          </ul>
        </section>
      }
      <section aria-labelledby="ks-fleet-machines" data-testid="fleet-machines">
        <div class="flex flex-wrap items-center gap-2 mb-2">
          <h2 id="ks-fleet-machines" class="m-0 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--ks-text-secondary)]">Machines · {{ rows().length }}</h2>
          <span class="flex-1"></span>
          <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" data-testid="fleet-add-machine" (click)="ui.addMachineOpen.set(true)">
            <app-icon name="plus" [size]="12" />
            Add machine
          </button>
        </div>
        @if (rows().length === 0) {
          <p class="text-[12.5px] text-[var(--ks-text-muted)]">No machines yet. Add this machine or one over SSH to start.</p>
        } @else {
          <div class="overflow-x-auto rounded-[var(--ks-radius-lg)] bg-[var(--ks-surface-1)]">
            <table class="ks-fleet-table w-full text-[12.5px]">
              <caption class="ks-sr-only">Machines</caption>
              <thead>
                <tr>
                  <th scope="col">Machine</th>
                  <th scope="col">State</th>
                  <th scope="col">Sessions</th>
                  <th scope="col">CPU</th>
                  <th scope="col">Memory</th>
                  <th scope="col">System</th>
                  <th scope="col"><span class="ks-sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                @for (r of rows(); track r.w.id) {
                  <tr [class.bg-[var(--ks-blue-soft)]]="selectedSid() === r.w.sid" [attr.data-machine]="r.w.sid" [class.opacity-70]="r.w.status !== 'online'">
                    <th scope="row" class="font-medium text-[var(--ks-text)]">
                      <button type="button" class="ks-focusable rounded-sm text-left" [attr.aria-current]="selectedSid() === r.w.sid ? 'true' : null" (click)="select(r.w)">
                        {{ r.w.name }}
                        <span class="ml-1 font-mono-ide text-[10.5px] font-normal text-[var(--ks-text-muted)]">{{ r.w.sid }}</span>
                      </button>
                    </th>
                    <td><span class="ks-badge" [attr.data-badge]="r.badge">{{ r.badge }}</span></td>
                    <td class="font-mono-ide">{{ r.running }} / {{ r.w.max_concurrent || '∞' }}</td>
                    <td>
                      <span class="ks-bar" role="img" [attr.aria-label]="'CPU ' + r.cpu + '%'"><span [style.width.%]="r.cpu"></span></span>
                      <span class="ml-1.5 font-mono-ide text-[11px] text-[var(--ks-text-muted)]">{{ r.w.metrics ? r.cpu + '%' : '—' }}</span>
                    </td>
                    <td>
                      <span class="ks-bar" role="img" [attr.aria-label]="'Memory ' + r.mem + '%'"><span [style.width.%]="r.mem"></span></span>
                      <span class="ml-1.5 font-mono-ide text-[11px] text-[var(--ks-text-muted)]">{{ r.w.metrics ? r.mem + '%' : '—' }}</span>
                    </td>
                    <td class="text-[var(--ks-text-secondary)]">{{ r.system }}</td>
                    <td class="text-right whitespace-nowrap">
                      @if (r.w.status === 'online') {
                        <button type="button" class="ks-btn ks-btn-sm" [disabled]="control.isBusy(r.w.id)" (click)="control.setDrained(r.w.id, !r.w.drained)" [ksTooltip]="r.w.drained ? 'Accept new work again' : 'Finish what runs, accept no new work'">
                          {{ r.w.drained ? 'Undrain' : 'Drain' }}
                        </button>
                      }
                      <button type="button" class="ks-btn ks-btn-sm" [attr.data-testid]="'machine-details-' + r.w.sid" (click)="ui.inspectedWorkerId.set(r.w.id)">Details</button>
                    </td>
                  </tr>
                }
              </tbody>
            </table>
          </div>
        }
      </section>

      <section class="rounded-[var(--ks-radius-lg)] bg-[var(--ks-surface-1)] px-4 py-3" aria-labelledby="ks-fleet-timeline" data-testid="fleet-timeline">
        <div class="flex flex-wrap items-center gap-1.5 mb-3">
          <h2 id="ks-fleet-timeline" class="m-0 mr-2 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--ks-text-secondary)]">Last {{ rangeTitle() }}</h2>
          <div class="inline-flex gap-1" role="radiogroup" aria-label="Time range">
            @for (r of ranges; track r.id) {
              <button
                type="button"
                role="radio"
                class="ks-btn ks-btn-sm ks-btn-soft"
                [attr.aria-checked]="range() === r.id"
                [class.!bg-[var(--ks-blue-soft)]]="range() === r.id"
                [class.!text-[#cfe2ff]]="range() === r.id"
                [attr.data-range]="r.id"
                (click)="setRange(r.id)"
              >
                {{ r.label }}
              </button>
            }
          </div>
        </div>
        @if (timelines().length === 0) {
          <p class="text-[12.5px] text-[var(--ks-text-muted)]">The timeline fills in once machines run work.</p>
        }
        @for (tl of timelines(); track tl.workerId) {
          <div class="grid grid-cols-[110px_minmax(0,1fr)] items-center gap-3 mb-1.5" [attr.data-timeline]="tl.name">
            <span class="truncate text-[12px] text-[var(--ks-text-secondary)]">{{ tl.name }}</span>
            <span class="relative block h-3 rounded-[3px] bg-white/[0.04] overflow-hidden" role="img" [attr.aria-label]="describe(tl)">
              @for (s of tl.segments; track $index) {
                <span
                  class="absolute top-0 bottom-0"
                  [class.ks-nodata]="s.kind === 'nodata'"
                  [style.left.%]="s.left"
                  [style.width.%]="s.width"
                  [style.background]="s.kind === 'work' ? workColor(s.load) : null"
                ></span>
              }
            </span>
          </div>
        }
        @if (timelines().length) {
          <div class="grid grid-cols-[110px_minmax(0,1fr)] gap-3" aria-hidden="true">
            <span></span>
            <span class="relative block h-4 text-[10.5px] text-[var(--ks-text-muted)] font-mono-ide">
              @for (t of ticks(); track $index) {
                <span class="absolute -translate-x-1/2 whitespace-nowrap" [class.!translate-x-[-100%]]="$last" [class.!translate-x-0]="$first" [style.left.%]="t.left">{{ t.label }}</span>
              }
            </span>
          </div>
          <div class="flex flex-wrap gap-4 mt-2 text-[11.5px] text-[var(--ks-text-muted)]">
            <span><span class="inline-block w-2.5 h-2 mr-1.5 align-middle" style="background: #8a8d96"></span>agent work (darker: fewer sessions than its capacity)</span>
            <span><span class="inline-block w-2.5 h-2 mr-1.5 align-middle ks-nodata"></span>no data (offline)</span>
            <span>From the hub's most recent sessions; other load needs per-machine history (coming).</span>
          </div>
        }
      </section>
    </div>
  `,
  styles: `
    .ks-fleet-table th,
    .ks-fleet-table td {
      padding: 8px 10px;
      text-align: left;
      border-bottom: 1px solid var(--ks-hairline);
      font-weight: inherit;
    }
    .ks-fleet-table thead th {
      font-size: 11px;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--ks-text-muted);
    }
    .ks-fleet-table tbody tr:last-child th,
    .ks-fleet-table tbody tr:last-child td {
      border-bottom: 0;
    }
    .ks-badge {
      display: inline-flex;
      padding: 1px 7px;
      border-radius: 4px;
      font-family: 'JetBrains Mono', ui-monospace, monospace;
      font-size: 10.5px;
      color: var(--ks-text-muted);
      background: var(--ks-raised);
    }
    .ks-badge[data-badge='FULL'] {
      color: #1b1406;
      background: var(--ks-amber);
    }
    .ks-badge[data-badge='DRAINING'] {
      color: #9ec5ff;
      background: transparent;
      box-shadow: inset 0 0 0 1px var(--ks-blue);
    }
    .ks-badge[data-badge='OFFLINE'],
    .ks-badge[data-badge='STARTING'] {
      background: repeating-linear-gradient(135deg, rgba(255, 255, 255, 0.08) 0 4px, transparent 4px 8px);
    }
    .ks-badge[data-badge='EMPTY'] {
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.12);
    }
    .ks-bar {
      display: inline-block;
      vertical-align: middle;
      width: 72px;
      height: 6px;
      border-radius: 3px;
      background: rgba(255, 255, 255, 0.07);
      overflow: hidden;
    }
    .ks-bar > span {
      display: block;
      height: 100%;
      background: #8a8d96;
    }
    .ks-nodata {
      background: repeating-linear-gradient(135deg, rgba(255, 255, 255, 0.25) 0 3px, transparent 3px 6px);
    }
  `,
})
export class FleetModeComponent {
  readonly ui = inject(ModeUiStore);
  readonly control = inject(ControlStore);
  readonly needsYou = inject(NeedsYouStore);
  private readonly fleet = inject(FleetStore);
  private readonly router = inject(Router);
  readonly ranges = TIMELINE_RANGES;

  readonly selectedSid = toSignal(inject(ActivatedRoute).paramMap.pipe(map((p) => p.get('sid') ?? '')), { initialValue: '' });
  readonly range = signal<TimelineRange>('12h');
  readonly rangeTitle = computed(() => TIMELINE_RANGES.find((r) => r.id === this.range())!.title);
  /** Re-renders the timeline every minute so "now" moves. */
  private readonly now = signal(Date.now());

  readonly rows = computed(() => {
    const running = new Map<string, number>();
    for (const t of this.fleet.tasks()) {
      if (occupiesSlot(t)) running.set(t.worker_id, (running.get(t.worker_id) ?? 0) + 1);
    }
    return this.fleet.workers().map((w) => {
      const n = running.get(w.id) ?? 0;
      const m = w.metrics;
      return {
        w,
        running: n,
        badge: machineBadge(w, n),
        cpu: m ? Math.round(Math.min(100, m.cpu_percent)) : 0,
        mem: m && m.mem_total_mb ? Math.round((m.mem_used_mb / m.mem_total_mb) * 100) : 0,
        system: [w.os, w.arch, w.facts?.cpu_cores ? `${w.facts.cpu_cores}c` : '', w.facts?.ram_total_mb ? `${Math.round(w.facts.ram_total_mb / 1024)}G` : '']
          .filter(Boolean)
          .join(' · '),
      };
    });
  });

  private readonly window = computed(() => {
    const end = this.now();
    const ms = TIMELINE_RANGES.find((r) => r.id === this.range())!.ms;
    return { start: end - ms, end };
  });

  readonly timelines = computed(() => {
    const { start, end } = this.window();
    const tasks = this.fleet.tasks();
    return this.fleet.workers().map((w) => machineTimeline(w, tasks, start, end));
  });

  readonly ticks = computed(() => {
    const { start, end } = this.window();
    return axisTicks(start, end, this.range());
  });

  constructor() {
    const t = setInterval(() => this.now.set(Date.now()), 60_000);
    inject(DestroyRef).onDestroy(() => clearInterval(t));
  }

  select(w: Worker): void {
    void this.router.navigate(this.selectedSid() === w.sid ? ['/fleet'] : ['/fleet', w.sid]);
  }

  setRange(r: TimelineRange): void {
    this.range.set(r);
  }

  workColor(load: number): string {
    // Grey baseline: busier is lighter, never a status colour.
    const a = 0.45 + 0.55 * load;
    return `rgba(138, 141, 150, ${a.toFixed(2)})`;
  }

  describe(tl: { name: string; busyMs: number; noDataMs: number }): string {
    const parts = [`${tl.name}: agent work for ${formatSpan(tl.busyMs)} in the last ${this.rangeTitle()}`];
    if (tl.noDataMs) parts.push(`no data for the last ${formatSpan(tl.noDataMs)}`);
    return parts.join('; ');
  }
}

import { ChangeDetectionStrategy, Component, DestroyRef, ElementRef, afterRenderEffect, computed, effect, inject, signal, untracked } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { ActivatedRoute, Router } from '@angular/router';
import { map } from 'rxjs';
import { HeatHistory } from '../../core/models/activity.models';
import { Worker } from '../../core/models/konsert.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { isVersionMismatch } from '../../core/models/version';
import { taskTitle } from '../../core/state/activity-logic';
import { ControlStore } from '../../core/state/control.store';
import { FleetStore } from '../../core/state/fleet.store';
import { WorkStore } from '../../core/state/work.store';
import { ActivitySpinnerComponent, HeatAvatarComponent } from '../../shared/activity-spinner.component';
import { IconComponent } from '../../shared/icon.component';
import { SpinnerLegendComponent } from '../../shared/spinner-legend.component';
import { SPINNER_LABEL_COLORS, SPINNER_WORDS, heatTextColor, spinnerMachine } from '../../shared/spinner-logic';
import { TooltipDirective } from '../../shared/tooltip.directive';
import { ModeUiStore } from '../mode-ui.store';
import { HEAT_RANGES, HEAT_STRIP_POINTS, HeatRange, MachineRow, MachineSession, describeStrip, heatStrip, machineRows, queueReasons, stripTicks } from './fleet-logic';

const WAKE_REASON = 'Keep Awake from the Konsert dashboard';

/**
 * 3 Fleet, machines first (signed-off design): each row is a machine (heat avatar, name,
 * spec, state badge, heat word and cause, Drain / Suspend / Details) with the sessions
 * running on it as cards (the same spinner, the agent's mark in the centre since the row is
 * the machine), its load and free slots, and its heat strip over the chosen range (12 h
 * default, 24 h, 3 days, 1 week; from the hub's heat history). Below: the queue and why
 * each task is waiting.
 */
@Component({
  selector: 'ks-fleet-mode',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective, ActivitySpinnerComponent, HeatAvatarComponent, SpinnerLegendComponent],
  host: { class: 'block h-full overflow-y-auto' },
  template: `
    <div class="px-4 py-3 flex flex-col gap-2.5 max-w-[1500px]">
      <div class="flex flex-wrap items-center gap-2.5 text-[12.5px]">
        <h2 id="ks-fleet-machines" class="m-0 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--ks-text-secondary)]">Machines · {{ rows().length }}</h2>
        <span class="hidden lg:inline text-[var(--ks-text-secondary)]">Each row is a machine, with the project work running on it.</span>
        <span class="flex-1"></span>
        <span class="text-[var(--ks-text-secondary)]" id="ks-heat-range-label">Heat over the last</span>
        <div class="inline-flex gap-1" role="radiogroup" aria-labelledby="ks-heat-range-label">
          @for (r of ranges; track r.id) {
            <button
              type="button"
              role="radio"
              class="ks-btn ks-btn-sm ks-btn-soft"
              [attr.aria-checked]="range() === r.id"
              [class.!bg-[var(--ks-blue-soft)]]="range() === r.id"
              [class.!text-[#cfe2ff]]="range() === r.id"
              [attr.data-range]="r.id"
              (click)="range.set(r.id)"
            >
              {{ r.label }}
            </button>
          }
        </div>
        <ks-spinner-legend class="ml-1" />
        @if (wakeCapable()) {
          <button
            type="button"
            class="ks-btn ks-btn-sm ks-btn-soft ml-1"
            data-testid="keep-awake-all"
            [attr.aria-pressed]="allAwake()"
            [class.text-[#e6b763]]="allAwake()"
            (click)="setWakeAll(!allAwake())"
            ksTooltip="Keep every machine awake (caffeinate on macOS, systemd-inhibit on Linux, the Windows execution state), including machines that connect later"
          >
            <app-icon name="coffee" [size]="12" />
            {{ allAwake() ? 'All awake' : 'Keep all awake' }}
          </button>
        }
        <button type="button" class="ks-btn ks-btn-sm ks-btn-soft ml-1" data-testid="fleet-add-machine" (click)="ui.addMachineOpen.set(true)">
          <app-icon name="plus" [size]="12" />
          Add machine
        </button>
      </div>

      @if (wakeError()) {
        <p role="alert" class="m-0 rounded-[var(--ks-radius-md)] bg-[var(--ks-red-soft)] px-3 py-2 text-[12px] text-[#f3a5a5]">{{ wakeError() }}</p>
      }
      @if (rows().length === 0) {
        <p class="text-[12.5px] text-[var(--ks-text-muted)]" data-testid="fleet-machines">No machines yet. Add this machine or one over SSH to start.</p>
      } @else {
        <div class="flex flex-col gap-2.5" role="list" aria-labelledby="ks-fleet-machines" data-testid="fleet-machines">
          @for (r of rows(); track r.w.id) {
            <section
              role="listitem"
              class="ks-machine"
              [class.ks-machine-selected]="selectedSid() === r.w.sid"
              [class.ks-machine-off]="r.w.status !== 'online'"
              [attr.aria-labelledby]="'ks-m-' + r.w.id"
              [attr.data-machine]="r.w.sid"
            >
              <div class="flex gap-3 items-start min-w-0">
                <ks-heat-avatar [machine]="machineOf(r)" [size]="44" shape="square" />
                <div class="min-w-0 flex flex-col gap-[3px]">
                  <div class="flex items-center gap-2 min-w-0">
                    <h3 class="m-0 text-[14px] font-semibold truncate" [id]="'ks-m-' + r.w.id">
                      <button type="button" class="ks-focusable rounded-sm text-left" [attr.aria-current]="selectedSid() === r.w.sid ? 'true' : null" (click)="select(r.w)">{{ r.w.name }}</button>
                    </h3>
                    <span class="ks-badge" [attr.data-badge]="r.badge">{{ r.badge }}</span>
                    @if (versionMismatch(r.w); as note) {
                      <span class="ks-badge" data-badge="VERSION" role="img" data-testid="version-mismatch-badge" [attr.aria-label]="note" [ksTooltip]="note">VERSION</span>
                    }
                  </div>
                  <div class="text-[12px] text-[var(--ks-text-secondary)] truncate">{{ r.spec }}</div>
                  <div class="text-[12px]" [style.color]="heatColor(r)" data-testid="machine-heat">{{ r.heatText }}</div>
                  <div class="flex flex-wrap gap-1 mt-1">
                    @if (r.w.status === 'online') {
                      <button type="button" class="ks-btn ks-btn-sm" [disabled]="control.isBusy(r.w.id)" (click)="control.setDrained(r.w.id, !r.w.drained)" [ksTooltip]="r.w.drained ? 'Accept new work again' : 'Finish what runs, accept no new work'">
                        {{ r.w.drained ? 'Undrain' : 'Drain' }}
                      </button>
                      @if (r.suspended && !r.running) {
                        <button type="button" class="ks-btn ks-btn-sm" [disabled]="control.isBusy(r.w.id)" (click)="control.workerBulk(r.w.id, 'resume-all')" ksTooltip="Resume every suspended session on this machine">Resume</button>
                      } @else {
                        <button type="button" class="ks-btn ks-btn-sm" [disabled]="control.isBusy(r.w.id) || !r.running" (click)="control.workerBulk(r.w.id, 'suspend-all')" ksTooltip="Freeze every session on this machine (nothing is lost)">Suspend</button>
                      }
                    }
                    @if (r.w.status === 'online' && r.w.wake_lock?.supported) {
                      <button
                        type="button"
                        class="ks-btn ks-btn-sm"
                        [attr.aria-pressed]="wakeOn(r.w)"
                        [attr.data-testid]="'keep-awake-' + r.w.sid"
                        [class.text-[#e6b763]]="wakeOn(r.w)"
                        (click)="setWake(r.w, !wakeOn(r.w))"
                        [ksTooltip]="wakeTip(r.w)"
                      >
                        <app-icon name="coffee" [size]="11" />
                        {{ wakeOn(r.w) ? (r.w.wake_lock?.active ? 'Awake' : 'Waking…') : 'Keep awake' }}
                      </button>
                    }
                    @if (r.w.launch_state === 'failed') {
                      <button type="button" class="ks-btn ks-btn-sm" (click)="fleet.retryStart(r.w.id)" [ksTooltip]="'Start konsertd on ' + r.w.name + ' again'">Retry start</button>
                    }
                    <button type="button" class="ks-btn ks-btn-sm" [attr.data-testid]="'machine-details-' + r.w.sid" (click)="ui.inspectedWorkerId.set(r.w.id)">Details</button>
                  </div>
                </div>
              </div>

              <div class="flex flex-col gap-2 min-w-0">
                <ul class="m-0 p-0 list-none flex flex-wrap gap-2" [attr.aria-label]="'Sessions on ' + r.w.name">
                  @for (s of r.sessions; track s.key) {
                    <li class="ks-sess-li" [class.opacity-70]="s.done">
                      <button type="button" class="ks-focusable ks-sess" [attr.data-session]="s.sid" [attr.data-state]="s.state" [disabled]="!s.item" (click)="open(s)">
                        <ks-spinner [state]="s.state" [machines]="[machineOf(r)]" [glyph]="true" [agent]="s.agent" [coarse]="s.coarse" [tps]="s.tps" />
                        <span class="min-w-0 flex-1">
                          <span class="flex items-center gap-1.5 text-[11px] text-[var(--ks-text-secondary)] min-w-0">
                            <span class="w-[7px] h-[7px] rounded-[2px] shrink-0" [style.background]="s.projectColor" aria-hidden="true"></span>
                            <span class="truncate">{{ s.project }} · {{ s.agent }}</span>
                          </span>
                          <span class="block truncate text-[12.5px] text-[var(--ks-text)]">{{ s.title }}</span>
                          <span class="block truncate text-[11.5px]" [style.color]="colors[s.state]" [ksTooltip]="s.nowTitle" data-testid="fleet-now"><span class="ks-sr-only">{{ words[s.state] }}: </span>{{ s.now }}</span>
                        </span>
                      </button>
                    </li>
                  }
                  @if (r.next) {
                    <li class="ks-extra ks-extra-next">next: {{ r.next }} (queued)</li>
                  }
                  @if (r.load) {
                    <li class="ks-extra" [attr.aria-label]="'Machine load: ' + r.load" ksTooltip="The whole machine: agents and everything else running on it">machine · {{ r.load }}</li>
                  }
                  @for (f of slots(r.free); track $index) {
                    <li class="ks-extra ks-extra-free">free slot</li>
                  }
                  @if (r.w.status !== 'online') {
                    <li class="ks-extra">{{ offlineText(r.w) }}</li>
                  } @else if (!r.sessions.length && !r.free && !r.load) {
                    <li class="ks-extra">nothing running</li>
                  }
                </ul>
                <div class="flex items-center gap-2.5">
                  <span class="ks-strip" role="img" [attr.aria-label]="strips()[r.w.id]?.label" data-testid="heat-strip">
                    @for (c of strips()[r.w.id]?.cells; track $index) {
                      <span [class.ks-nodata]="!c.color" [style.background]="c.color"></span>
                    }
                  </span>
                  <span class="font-mono-ide text-[10.5px] text-[var(--ks-text-muted)] w-[110px] shrink-0">heat · last {{ range() }}</span>
                </div>
              </div>
            </section>
          }
          <div class="hidden min-[901px]:grid grid-cols-[minmax(0,250px)_minmax(0,1fr)] gap-x-[18px] px-[14px]" aria-hidden="true">
            <span></span>
            <span class="flex items-center gap-2.5">
              <span class="relative block flex-1 h-4 text-[10.5px] text-[var(--ks-text-muted)] font-mono-ide">
                @for (t of ticks(); track $index) {
                  <span class="absolute whitespace-nowrap" [class.-translate-x-1/2]="!$first && !$last" [class.-translate-x-full]="$last" [style.left.%]="t.left">{{ t.label }}</span>
                }
              </span>
              <span class="w-[110px] shrink-0"></span>
            </span>
          </div>
        </div>
      }

      <section class="rounded-[var(--ks-radius-lg)] bg-[var(--ks-surface-1)] px-[14px] py-3" aria-labelledby="ks-fleet-queue" data-testid="fleet-queue">
        <h2 id="ks-fleet-queue" class="m-0 mb-2 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--ks-text-secondary)]">Queue · why it's waiting</h2>
        @if (queue().length === 0) {
          <p class="m-0 text-[12.5px] text-[var(--ks-text-muted)]">Nothing is waiting for a machine.</p>
        } @else {
          <ul class="m-0 p-0 list-none flex flex-col gap-1.5">
            @for (q of queue(); track q.id) {
              <li class="flex flex-wrap items-center gap-x-3 gap-y-1 text-[12.5px]" [attr.data-queued]="q.sid">
                <span class="font-mono-ide text-[var(--ks-text-muted)]">{{ q.sid }}</span>
                <span class="text-[var(--ks-text)]">{{ q.title }}</span>
                @for (why of q.reasons; track $index) {
                  @if (!$first) {
                    <span class="text-[var(--ks-text-muted)]" aria-hidden="true">·</span>
                  }
                  <span [class.text-[var(--ks-amber)]]="why.tone === 'warn'" [class.text-[var(--ks-text-secondary)]]="why.tone === 'muted'">{{ why.text }}</span>
                }
                <span class="flex-1"></span>
                <button type="button" class="ks-btn ks-btn-sm" (click)="ui.openInCommand(q.sid)">Open</button>
              </li>
            }
          </ul>
        }
      </section>
    </div>
  `,
  styles: `
    :host {
      --ks-spinner-hole: #1c1d21;
    }
    .ks-machine {
      display: grid;
      grid-template-columns: minmax(0, 250px) minmax(0, 1fr);
      gap: 8px 18px;
      padding: 12px 14px;
      border-radius: var(--ks-radius-lg);
      background: var(--ks-surface-1);
    }
    @media (max-width: 900px) {
      .ks-machine {
        grid-template-columns: minmax(0, 1fr);
      }
    }
    .ks-machine-selected {
      box-shadow: inset 0 0 0 1px var(--ks-blue);
    }
    .ks-machine-off {
      opacity: 0.75;
    }
    .ks-sess-li {
      display: flex;
      flex: 1 1 230px;
      min-width: 230px;
      max-width: 320px;
    }
    .ks-sess {
      display: flex;
      flex: 1;
      min-width: 0;
      gap: 10px;
      align-items: center;
      padding: 8px 12px 8px 10px;
      border: 0;
      border-radius: 10px;
      background: #1c1d21;
      color: var(--ks-text);
      text-align: left;
      cursor: pointer;
    }
    .ks-sess:hover:not(:disabled) {
      background: #222328;
    }
    .ks-sess:disabled {
      cursor: default;
    }
    .ks-extra {
      display: flex;
      align-items: center;
      padding: 8px 12px;
      border-radius: 10px;
      font-size: 12px;
      color: var(--ks-text-secondary);
      background: rgba(255, 255, 255, 0.04);
    }
    .ks-extra-free {
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.12);
    }
    .ks-extra-next {
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(224, 168, 76, 0.4);
      color: #e9c27f;
    }
    .ks-strip {
      display: flex;
      flex: 1;
      gap: 1px;
      height: 10px;
      border-radius: 3px;
      overflow: hidden;
    }
    .ks-strip > span {
      flex: 1;
    }
    .ks-nodata {
      background: repeating-linear-gradient(135deg, rgba(255, 255, 255, 0.18) 0 3px, transparent 3px 6px);
    }
    .ks-badge {
      display: inline-flex;
      padding: 1px 6px;
      border-radius: 4px;
      font-family: 'JetBrains Mono', ui-monospace, monospace;
      font-size: 10.5px;
      color: var(--ks-text-muted);
      background: rgba(255, 255, 255, 0.05);
      white-space: nowrap;
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
    .ks-badge[data-badge='VERSION'] {
      color: #e9c27f;
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(224, 168, 76, 0.5);
    }
    .ks-badge[data-badge='IDLE'] {
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.12);
    }
  `,
})
export class FleetModeComponent {
  readonly ui = inject(ModeUiStore);
  readonly control = inject(ControlStore);
  readonly fleet = inject(FleetStore);
  private readonly work = inject(WorkStore);
  private readonly api = inject(ApiClientService);
  private readonly router = inject(Router);
  private readonly host = inject<ElementRef<HTMLElement>>(ElementRef);
  readonly ranges = HEAT_RANGES;
  readonly colors = SPINNER_LABEL_COLORS;
  readonly words = SPINNER_WORDS;

  readonly selectedSid = toSignal(inject(ActivatedRoute).paramMap.pipe(map((p) => p.get('sid') ?? '')), { initialValue: '' });
  readonly range = signal<HeatRange>('12h');
  private readonly rangeTitle = computed(() => HEAT_RANGES.find((r) => r.id === this.range())!.title);
  private readonly now = signal(Date.now());
  readonly history = signal<HeatHistory | null>(null);
  readonly wakeError = signal('');
  readonly wakeCapable = computed(() => this.fleet.workers().some((w) => w.status === 'online' && w.wake_lock?.supported));
  readonly allAwake = computed(() => {
    const able = this.fleet.workers().filter((w) => w.status === 'online' && w.wake_lock?.supported);
    return able.length > 0 && able.every((w) => this.wakeOn(w));
  });

  readonly rows = computed(() =>
    machineRows({
      workers: this.fleet.workers(),
      tasks: this.fleet.tasks(),
      groups: this.work.projectGroups(),
      items: this.work.byId(),
      allItems: this.work.items(),
      heat: this.fleet.heatByWorker(),
      now: this.now(),
    }),
  );

  readonly strips = computed(() => {
    const h = this.history();
    const out: Record<string, { cells: ReturnType<typeof heatStrip>; label: string }> = {};
    for (const w of this.fleet.workers()) {
      const cells = heatStrip(h, w.id);
      out[w.id] = { cells, label: describeStrip(w.name, cells, this.rangeTitle()) };
    }
    return out;
  });

  readonly ticks = computed(() => {
    const h = this.history();
    const end = h ? Date.parse(h.end) : this.now();
    const start = h ? Date.parse(h.start) : end - HEAT_RANGES.find((r) => r.id === this.range())!.ms;
    return stripTicks(start, end, this.range());
  });

  readonly queue = computed(() => queueReasons(this.fleet.tasks(), this.fleet.workers(), (t) => taskTitle(t).text));

  constructor() {
    // Heat history: on range change and every minute.
    effect(() => {
      const r = this.range();
      this.now();
      untracked(() => void this.loadHistory(r));
    });
    const t = setInterval(() => this.now.set(Date.now()), 60_000);
    inject(DestroyRef).onDestroy(() => clearInterval(t));
    // /fleet/<sid> scrolls that machine into view.
    afterRenderEffect(() => {
      const sid = this.selectedSid();
      if (!sid) return;
      this.host.nativeElement.querySelector(`[data-machine="${CSS.escape(sid)}"]`)?.scrollIntoView({ block: 'nearest' });
    });
  }

  private async loadHistory(r: HeatRange): Promise<void> {
    try {
      const h = await this.api.getHeatHistory(r, HEAT_STRIP_POINTS);
      if (this.range() === r) this.history.set(h);
    } catch {
      // No history (older hub, offline): the strip shows "no data".
      if (this.range() === r) this.history.set(null);
    }
  }

  versionMismatch(w: Worker): string {
    const hub = this.fleet.controllerVersion();
    return isVersionMismatch(hub, w.version) ? `Version mismatch: ${w.name} runs konsertd ${w.version}, hub is ${hub}` : '';
  }

  wakeOn(w: Worker): boolean {
    return w.wake_lock?.desired ?? !!w.wake_lock?.active;
  }

  wakeTip(w: Worker): string {
    const wl = w.wake_lock;
    if (wl?.active) return `${w.name} is kept awake${wl.method ? ` (${wl.method})` : ''}; press to let it sleep`;
    if (this.wakeOn(w)) return wl?.error ? `Couldn't keep ${w.name} awake: ${wl.error}` : `Asking ${w.name} to stay awake…`;
    return `Keep ${w.name} from sleeping while you're away`;
  }

  async setWake(w: Worker, on: boolean): Promise<void> {
    this.wakeError.set('');
    try {
      await this.api.setWorkerWakeLock(w.id, on, WAKE_REASON);
    } catch (e) {
      this.wakeError.set(`Could not update ${w.name}: ${e instanceof Error ? e.message : String(e)}`);
    }
  }

  async setWakeAll(on: boolean): Promise<void> {
    this.wakeError.set('');
    try {
      await this.api.setFleetWakeLock(on, WAKE_REASON);
    } catch (e) {
      this.wakeError.set(`Could not ${on ? 'enable' : 'disable'} Keep awake: ${e instanceof Error ? e.message : String(e)}`);
    }
  }

  machineOf(r: MachineRow) {
    return spinnerMachine(r.w.name, r.heat);
  }

  heatColor(r: MachineRow): string {
    return heatTextColor(r.heat ? r.heat.score : null);
  }

  slots(n: number): number[] {
    return Array.from({ length: Math.min(n, 6) });
  }

  offlineText(w: Worker): string {
    if (w.launch_state === 'starting') return 'starting konsertd…';
    if (w.launch_state === 'failed') return `could not start: ${w.launch_error || 'see Details'}`;
    return 'offline: not reporting';
  }

  select(w: Worker): void {
    void this.router.navigate(this.selectedSid() === w.sid ? ['/fleet'] : ['/fleet', w.sid]);
  }

  open(s: MachineSession): void {
    if (s.item) this.ui.openInCommand(s.item);
  }
}

import { Component, OnDestroy, computed, inject, output, signal } from '@angular/core';
import { Worker } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { AuthService } from '../core/services/auth.service';
import { KonsertSocketService } from '../core/services/konsert-socket.service';
import { FleetStore } from '../core/state/fleet.store';
import { TabsStore } from '../core/state/tabs.store';
import { IconComponent } from '../shared/icon.component';
import { FooterUsageComponent } from './footer-usage.component';
import { TooltipDirective } from '../shared/tooltip.directive';

type FooterPopover = 'resources' | 'ports' | 'hosts' | 'wake' | null;

/** Footer view of one online host's Keep Awake state. */
export interface HostWakeRow {
  worker: Worker;
  supported: boolean;
  /** What the hub wants (falls back to the reported state). */
  on: boolean;
  active: boolean;
  /** Wanted but not (yet) held: starting, or failed with an error. */
  pending: boolean;
  label: string;
  error?: string;
}

const HOST_WAKE_REASON = 'Keep Awake from the Konsert dashboard';

@Component({
  selector: 'app-status-footer',
  standalone: true,
  imports: [TooltipDirective, IconComponent, FooterUsageComponent],
  template: `
    <footer (keydown.escape)="openPopover.set(null)" class="relative h-8 flex items-center justify-between px-3.5 bg-[var(--ks-bg-chrome)] text-[11.5px] text-[#8e9098] select-none shrink-0 z-30">
      <!-- Backdrop to close open popover -->
      @if (openPopover()) {
        <div class="fixed inset-0 z-20" (click)="openPopover.set(null)"></div>
      }

      <!-- Left: Connection + Usage Summary (Orca style) -->
      <div class="relative z-30 flex items-center gap-3 min-w-0">
        <span class="inline-flex items-center gap-1.5 shrink-0">
          <span
            class="h-1.5 w-1.5 rounded-full"
            [class.bg-[#69b879]]="socket.state() === 'connected'"
            [class.bg-[#d9a043]]="socket.state() === 'connecting'"
            [class.bg-[#e05d5d]]="socket.state() === 'disconnected'"
          ></span>
          <span class="hidden sm:inline text-[#a1a3ab]">{{ primaryHostLabel() }}</span>
        </span>

        <ks-footer-usage />
      </div>

      <!-- Right: Keep Awake, Resources, Ports, Hosts, Commands (Orca + Herdr) -->
      <div class="relative z-30 flex items-center gap-1.5 shrink-0">
        <!-- Keep Awake: browser screen wake lock + host wake locks (caffeinate / systemd-inhibit) -->
        <span class="inline-flex items-center rounded-md" [class.bg-[#24262e]]="openPopover() === 'wake'">
          <button
            type="button"
            data-testid="keep-awake-toggle"
            (click)="toggleWakeLock()"
            [attr.aria-pressed]="anyAwake()"
            [attr.aria-label]="wakeAriaLabel()"
            [ksTooltip]="wakeAriaLabel()"
            class="inline-flex items-center gap-1 rounded-l-md px-2 py-1 hover:bg-white/[0.06] transition cursor-pointer"
            [class.text-[#e6b763]]="anyAwake()"
            [class.text-[#8e9098]]="!anyAwake()"
          >
            <app-icon name="coffee" [size]="13" />
            <span class="hidden sm:inline" data-testid="keep-awake-label">{{ wakeLabel() }}</span>
            <span
              class="h-1.5 w-1.5 rounded-full"
              [class.bg-[#69b879]]="wakeIndicator() === 'on'"
              [class.bg-[#d9a043]]="wakeIndicator() === 'partial'"
              [class.bg-[#5e6068]]="wakeIndicator() === 'off'"
            ></span>
          </button>
          <button
            type="button"
            data-testid="keep-awake-details"
            (click)="togglePopover('wake')"
            aria-label="Keep Awake details per host"
            ksTooltip="Keep Awake details per host"
            aria-haspopup="dialog"
            [attr.aria-expanded]="openPopover() === 'wake'"
            class="inline-flex items-center justify-center rounded-r-md w-[24px] h-[24px] text-[#8e9098] hover:bg-white/[0.06] hover:text-[#ececed] transition cursor-pointer"
          >
            <svg aria-hidden="true" width="9" height="9" viewBox="0 0 10 10" fill="none" stroke="currentColor" stroke-width="1.5">
              <path d="M2 6.5 5 3.5l3 3" />
            </svg>
          </button>
        </span>

        <!-- Resource Manager Trigger -->
        <button
          type="button"
          (click)="togglePopover('resources')"
          data-testid="footer-resources"
          ksTooltip="Fleet Resource Manager & PTYs"
          class="inline-flex items-center gap-1 rounded-md px-2 py-1 hover:bg-white/[0.06] hover:text-[#ececed] transition cursor-pointer"
          [class.bg-[#24262e]]="openPopover() === 'resources'"
          [class.text-[#ececed]]="openPopover() === 'resources'"
        >
          <app-icon name="activity" [size]="13" />
          <span class="font-mono-ide">{{ fleet.resourceTotals().activePtys }}</span>
        </button>

        <!-- Listening Ports Trigger -->
        <button
          type="button"
          (click)="togglePopover('ports')"
          data-testid="footer-ports"
          ksTooltip="Listening ports across fleet"
          class="inline-flex items-center gap-1 rounded-md px-2 py-1 hover:bg-white/[0.06] hover:text-[#ececed] transition cursor-pointer"
          [class.bg-[#24262e]]="openPopover() === 'ports'"
          [class.text-[#ececed]]="openPopover() === 'ports'"
        >
          <app-icon name="ports" [size]="13" />
          <span class="font-mono-ide">{{ fleet.listeningPorts().length }}</span>
        </button>

        <!-- Hosts Summary Trigger -->
        <button
          type="button"
          (click)="togglePopover('hosts')"
          data-testid="footer-hosts"
          ksTooltip="Connected fleet hosts"
          class="inline-flex items-center gap-1 rounded-md px-2 py-1 hover:bg-white/[0.06] hover:text-[#ececed] transition cursor-pointer"
          [class.bg-[#24262e]]="openPopover() === 'hosts'"
          [class.text-[#ececed]]="openPopover() === 'hosts'"
        >
          <app-icon name="server" [size]="13" />
          <span>{{ fleet.onlineWorkersCount() }} hosts</span>
        </button>

        <!-- Command Palette Shortcut (Herdr style) -->
        <button
          type="button"
          (click)="openCommands.emit()"
          ksTooltip="Command Palette (⌘K)"
          class="hidden md:inline-flex items-center gap-1 rounded-md px-2 py-1 hover:bg-white/[0.06] hover:text-[#ececed] text-[#8a8c94] transition cursor-pointer ml-1"
        >
          <span class="font-mono-ide text-[10px]">⌘K</span>
          <span>commands</span>
        </button>
      </div>

      <!-- POPOVER: KEEP AWAKE (browser + hosts) -->
      @if (openPopover() === 'wake') {
        <div
          role="dialog"
          aria-label="Keep Awake"
          data-testid="keep-awake-popover"
          class="ks-popover absolute bottom-9 right-40 z-30 w-80 max-w-[92vw] rounded-xl overflow-hidden"
        >
          <div class="flex items-center justify-between px-3 py-2 border-b border-white/[0.06]">
            <span class="text-[12px] font-semibold text-[#ececed]">Keep Awake</span>
            <button
              type="button"
              (click)="toggleWakeLock()"
              class="text-[10.5px] text-[#8e9098] hover:text-[#ececed] cursor-pointer"
            >
              {{ anyAwake() ? 'Turn all off' : 'Turn all on' }}
            </button>
          </div>
          <div class="p-2 space-y-1 text-[11px] max-h-80 overflow-y-auto">
            <label class="flex items-center justify-between gap-2 rounded-lg px-2 py-1.5 hover:bg-[#1c1d22] cursor-pointer">
              <span class="min-w-0">
                <span class="block text-[#e4e4e7] font-medium">This browser</span>
                <span class="block text-[10px] text-[#86888f]">
                  {{ keepAwake() ? (browserLockHeld() ? 'Screen kept on' : 'On (screen wake lock unavailable here)') : 'Off' }}
                </span>
              </span>
              <input
                type="checkbox"
                role="switch"
                data-testid="browser-wake-switch"
                class="accent-[#e6b763] h-3.5 w-3.5 cursor-pointer"
                [checked]="keepAwake()"
                (change)="setBrowserWakeLock(!keepAwake())"
                aria-label="Keep this browser's screen awake"
              />
            </label>

            <div class="flex items-center justify-between px-2 pt-2 pb-0.5">
              <span class="text-[10px] font-semibold uppercase tracking-wider text-[#86888f]">Hosts</span>
              <span class="font-mono-ide text-[10px] text-[#86888f]">{{ hostsAwake() }}/{{ hostWakeRows().length }} awake</span>
            </div>
            @if (hostWakeRows().length === 0) {
              <div class="rounded-lg bg-[#121316] px-3 py-2.5 text-[#86888f]">No online hosts.</div>
            }
            @for (row of hostWakeRows(); track row.worker.id) {
              <label
                class="flex items-center justify-between gap-2 rounded-lg px-2 py-1.5"
                [class.hover:bg-[#1c1d22]]="row.supported"
                [class.cursor-pointer]="row.supported"
                [attr.data-testid]="'host-wake-' + row.worker.id"
              >
                <span class="min-w-0">
                  <span class="flex items-center gap-1.5 text-[#e4e4e7] font-medium">
                    <span
                      class="h-1.5 w-1.5 rounded-full shrink-0"
                      [class.bg-[#69b879]]="row.active"
                      [class.bg-[#d9a043]]="row.pending"
                      [class.bg-[#5e6068]]="!row.active && !row.pending"
                    ></span>
                    <span class="truncate">{{ row.worker.name }}</span>
                  </span>
                  <span
                    class="block text-[10px] truncate"
                    [class.text-[#e05d5d]]="!!row.error"
                    [class.text-[#86888f]]="!row.error"
                    [ksTooltip]="row.error || row.label"
                  >
                    {{ row.error || row.label }}
                  </span>
                </span>
                <input
                  type="checkbox"
                  role="switch"
                  class="accent-[#e6b763] h-3.5 w-3.5 cursor-pointer disabled:cursor-not-allowed disabled:opacity-40"
                  [checked]="row.on"
                  [disabled]="!row.supported"
                  (change)="setHostWakeLock(row.worker, !row.on)"
                  [attr.aria-label]="'Keep ' + row.worker.name + ' awake'"
                />
              </label>
            }
            @if (wakeError()) {
              <div role="alert" class="rounded-lg bg-[#2a1717] px-3 py-1.5 text-[#f0a0a0]">{{ wakeError() }}</div>
            }
            <p class="px-2 pt-1 text-[10px] leading-snug text-[#6e7078]">
              Hosts stay awake via caffeinate (macOS), systemd-inhibit (Linux) or the Windows execution state, and are
              released if the hub stays unreachable for a few minutes.
            </p>
          </div>
        </div>
      }

      <!-- POPOVER: RESOURCE MANAGER (Orca Image 1 style) -->
      @if (openPopover() === 'resources') {
        <div class="ks-popover absolute bottom-9 right-3 z-30 w-96 max-w-[94vw] rounded-xl overflow-hidden">
          <div class="flex items-center justify-between px-3.5 py-2.5 border-b border-white/[0.06]">
            <div class="flex items-center gap-2 text-[12px] font-semibold text-[#ececed]">
              <app-icon name="activity" [size]="13" />
              <span>Resource Manager</span>
            </div>
            <button
              type="button"
              (click)="fleet.refresh()"
              class="text-[11px] text-[#8e9098] hover:text-[#ececed] cursor-pointer"
            >
              Refresh
            </button>
          </div>

          <!-- Resources | PTYs Sub-tabs -->
          <div class="flex items-center gap-3 px-3.5 pt-2 border-b border-white/[0.06] text-[12px]">
            <button
              type="button"
              (click)="resourceTab.set('resources')"
              class="pb-2 border-b-2 transition cursor-pointer"
              [class.border-[#ececed]]="resourceTab() === 'resources'"
              [class.text-[#ececed]]="resourceTab() === 'resources'"
              [class.border-transparent]="resourceTab() !== 'resources'"
              [class.text-[#8a8c94]]="resourceTab() !== 'resources'"
            >
              Resources
            </button>
            <button
              type="button"
              (click)="resourceTab.set('ptys')"
              class="pb-2 border-b-2 inline-flex items-center gap-1.5 transition cursor-pointer"
              [class.border-[#ececed]]="resourceTab() === 'ptys'"
              [class.text-[#ececed]]="resourceTab() === 'ptys'"
              [class.border-transparent]="resourceTab() !== 'ptys'"
              [class.text-[#8a8c94]]="resourceTab() !== 'ptys'"
            >
              <span>PTYs</span>
              <span class="rounded-full bg-[#26282e] px-1.5 text-[10px] font-mono-ide">{{ fleet.resourceTotals().activePtys }}</span>
            </button>
          </div>

          @if (resourceTab() === 'resources') {
            <div class="p-3.5 space-y-3">
              <!-- Memory Summary Bar -->
              <div class="space-y-1.5">
                <div class="flex items-center justify-between text-[11px]">
                  <span class="text-[#d4d4d8]">
                    Fleet Memory:
                    <strong class="font-mono-ide">{{ formatMB(fleet.resourceTotals().memUsedMB) }}</strong>
                    / {{ formatMB(fleet.resourceTotals().memTotalMB) }}
                  </span>
                  <span class="font-mono-ide text-[#8e9098]">CPU ~{{ fleet.resourceTotals().avgCpuPercent }}%</span>
                </div>
                <div class="h-1.5 w-full rounded-full bg-[#24262c] overflow-hidden">
                  <div
                    class="h-full bg-[#60a5fa] rounded-full transition-all"
                    [style.width.%]="fleet.resourceTotals().memPercent"
                  ></div>
                </div>
              </div>

              <!-- Per-Worker Table -->
              <div class="space-y-1 max-h-56 overflow-y-auto">
                <div class="grid grid-cols-12 text-[10px] text-[#86888f] px-1.5 pb-1">
                  <span class="col-span-6">Host</span>
                  <span class="col-span-3 text-right">CPU</span>
                  <span class="col-span-3 text-right">Mem</span>
                </div>
                @for (w of fleet.workers(); track w.id) {
                  <div class="grid grid-cols-12 items-center rounded-md px-1.5 py-1.5 hover:bg-[#1f2127] text-[11px]">
                    <div class="col-span-6 flex items-center gap-1.5 min-w-0">
                      <span class="h-1.5 w-1.5 rounded-full shrink-0" [class.bg-[#69b879]]="w.status === 'online'" [class.bg-[#484a52]]="w.status !== 'online'"></span>
                      <span class="truncate text-[#e4e4e7]">{{ w.name }}</span>
                    </div>
                    <span class="col-span-3 text-right font-mono-ide text-[#a1a3ab]">
                      {{ w.metrics ? (w.metrics.cpu_percent.toFixed(0) + '%') : '—' }}
                    </span>
                    <span class="col-span-3 text-right font-mono-ide text-[#d4d4d8]">
                      {{ w.metrics && w.metrics.mem_used_mb ? formatMB(w.metrics.mem_used_mb) : '—' }}
                    </span>
                  </div>
                }
              </div>
            </div>
          } @else {
            <div class="p-3 space-y-1.5 max-h-64 overflow-y-auto">
              @if (activePtyTasks().length === 0) {
                <div class="py-6 text-center text-[12px] text-[#86888f]">No active PTY sessions running</div>
              }
              @for (t of activePtyTasks(); track t.id) {
                <div class="flex items-center justify-between gap-2 rounded-lg bg-[#1b1c20] px-3 py-2.5">
                  <button
                    type="button"
                    (click)="focusTask(t.id)"
                    class="flex items-center gap-2 min-w-0 flex-1 text-left cursor-pointer"
                  >
                    <app-icon [name]="t.agent" [size]="12" />
                    <div class="min-w-0">
                      <div class="text-[12px] text-[#ececed] truncate">{{ t.agent }} · {{ t.workdir }}</div>
                      <div class="text-[10px] font-mono-ide text-[#8a8c94]">{{ fleet.workersById()[t.worker_id]?.name || fleet.workersById()[t.worker_id]?.sid || t.worker_id }} · {{ t.status }}</div>
                    </div>
                  </button>
                  <button
                    type="button"
                    (click)="stopTask(t.id)"
                    class="ks-btn ks-btn-sm ks-btn-danger"
                  >
                    Stop
                  </button>
                </div>
              }
            </div>
          }
        </div>
      }

      <!-- POPOVER: LISTENING PORTS -->
      @if (openPopover() === 'ports') {
        <div class="ks-popover absolute bottom-9 right-16 z-30 w-80 max-w-[92vw] rounded-xl overflow-hidden">
          <div class="flex items-center justify-between px-3 py-2 border-b border-white/[0.06]">
            <span class="text-[12px] font-semibold text-[#ececed]">Listening Fleet Ports</span>
            <span class="font-mono-ide text-[10px] text-[#8a8c94]">{{ fleet.listeningPorts().length }} open</span>
          </div>
          <div class="p-2 max-h-60 overflow-y-auto space-y-1">
            @if (fleet.listeningPorts().length === 0) {
              <div class="py-5 text-center text-[11px] text-[#86888f]">No listening dev ports detected yet.</div>
            }
            @for (p of fleet.listeningPorts(); track p.workerId + ':' + p.port) {
              <div class="flex items-center justify-between rounded-md px-2 py-1.5 hover:bg-[#1f2127] font-mono-ide text-[11px]">
                <div class="flex items-center gap-2 min-w-0">
                  <span class="text-[#69b879] font-semibold">:{{ p.port }}</span>
                  <span class="text-[#d4d4d8] truncate">{{ p.process }}</span>
                </div>
                <span class="text-[10px] text-[#8a8c94] shrink-0">{{ p.workerName }}</span>
              </div>
            }
          </div>
        </div>
      }

      <!-- POPOVER: HOSTS -->
      @if (openPopover() === 'hosts') {
        <div class="ks-popover absolute bottom-9 right-3 z-30 w-80 max-w-[92vw] rounded-xl overflow-hidden">
          <div class="flex items-center justify-between px-3 py-2 border-b border-white/[0.06]">
            <span class="text-[12px] font-semibold text-[#ececed]">Fleet Hosts</span>
            <span class="font-mono-ide text-[10px] text-[#8a8c94]">{{ fleet.onlineWorkersCount() }}/{{ fleet.workers().length }} online</span>
          </div>
          <div class="p-2 max-h-64 overflow-y-auto space-y-1.5">
            @for (w of fleet.workers(); track w.id) {
              <button
                type="button"
                (click)="inspectHost(w)"
                class="w-full text-left rounded-lg bg-[#1a1b1f] hover:bg-[#202229] px-3 py-2.5 space-y-1 transition cursor-pointer"
              >
                <div class="flex items-center justify-between gap-2">
                  <span class="inline-flex items-center gap-1.5 text-[12px] font-medium text-[#ececed] truncate">
                    <span class="h-1.5 w-1.5 rounded-full shrink-0" [class.bg-[#69b879]]="w.status === 'online'" [class.bg-[#484a52]]="w.status !== 'online'"></span>
                    <span class="truncate">{{ w.name }}</span>
                  </span>
                  <span class="font-mono-ide text-[10px] text-[#8a8c94] shrink-0">{{ w.os }}/{{ w.arch }} · Details</span>
                </div>
                <div class="flex flex-wrap items-center gap-1">
                  <span class="rounded bg-[#22242a] px-1.5 py-0.5 text-[9px] font-mono-ide" [class.text-[#69b879]]="w.permission_mode === 'full_access'" [class.text-[#d9a043]]="w.permission_mode === 'sandboxed'">
                    {{ w.permission_mode }}
                  </span>
                  @for (a of w.agents; track a) {
                    <span class="rounded bg-[#22242a] px-1.5 py-0.5 text-[9px] font-mono-ide text-[#b4b6be]">{{ a }}</span>
                  }
                </div>
              </button>
            }
          </div>
        </div>
      }
    </footer>
  `,
})
export class StatusFooterComponent implements OnDestroy {
  readonly openCommands = output<void>();
  readonly openMachineDetails = output<Worker>();

  readonly fleet = inject(FleetStore);
  readonly socket = inject(KonsertSocketService);
  readonly auth = inject(AuthService);
  private readonly tabs = inject(TabsStore);
  private readonly api = inject(ApiClientService);

  readonly openPopover = signal<FooterPopover>(null);
  readonly resourceTab = signal<'resources' | 'ptys'>('resources');
  /** Browser screen wake lock wanted (held when the Screen Wake Lock API allows it). */
  readonly keepAwake = signal(false);
  readonly browserLockHeld = signal(false);
  readonly wakeError = signal('');

  private wakeLock: { release(): Promise<void> } | null = null;

  /** Online hosts with their Keep Awake state, by name. */
  readonly hostWakeRows = computed<HostWakeRow[]>(() =>
    this.fleet
      .workers()
      .filter((w) => w.status === 'online')
      .sort((a, b) => a.name.localeCompare(b.name))
      .map((w) => hostWakeRow(w)),
  );
  readonly hostsAwake = computed(() => this.hostWakeRows().filter((r) => r.active).length);
  private readonly hostsWanted = computed(() => this.hostWakeRows().filter((r) => r.on).length);

  /** Anything (browser or a host) is kept awake or asked to be. */
  readonly anyAwake = computed(() => this.keepAwake() || this.hostsAwake() > 0 || this.hostsWanted() > 0);

  /** Green when everything asked for is held, amber while a host is pending or failed. */
  readonly wakeIndicator = computed<'on' | 'partial' | 'off'>(() => {
    if (!this.anyAwake()) return 'off';
    return this.hostWakeRows().some((r) => r.pending) ? 'partial' : 'on';
  });

  readonly wakeLabel = computed(() => {
    const total = this.hostWakeRows().length;
    const awake = this.hostsAwake();
    if (!this.anyAwake()) return 'Off';
    if (total === 0) return 'Awake';
    if (!this.keepAwake()) return `Hosts ${awake}/${total}`;
    return `Awake · ${awake}/${total} hosts`;
  });

  readonly wakeAriaLabel = computed(() => {
    const total = this.hostWakeRows().length;
    const browser = this.keepAwake() ? 'browser on' : 'browser off';
    const hosts = total === 0 ? 'no online hosts' : `${this.hostsAwake()} of ${total} hosts awake`;
    const action = this.anyAwake() ? 'activate to turn all off' : 'activate to keep this browser and all hosts awake';
    return `Keep Awake: ${browser}, ${hosts}; ${action}`;
  });

  readonly primaryHostLabel = computed(() => {
    const tab = this.tabs.activeTab();
    if (tab?.kind === 'terminal' && tab.workerId) {
      return this.fleet.workersById()[tab.workerId]?.name || 'Fleet';
    }
    const online = this.fleet.workers().find((w) => w.status === 'online');
    return online ? online.name : 'Konsert Hub';
  });

  readonly activePtyTasks = computed(() =>
    this.fleet.tasks().filter((t) => t.status === 'running' || t.status === 'waiting_input' || t.status === 'assigned')
  );

  togglePopover(name: FooterPopover): void {
    this.openPopover.update((cur) => (cur === name ? null : name));
  }

  inspectHost(w: Worker): void {
    this.openPopover.set(null);
    this.openMachineDetails.emit(w);
  }

  /** Main toggle: everything off if anything is awake, else browser plus all hosts on. */
  async toggleWakeLock(): Promise<void> {
    const enable = !this.anyAwake();
    this.wakeError.set('');
    await Promise.all([this.setBrowserWakeLock(enable), this.setAllHostsWakeLock(enable)]);
  }

  async setBrowserWakeLock(enable: boolean): Promise<void> {
    if (!enable) {
      this.keepAwake.set(false);
      await this.releaseWakeLock();
      return;
    }
    this.keepAwake.set(true);
    try {
      const nav = navigator as Navigator & { wakeLock?: { request(type: string): Promise<{ release(): Promise<void> }> } };
      if (nav.wakeLock && !this.wakeLock) {
        this.wakeLock = await nav.wakeLock.request('screen');
        this.browserLockHeld.set(true);
      }
    } catch {
      // Not allowed here (e.g. insecure origin or hidden tab): the hosts still get theirs.
      this.browserLockHeld.set(false);
    }
  }

  async setAllHostsWakeLock(enable: boolean): Promise<void> {
    try {
      await this.api.setFleetWakeLock(enable, HOST_WAKE_REASON);
    } catch (e) {
      this.wakeError.set(`Could not ${enable ? 'enable' : 'disable'} host Keep Awake: ${errorMessage(e)}`);
    }
  }

  async setHostWakeLock(w: Worker, enable: boolean): Promise<void> {
    this.wakeError.set('');
    try {
      await this.api.setWorkerWakeLock(w.id, enable, HOST_WAKE_REASON);
    } catch (e) {
      this.wakeError.set(`Could not update ${w.name}: ${errorMessage(e)}`);
    }
  }

  private async releaseWakeLock(): Promise<void> {
    const lock = this.wakeLock;
    this.wakeLock = null;
    this.browserLockHeld.set(false);
    if (lock) {
      try {
        await lock.release();
      } catch {
        // ignore
      }
    }
  }

  focusTask(taskId: string): void {
    const task = this.fleet.tasksById()[taskId];
    if (!task) return;
    this.tabs.openTaskTab(task, this.fleet.workersById()[task.worker_id]?.name);
    this.openPopover.set(null);
  }

  async stopTask(taskId: string): Promise<void> {
    try {
      await this.api.cancelTask(taskId);
    } catch {
      // ignore
    }
  }

  formatMB(mb: number): string {
    if (!mb) return '0 MB';
    if (mb >= 1024) return `${(mb / 1024).toFixed(1)} GB`;
    return `${mb} MB`;
  }

  formatNum(n: number): string {
    return n.toLocaleString();
  }

  formatPct(v: number): string {
    return `${Math.round(Math.max(0, Math.min(100, v)))}%`;
  }

  formatReset(iso: string): string {
    const diffMs = new Date(iso).getTime() - Date.now();
    if (isNaN(diffMs) || diffMs <= 0) return 'resets soon';
    const mins = Math.round(diffMs / 60000);
    if (mins < 60) return `resets in ${mins}m`;
    const hrs = Math.floor(mins / 60);
    const remM = mins % 60;
    return remM > 0 ? `resets in ${hrs}h ${remM}m` : `resets in ${hrs}h`;
  }

  ngOnDestroy(): void {
    void this.releaseWakeLock();
  }
}

function hostWakeRow(w: Worker): HostWakeRow {
  const wl = w.wake_lock;
  const supported = !!wl?.supported;
  const active = !!wl?.active;
  const on = wl?.desired ?? active;
  const pending = supported && on && !active;
  let label: string;
  if (!supported) label = 'Not supported by this konsertd';
  else if (active) label = wl?.method ? `Awake · ${wl.method}` : 'Awake';
  else if (pending) label = 'Starting…';
  else label = 'Off';
  return { worker: w, supported, on, active, pending, label, error: pending ? wl?.error || undefined : undefined };
}

function errorMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

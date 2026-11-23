import { Component, OnDestroy, computed, inject, output, signal } from '@angular/core';
import { Worker } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { AuthService } from '../core/services/auth.service';
import { KonsertSocketService } from '../core/services/konsert-socket.service';
import { FleetStore } from '../core/state/fleet.store';
import { TabsStore } from '../core/state/tabs.store';
import { IconComponent } from '../shared/icon.component';

type FooterPopover = 'usage' | 'resources' | 'ports' | 'hosts' | null;

@Component({
  selector: 'app-status-footer',
  standalone: true,
  imports: [IconComponent],
  template: `
    <footer class="relative h-7 flex items-center justify-between px-3 border-t border-[#26282e] bg-[#16171a] text-[11px] text-[#8e9098] select-none shrink-0 z-30">
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

        <button
          type="button"
          (click)="togglePopover('usage')"
          title="Subscription quotas & API token usage"
          class="inline-flex items-center gap-2 rounded px-1.5 py-0.5 hover:bg-[#22242a] hover:text-[#ececed] transition cursor-pointer min-w-0 overflow-hidden"
        >
          <span class="text-[#686a72] shrink-0">Used</span>
          @if (subscriptionQuotas().length > 0) {
            <span class="inline-flex items-center gap-2.5 truncate">
              @for (q of subscriptionQuotas(); track q.provider + ':' + (q.window_label || '')) {
                <span class="inline-flex items-center gap-1 shrink-0">
                  <app-icon [name]="q.provider" [size]="11" />
                  <span class="text-[#b4b6be]">{{ q.label }}</span>
                  @if (q.window_label) {
                    <span class="font-mono-ide text-[#787a82]">{{ q.window_label }}</span>
                  }
                  <span
                    class="font-mono-ide"
                    [class.text-[#e05d5d]]="q.used_percent >= 90"
                    [class.text-[#d9a043]]="q.used_percent >= 70 && q.used_percent < 90"
                    [class.text-[#d4d4d8]]="q.used_percent < 70"
                  >
                    {{ formatPct(q.used_percent) }}
                  </span>
                </span>
              }
            </span>
          } @else {
            <span class="font-mono-ide text-[#b4b6be]">{{ formattedTotalTokens() }}</span>
            @if (auth.provider()) {
              <span class="hidden md:inline text-[#686a72]">· {{ auth.provider() }}</span>
            }
          }
        </button>
      </div>

      <!-- Right: Keep Awake, Resources, Ports, Hosts, Commands (Orca + Herdr) -->
      <div class="relative z-30 flex items-center gap-1 shrink-0">
        <!-- Keep Awake (Built-in Caffeine) -->
        <button
          type="button"
          (click)="toggleWakeLock()"
          title="Keep display awake while monitoring fleet"
          class="inline-flex items-center gap-1 rounded px-1.5 py-0.5 hover:bg-[#22242a] transition cursor-pointer"
          [class.text-[#e6b763]]="keepAwake()"
          [class.text-[#8e9098]]="!keepAwake()"
        >
          <app-icon name="coffee" [size]="12" />
          <span class="hidden sm:inline">{{ keepAwake() ? 'On' : 'Off' }}</span>
          <span class="h-1.5 w-1.5 rounded-full" [class.bg-[#69b879]]="keepAwake()" [class.bg-[#5e6068]]="!keepAwake()"></span>
        </button>

        <!-- Resource Manager Trigger -->
        <button
          type="button"
          (click)="togglePopover('resources')"
          title="Fleet Resource Manager & PTYs"
          class="inline-flex items-center gap-1 rounded px-1.5 py-0.5 hover:bg-[#22242a] hover:text-[#ececed] transition cursor-pointer"
          [class.bg-[#24262e]]="openPopover() === 'resources'"
          [class.text-[#ececed]]="openPopover() === 'resources'"
        >
          <app-icon name="activity" [size]="12" />
          <span class="font-mono-ide">{{ fleet.resourceTotals().activePtys }}</span>
        </button>

        <!-- Listening Ports Trigger -->
        <button
          type="button"
          (click)="togglePopover('ports')"
          title="Listening ports across fleet"
          class="inline-flex items-center gap-1 rounded px-1.5 py-0.5 hover:bg-[#22242a] hover:text-[#ececed] transition cursor-pointer"
          [class.bg-[#24262e]]="openPopover() === 'ports'"
          [class.text-[#ececed]]="openPopover() === 'ports'"
        >
          <app-icon name="ports" [size]="12" />
          <span class="font-mono-ide">{{ fleet.listeningPorts().length }}</span>
        </button>

        <!-- Hosts Summary Trigger -->
        <button
          type="button"
          (click)="togglePopover('hosts')"
          title="Connected fleet hosts"
          class="inline-flex items-center gap-1 rounded px-1.5 py-0.5 hover:bg-[#22242a] hover:text-[#ececed] transition cursor-pointer"
          [class.bg-[#24262e]]="openPopover() === 'hosts'"
          [class.text-[#ececed]]="openPopover() === 'hosts'"
        >
          <app-icon name="server" [size]="12" />
          <span>{{ fleet.onlineWorkersCount() }} hosts</span>
        </button>

        <!-- Command Palette Shortcut (Herdr style) -->
        <button
          type="button"
          (click)="openCommands.emit()"
          title="Command Palette (⌘K)"
          class="hidden md:inline-flex items-center gap-1 rounded px-1.5 py-0.5 hover:bg-[#22242a] hover:text-[#ececed] text-[#787a82] transition cursor-pointer ml-1"
        >
          <span class="font-mono-ide text-[10px]">⌘K</span>
          <span>commands</span>
        </button>
      </div>

      <!-- POPOVER: SUBSCRIPTION QUOTAS & API TOKEN USAGE -->
      @if (openPopover() === 'usage') {
        <div class="ks-popover absolute bottom-8 left-3 z-30 w-88 max-w-[92vw] rounded-xl border border-[#2c2e36] overflow-hidden">
          <div class="flex items-center justify-between px-3 py-2 border-b border-[#24262c]">
            <span class="text-[12px] font-semibold text-[#ececed]">Subscription Quotas &amp; Usage</span>
            <button
              type="button"
              (click)="fleet.refresh()"
              class="text-[10.5px] text-[#8e9098] hover:text-[#ececed] cursor-pointer"
            >
              Refresh
            </button>
          </div>
          <div class="p-3 space-y-3 text-[11px] max-h-80 overflow-y-auto">
            <!-- Subscription Quotas (Primary) -->
            <div class="space-y-1.5">
              <div class="text-[10px] font-semibold uppercase tracking-wider text-[#686a72]">
                Subscription Windows
              </div>
              @if (subscriptionQuotas().length === 0) {
                <div class="rounded-lg bg-[#121316] border border-[#23252b] px-2.5 py-2 text-[#686a72]">
                  No rolling subscription quotas reported yet. Active providers appear here automatically.
                </div>
              } @else {
                <div class="space-y-2">
                  @for (q of subscriptionQuotas(); track q.provider + ':' + (q.window_label || '')) {
                    <div class="rounded-lg bg-[#121316] border border-[#23252b] px-2.5 py-2 space-y-1.5">
                      <div class="flex items-center justify-between gap-2">
                        <span class="inline-flex items-center gap-1.5 text-[#e4e4e7] font-medium">
                          <app-icon [name]="q.provider" [size]="12" />
                          <span>{{ q.label }}</span>
                          @if (q.window_label) {
                            <span class="font-mono-ide text-[10px] text-[#787a82]">{{ q.window_label }}</span>
                          }
                        </span>
                        <span
                          class="font-mono-ide font-medium"
                          [class.text-[#e05d5d]]="q.used_percent >= 90"
                          [class.text-[#d9a043]]="q.used_percent >= 70 && q.used_percent < 90"
                          [class.text-[#ececed]]="q.used_percent < 70"
                        >
                          {{ formatPct(q.used_percent) }}
                        </span>
                      </div>
                      <div class="h-1.5 w-full rounded-full bg-[#24262c] overflow-hidden">
                        <div
                          class="h-full rounded-full transition-all"
                          [class.bg-[#e05d5d]]="q.used_percent >= 90"
                          [class.bg-[#d9a043]]="q.used_percent >= 70 && q.used_percent < 90"
                          [class.bg-[#60a5fa]]="q.used_percent < 70"
                          [style.width.%]="q.used_percent"
                        ></div>
                      </div>
                      @if (q.reset_at) {
                        <div class="text-[10px] font-mono-ide text-[#686a72]">
                          {{ formatReset(q.reset_at) }}
                        </div>
                      }
                    </div>
                  }
                </div>
              }
            </div>

            <!-- API Token Usage (Fallback / Secondary) -->
            <div class="space-y-2 pt-2 border-t border-[#24262c]">
              <div class="flex items-center justify-between text-[10px] font-semibold uppercase tracking-wider text-[#686a72]">
                <span>API Token Usage</span>
                <span class="font-mono-ide font-normal">{{ fleet.usage().total_requests }} calls</span>
              </div>
              <div class="grid grid-cols-2 gap-2">
                <div class="rounded-lg bg-[#121316] border border-[#23252b] p-2">
                  <div class="text-[#686a72]">Input Tokens</div>
                  <div class="font-mono-ide text-[13px] text-[#ececed]">{{ formatNum(fleet.usage().total_input_tokens) }}</div>
                </div>
                <div class="rounded-lg bg-[#121316] border border-[#23252b] p-2">
                  <div class="text-[#686a72]">Output Tokens</div>
                  <div class="font-mono-ide text-[13px] text-[#ececed]">{{ formatNum(fleet.usage().total_output_tokens) }}</div>
                </div>
              </div>
              @if (fleet.usage().by_provider.length > 0) {
                <div class="space-y-1">
                  @for (b of fleet.usage().by_provider; track b.provider + b.model) {
                    <div class="flex items-center justify-between rounded bg-[#121316] px-2.5 py-1.5 font-mono-ide">
                      <span class="text-[#d4d4d8] truncate">{{ b.provider }}/{{ b.model }}</span>
                      <span class="text-[#8e9098] shrink-0">{{ formatNum(b.input_tokens + b.output_tokens) }}</span>
                    </div>
                  }
                </div>
              }
            </div>
          </div>
        </div>
      }

      <!-- POPOVER: RESOURCE MANAGER (Orca Image 1 style) -->
      @if (openPopover() === 'resources') {
        <div class="ks-popover absolute bottom-8 right-3 z-30 w-96 max-w-[94vw] rounded-xl border border-[#2c2e36] overflow-hidden">
          <div class="flex items-center justify-between px-3.5 py-2.5 border-b border-[#24262c]">
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
          <div class="flex items-center gap-3 px-3.5 pt-2 border-b border-[#24262c] text-[12px]">
            <button
              type="button"
              (click)="resourceTab.set('resources')"
              class="pb-2 border-b-2 transition cursor-pointer"
              [class.border-[#ececed]]="resourceTab() === 'resources'"
              [class.text-[#ececed]]="resourceTab() === 'resources'"
              [class.border-transparent]="resourceTab() !== 'resources'"
              [class.text-[#787a82]]="resourceTab() !== 'resources'"
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
              [class.text-[#787a82]]="resourceTab() !== 'ptys'"
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
                <div class="grid grid-cols-12 text-[10px] text-[#686a72] px-1.5 pb-1">
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
                <div class="py-6 text-center text-[12px] text-[#686a72]">No active PTY sessions running</div>
              }
              @for (t of activePtyTasks(); track t.id) {
                <div class="flex items-center justify-between gap-2 rounded-lg border border-[#24262c] bg-[#1b1c20] px-2.5 py-2">
                  <button
                    type="button"
                    (click)="focusTask(t.id)"
                    class="flex items-center gap-2 min-w-0 flex-1 text-left cursor-pointer"
                  >
                    <app-icon [name]="t.agent" [size]="12" />
                    <div class="min-w-0">
                      <div class="text-[12px] text-[#ececed] truncate">{{ t.agent }} · {{ t.workdir }}</div>
                      <div class="text-[10px] font-mono-ide text-[#787a82]">{{ fleet.workersById()[t.worker_id]?.name || t.worker_id }} · {{ t.status }}</div>
                    </div>
                  </button>
                  <button
                    type="button"
                    (click)="stopTask(t.id)"
                    class="rounded border border-[#2d3038] hover:border-[#e05d5d]/50 px-2 py-0.5 text-[10px] text-[#a1a3ab] hover:text-[#f09595] cursor-pointer"
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
        <div class="ks-popover absolute bottom-8 right-16 z-30 w-80 max-w-[92vw] rounded-xl border border-[#2c2e36] overflow-hidden">
          <div class="flex items-center justify-between px-3 py-2 border-b border-[#24262c]">
            <span class="text-[12px] font-semibold text-[#ececed]">Listening Fleet Ports</span>
            <span class="font-mono-ide text-[10px] text-[#787a82]">{{ fleet.listeningPorts().length }} open</span>
          </div>
          <div class="p-2 max-h-60 overflow-y-auto space-y-1">
            @if (fleet.listeningPorts().length === 0) {
              <div class="py-5 text-center text-[11px] text-[#686a72]">No listening dev ports detected yet.</div>
            }
            @for (p of fleet.listeningPorts(); track p.workerId + ':' + p.port) {
              <div class="flex items-center justify-between rounded-md px-2 py-1.5 hover:bg-[#1f2127] font-mono-ide text-[11px]">
                <div class="flex items-center gap-2 min-w-0">
                  <span class="text-[#69b879] font-semibold">:{{ p.port }}</span>
                  <span class="text-[#d4d4d8] truncate">{{ p.process }}</span>
                </div>
                <span class="text-[10px] text-[#787a82] shrink-0">{{ p.workerName }}</span>
              </div>
            }
          </div>
        </div>
      }

      <!-- POPOVER: HOSTS -->
      @if (openPopover() === 'hosts') {
        <div class="ks-popover absolute bottom-8 right-3 z-30 w-80 max-w-[92vw] rounded-xl border border-[#2c2e36] overflow-hidden">
          <div class="flex items-center justify-between px-3 py-2 border-b border-[#24262c]">
            <span class="text-[12px] font-semibold text-[#ececed]">Fleet Hosts</span>
            <span class="font-mono-ide text-[10px] text-[#787a82]">{{ fleet.onlineWorkersCount() }}/{{ fleet.workers().length }} online</span>
          </div>
          <div class="p-2 max-h-64 overflow-y-auto space-y-1.5">
            @for (w of fleet.workers(); track w.id) {
              <button
                type="button"
                (click)="inspectHost(w)"
                class="w-full text-left rounded-lg border border-[#23252c] hover:border-[#353946] bg-[#1a1b1f] hover:bg-[#202229] px-2.5 py-2 space-y-1 transition cursor-pointer"
              >
                <div class="flex items-center justify-between gap-2">
                  <span class="inline-flex items-center gap-1.5 text-[12px] font-medium text-[#ececed] truncate">
                    <span class="h-1.5 w-1.5 rounded-full shrink-0" [class.bg-[#69b879]]="w.status === 'online'" [class.bg-[#484a52]]="w.status !== 'online'"></span>
                    <span class="truncate">{{ w.name }}</span>
                  </span>
                  <span class="font-mono-ide text-[10px] text-[#787a82] shrink-0">{{ w.os }}/{{ w.arch }} · Details</span>
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
  readonly keepAwake = signal(false);

  private wakeLock: { release(): Promise<void> } | null = null;

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

  readonly subscriptionQuotas = computed(() => this.fleet.usage().quotas ?? []);

  readonly formattedTotalTokens = computed(() => {
    const total = this.fleet.usage().total_input_tokens + this.fleet.usage().total_output_tokens;
    if (total === 0) return '0 tok';
    if (total >= 1_000_000) return `${(total / 1_000_000).toFixed(1)}M tok`;
    if (total >= 1_000) return `${(total / 1_000).toFixed(1)}k tok`;
    return `${total} tok`;
  });

  togglePopover(name: FooterPopover): void {
    this.openPopover.update((cur) => (cur === name ? null : name));
  }

  inspectHost(w: Worker): void {
    this.openPopover.set(null);
    this.openMachineDetails.emit(w);
  }

  async toggleWakeLock(): Promise<void> {
    if (this.keepAwake()) {
      await this.releaseWakeLock();
      this.keepAwake.set(false);
      return;
    }
    try {
      const nav = navigator as Navigator & { wakeLock?: { request(type: string): Promise<{ release(): Promise<void> }> } };
      if (nav.wakeLock) {
        this.wakeLock = await nav.wakeLock.request('screen');
      }
      this.keepAwake.set(true);
    } catch {
      // Fallback visual toggle even on non-HTTPS localhost
      this.keepAwake.set(true);
    }
  }

  private async releaseWakeLock(): Promise<void> {
    if (this.wakeLock) {
      try {
        await this.wakeLock.release();
      } catch {
        // ignore
      }
      this.wakeLock = null;
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

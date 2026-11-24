import { Component, computed, inject, input, output, signal } from '@angular/core';
import { Worker } from '../core/models/konsert.models';
import { isVersionMismatch } from '../core/models/version';
import { ApiClientService } from '../core/services/api-client.service';
import { FleetStore } from '../core/state/fleet.store';
import { IconComponent } from '../shared/icon.component';
import { ModalDialogComponent } from '../shared/modal-dialog.component';
import { TooltipDirective } from '../shared/tooltip.directive';
import { ControlStore } from '../core/state/control.store';
import { HoldsRequestsComponent } from '../features/control/holds-requests.component';
import { YourActionsComponent } from '../features/control/your-actions.component';
import { MachineCredentialComponent } from './machine-credential.component';

@Component({
  selector: 'app-machine-detail-modal',
  standalone: true,
  imports: [IconComponent, ModalDialogComponent, TooltipDirective, HoldsRequestsComponent, YourActionsComponent, MachineCredentialComponent],
  template: `
    <app-modal-dialog labelledBy="machine-detail-title" panelClass="w-full max-w-[580px] rounded-[var(--ks-radius-xl)] bg-[var(--ks-surface-2)] p-6 space-y-5 max-h-[88vh] overflow-y-auto" (close)="close.emit()">
      <!-- Header -->
      <div class="flex items-start justify-between gap-3">
        <div class="flex items-start gap-3 min-w-0">
          <div class="h-9 w-9 rounded-lg bg-[#141518] flex items-center justify-center shrink-0">
            <app-icon
              [name]="worker().isolation === 'container' ? 'container' : worker().os === 'darwin' ? 'monitor' : 'server'"
              [size]="16"
              class="text-[#b4b6be]"
            />
          </div>
          <div class="min-w-0">
            <div class="flex items-center gap-2 flex-wrap">
              <h3 class="text-[15px] font-semibold text-[#ececed] truncate" id="machine-detail-title">{{ worker().name }}</h3>
              <span
                class="inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[10.5px] font-medium"
                [class.bg-[#69b879]/15]="worker().status === 'online'"
                [class.text-[#8fd49d]]="worker().status === 'online'"
                [class.bg-[#2a2c34]]="worker().status !== 'online'"
                [class.text-[#8e9098]]="worker().status !== 'online'"
              >
                <span
                  class="h-1.5 w-1.5 rounded-full"
                  [class.bg-[#69b879]]="worker().status === 'online'"
                  [class.bg-[#686a72]]="worker().status !== 'online'"
                ></span>
                {{ worker().status }}
              </span>
            </div>
            <div class="text-[11.5px] text-[#8e9098] font-mono-ide mt-0.5 truncate">
              {{ worker().os }}/{{ worker().arch }} · {{ worker().isolation }} · {{ worker().host_group }}
            </div>
          </div>
        </div>

        <button
          type="button"
          (click)="close.emit()"
          aria-label="Esc: close dialog"
          class="text-[11px] text-[#86888f] hover:text-[#ececed] transition cursor-pointer shrink-0 pt-0.5"
        >
          Esc
        </button>
      </div>

      <!-- Core Identity & Runtime Grid -->
      <div class="grid grid-cols-2 sm:grid-cols-4 gap-2 text-[11px]">
        <div class="rounded-lg bg-[#141518] px-3 py-2.5">
          <div class="text-[#86888f] text-[10px]">Permission</div>
          <div
            class="font-mono-ide mt-0.5 font-medium"
            [class.text-[#d9a043]]="worker().permission_mode === 'full_access'"
            [class.text-[#8fd49d]]="worker().permission_mode !== 'full_access'"
          >
            {{ worker().permission_mode }}
          </div>
        </div>
        <div class="rounded-lg bg-[#141518] px-3 py-2.5">
          <div class="text-[#86888f] text-[10px]">Active / Slots</div>
          <div class="font-mono-ide mt-0.5 text-[#e4e4e7]">
            {{ activeSessionsCount() }} / {{ worker().max_concurrent }}
          </div>
        </div>
        <div class="rounded-lg bg-[#141518] px-3 py-2.5">
          <div class="text-[#86888f] text-[10px]">Last Seen</div>
          <div class="font-mono-ide mt-0.5 text-[#d4d4d8] truncate">{{ lastSeenLabel() }}</div>
        </div>
        <div class="rounded-lg bg-[#141518] px-3 py-2.5">
          <div class="text-[#86888f] text-[10px]">Saved Spaces</div>
          <div class="font-mono-ide mt-0.5 text-[#e4e4e7]">{{ savedSpacesCount() }}</div>
        </div>
      </div>

      <!-- Worker ID row -->
      <div class="flex items-center justify-between gap-2 rounded-lg bg-[#141518] px-3 py-2.5 text-[11px]">
        <div class="min-w-0">
          <span class="text-[#86888f] mr-2">Worker ID</span>
          <span class="font-mono-ide text-[#c8cad0] break-all" [ksTooltip]="worker().id">{{ worker().sid || worker().id }}</span>
        </div>
        <button
          type="button"
          (click)="copyWorkerId()"
          class="inline-flex items-center gap-1 rounded bg-[#22242b] hover:bg-[#2b2e37] px-2 py-0.5 text-[10.5px] text-[#c8cad0] transition cursor-pointer shrink-0"
        >
          <app-icon name="copy" [size]="10" />
          <span>{{ copiedId() ? 'Copied' : 'Copy' }}</span>
        </button>
      </div>

      <!-- Machine control: drain, suspend all, stop all; holds and requests here (PRD §5.8a) -->
      <section class="rounded-lg bg-[#141518] px-3 py-2.5 text-[11px] space-y-2.5" aria-labelledby="machine-control-heading" data-testid="machine-control">
        <div class="flex items-center justify-between gap-2 flex-wrap">
          <div class="flex items-center gap-2">
            <h4 id="machine-control-heading" class="text-[#86888f]">Control</h4>
            @if (worker().drained) {
              <span class="rounded-full px-2 py-0.5 text-[10.5px] font-medium bg-[var(--ks-amber-soft)] text-[#e6b763]" data-testid="machine-drained">
                Drained: no new work
              </span>
            }
          </div>
          <div class="flex items-center gap-1 flex-wrap">
            <button
              type="button"
              class="ks-btn ks-btn-sm"
              data-testid="machine-drain"
              [attr.aria-pressed]="!!worker().drained"
              [disabled]="control.isBusy(worker().id)"
              (click)="control.setDrained(worker().id, !worker().drained)"
              [ksTooltip]="worker().drained ? 'Accept new work again' : 'Finish current work and accept nothing new'"
            >
              <app-icon name="drain" [size]="11" />
              {{ worker().drained ? 'Undrain' : 'Drain' }}
            </button>
            <button type="button" class="ks-btn ks-btn-sm" data-testid="machine-suspend-all" [disabled]="control.isBusy(worker().id)" (click)="control.workerBulk(worker().id, 'suspend-all')" ksTooltip="Suspend every managed session on this machine">
              <app-icon name="pause" [size]="11" />
              Suspend all
            </button>
            <button type="button" class="ks-btn ks-btn-sm" data-testid="machine-resume-all" [disabled]="control.isBusy(worker().id)" (click)="control.workerBulk(worker().id, 'resume-all')" ksTooltip="Resume suspended sessions on this machine">
              <app-icon name="play" [size]="11" />
              Resume all
            </button>
            <button type="button" class="ks-btn ks-btn-sm ks-btn-danger" data-testid="machine-stop-all" [disabled]="control.isBusy(worker().id)" (click)="stopAll()" ksTooltip="Cancel every managed session on this machine">
              <app-icon name="stop" [size]="10" />
              Stop all
            </button>
          </div>
        </div>
        <ks-holds-requests [workerId]="worker().id" />
        <ks-your-actions [workerId]="worker().id" [limit]="8" />
      </section>

      <!-- How the hub starts konsertd here, and the last attempt -->
      @if (worker().launch; as launch) {
        <div class="rounded-lg bg-[#141518] px-3 py-2.5 text-[11px] space-y-1.5" data-testid="worker-launch">
          <div class="flex items-center justify-between gap-2">
            <div class="min-w-0">
              <span class="text-[#86888f] mr-2">Started by the hub</span>
              <span class="font-mono-ide text-[#c8cad0] break-all">{{ launchLabel() }}</span>
            </div>
            @if (worker().status !== 'online') {
              @if (worker().launch_state === 'starting') {
                <span class="inline-flex items-center gap-1.5 text-[10.5px] text-[#c8cad0] shrink-0" data-testid="worker-launch-starting">
                  <span class="h-2.5 w-2.5 rounded-full border-[1.5px] border-[var(--ks-blue)] border-t-transparent ks-ring-spin"></span>
                  Starting konsertd...
                </span>
              } @else {
                <button
                  type="button"
                  (click)="retryStart()"
                  class="ks-btn ks-btn-sm shrink-0"
                  [ksTooltip]="launch === 'ssh' ? 'Install and start konsertd over ssh again' : 'Start konsertd on this machine again'"
                  data-testid="worker-launch-retry"
                >
                  <app-icon name="refresh" [size]="11" />
                  <span>{{ worker().launch_state === 'failed' ? 'Retry' : 'Start' }}</span>
                </button>
              }
            }
          </div>
          @if (worker().status !== 'online' && worker().launch_state === 'failed' && worker().launch_error) {
            <div role="alert" class="rounded-md bg-[var(--ks-red-soft)] px-2.5 py-1.5 text-[11px] text-[#f09595] break-words" data-testid="worker-launch-error">
              {{ worker().launch_error }}
            </div>
          }
        </div>
      }

      <!-- konsertd version row -->
      <div
        class="flex items-center justify-between gap-2 rounded-lg bg-[#141518] px-3 py-2.5 text-[11px]"
        data-testid="worker-version"
      >
        <div class="min-w-0">
          <span class="text-[#86888f] mr-2">konsertd version</span>
          <span class="font-mono-ide text-[#c8cad0] break-all">{{ versionLabel() }}</span>
        </div>
        @if (versionMismatch()) {
          <span
            class="inline-flex items-center gap-1 rounded-full bg-[#d9a043]/15 px-2 py-0.5 text-[10.5px] font-medium text-[#e6b566] shrink-0"
            [ksTooltip]="mismatchText()"
            data-testid="worker-version-mismatch"
          >
            Differs from hub {{ fleet.controllerVersion() }}
          </span>
        }
      </div>

      <!-- Live Telemetry Metrics -->
      @if (worker().metrics; as m) {
        <div class="rounded-lg bg-[#141518] p-3 space-y-2 text-[11.5px]">
          <div class="text-[10.5px] font-semibold uppercase tracking-wider text-[#8a8c94]">Live Telemetry</div>
          <div class="grid grid-cols-2 sm:grid-cols-4 gap-2 font-mono-ide text-[11px]">
            <div>
              <span class="text-[#86888f] block text-[10px]">CPU</span>
              <span class="text-[#ececed]">{{ m.cpu_percent.toFixed(0) }}%</span>
            </div>
            <div>
              <span class="text-[#86888f] block text-[10px]">Memory</span>
              <span class="text-[#ececed]">{{ formatMB(m.mem_used_mb) }} / {{ formatMB(m.mem_total_mb) }}</span>
            </div>
            <div>
              <span class="text-[#86888f] block text-[10px]">Load Avg</span>
              <span class="text-[#ececed]">{{ m.load_avg.toFixed(2) }}</span>
            </div>
            <div>
              <span class="text-[#86888f] block text-[10px]">Open Ports</span>
              <span class="text-[#ececed]">{{ m.listening_ports?.length || 0 }}</span>
            </div>
          </div>
        </div>
      }

      @if (worker().metrics?.listening_ports?.length) {
        <div class="rounded-lg bg-[#141518] p-3 space-y-1.5 text-[11.5px]" data-testid="machine-ports">
          <span class="text-[10.5px] font-semibold uppercase tracking-wider text-[#8a8c94]">Listening ports</span>
          <ul class="m-0 p-0 list-none flex flex-wrap gap-1.5 font-mono-ide">
            @for (p of worker().metrics!.listening_ports!; track p.port + (p.address ?? '')) {
              <li class="rounded bg-white/[0.05] px-1.5 py-0.5 text-[#d4d4d8]">{{ p.port }}@if (p.process) {<span class="text-[#86888f]"> {{ p.process }}</span>}</li>
            }
          </ul>
        </div>
      }

      <app-machine-credential [worker]="worker()" />

      <!-- Cached Machine Facts -->
      @if (worker().facts; as f) {
        <div class="rounded-lg bg-[#141518] p-3 space-y-2 text-[11.5px]">
          <div class="flex items-center justify-between">
            <span class="text-[10.5px] font-semibold uppercase tracking-wider text-[#8a8c94]">Machine Facts</span>
            <span class="font-mono-ide text-[10px] text-[#86888f]">{{ f.virtualization || worker().isolation }}</span>
          </div>
          <div class="grid grid-cols-1 sm:grid-cols-2 gap-1.5 text-[11px]">
            @if (f.os_name || f.os_version) {
              <div class="truncate">
                <span class="text-[#86888f]">OS: </span>
                <span class="text-[#d4d4d8]">{{ f.os_name }} {{ f.os_version }}</span>
              </div>
            }
            @if (f.cpu_model || f.cpu_cores) {
              <div class="truncate">
                <span class="text-[#86888f]">CPU: </span>
                <span class="text-[#d4d4d8]">{{ f.cpu_cores }}c {{ f.cpu_model }}</span>
              </div>
            }
            @if (f.disk_total_gb) {
              <div>
                <span class="text-[#86888f]">Disk: </span>
                <span class="font-mono-ide text-[#d4d4d8]">{{ f.disk_free_gb }} GB free / {{ f.disk_total_gb }} GB ({{ f.storage_type || 'disk' }})</span>
              </div>
            }
            @if (f.ram_total_mb) {
              <div>
                <span class="text-[#86888f]">RAM / Swap: </span>
                <span class="font-mono-ide text-[#d4d4d8]">{{ formatMB(f.ram_total_mb) }} / {{ formatMB(f.swap_total_mb) }}</span>
              </div>
            }
          </div>
          @if (toolEntries().length > 0) {
            <div class="flex flex-wrap gap-1 pt-1">
              @for (entry of toolEntries(); track entry.name) {
                <span class="rounded bg-[#1e2027] px-1.5 py-0.5 text-[10px] font-mono-ide text-[#a1a3ab]">
                  {{ entry.name }}@if (entry.version) { <span class="text-[#86888f]"> {{ entry.version }}</span> }
                </span>
              }
            </div>
          }
        </div>
      }

      <!-- Installed Agents -->
      <div class="space-y-1.5">
        <div class="text-[10.5px] font-semibold uppercase tracking-wider text-[#8a8c94]">Installed Agents</div>
        <div class="flex flex-wrap gap-1.5">
          @for (a of worker().agents; track a) {
            <span class="inline-flex items-center gap-1.5 rounded-md bg-[#141518] px-2 py-1 text-[11px] font-mono-ide text-[#d4d4d8]">
              <app-icon [name]="a" [size]="11" />
              <span>{{ a }}</span>
            </span>
          }
        </div>
      </div>

      <!-- Inline "Are you sure?" Remove Confirmation -->
      @if (confirmRemove()) {
        <div class="rounded-lg bg-[#e05d5d]/10 p-3.5 space-y-3">
          <div class="space-y-1">
            <div class="text-[12.5px] font-semibold text-[#f5b4b4]">
              Are you sure you want to remove "{{ worker().name }}"?
            </div>
            <p class="text-[11.5px] text-[#e6b8b8]/85 leading-relaxed">
              This will disconnect its <code class="font-mono-ide">konsertd</code> daemon (stopping auto-reconnect) and remove its saved spaces and repo links. Task history is preserved.
            </p>
          </div>
          @if (removeError()) {
            <div class="text-[11.5px] text-[#fca5a5]">{{ removeError() }}</div>
          }
          <div class="flex items-center justify-end gap-2">
            <button
              type="button"
              (click)="confirmRemove.set(false)"
              [disabled]="removing()"
              class="ks-btn ks-btn-sm ks-btn-soft"
            >
              Cancel
            </button>
            <button
              type="button"
              (click)="confirmDeleteMachine()"
              [disabled]="removing()"
              class="rounded-md bg-[#c94a4a] hover:bg-[#d85555] disabled:opacity-50 px-3.5 py-1.5 text-[11.5px] font-medium text-white transition cursor-pointer"
            >
              {{ removing() ? 'Removing...' : 'Yes, Remove Machine' }}
            </button>
          </div>
        </div>
      } @else {
        <!-- Footer Actions -->
        <div class="flex items-center justify-between gap-2 pt-2 border-t border-white/[0.06]">
          <button
            type="button"
            (click)="confirmRemove.set(true)"
            class="ks-btn ks-btn-sm ks-btn-danger"
          >
            <app-icon name="trash" [size]="12" />
            <span>Remove Machine</span>
          </button>

          <div class="flex items-center gap-2">
            @if (worker().status === 'online') {
              <button
                type="button"
                (click)="startTerminal.emit(worker())"
                class="ks-btn ks-btn-sm ks-btn-soft"
              >
                <app-icon name="shell" [size]="12" />
                <span>Terminal</span>
              </button>
              <button
                type="button"
                (click)="startChat.emit(worker())"
                class="ks-btn ks-btn-primary"
              >
                <app-icon name="orchestrator" [size]="12" />
                <span>New Chat</span>
              </button>
            } @else {
              <button
                type="button"
                (click)="close.emit()"
                class="ks-btn ks-btn-soft"
              >
                Close
              </button>
            }
          </div>
        </div>
      }
    </app-modal-dialog>
  `,
})
export class MachineDetailModalComponent {
  readonly worker = input.required<Worker>();
  readonly close = output<void>();
  readonly removed = output<string>();
  readonly startChat = output<Worker>();
  readonly startTerminal = output<Worker>();

  private readonly api = inject(ApiClientService);
  readonly fleet = inject(FleetStore);
  readonly control = inject(ControlStore);

  readonly confirmRemove = signal(false);
  readonly removing = signal(false);
  readonly removeError = signal('');
  readonly copiedId = signal(false);

  readonly activeSessionsCount = computed(() => {
    const wid = this.worker().id;
    return this.fleet
      .tasks()
      .filter((t) => t.worker_id === wid && (t.status === 'running' || t.status === 'waiting_input' || t.status === 'assigned' || t.status === 'suspended'))
      .length;
  });

  readonly savedSpacesCount = computed(() => {
    const wid = this.worker().id;
    return this.fleet.spaces().filter((s) => s.worker_id === wid).length;
  });

  /** The reported version, or "unknown" when the worker sent none. */
  readonly versionLabel = computed(() => this.worker().version?.trim() || 'unknown');

  readonly versionMismatch = computed(() =>
    isVersionMismatch(this.fleet.controllerVersion(), this.worker().version),
  );

  readonly mismatchText = computed(
    () =>
      `konsertd version ${this.worker().version} differs from the hub version ${this.fleet.controllerVersion()}`,
  );

  readonly toolEntries = computed(() => {
    const tools = this.worker().facts?.tools;
    if (!tools) return [];
    return Object.entries(tools)
      .map(([name, version]) => ({ name, version }))
      .sort((a, b) => a.name.localeCompare(b.name))
      .slice(0, 14);
  });

  readonly lastSeenLabel = computed(() => {
    const raw = this.worker().last_seen_at;
    if (!raw) return '—';
    const d = new Date(raw);
    if (isNaN(d.getTime())) return raw;
    const secs = Math.max(0, Math.floor((Date.now() - d.getTime()) / 1000));
    if (secs < 10) return 'just now';
    if (secs < 60) return `${secs}s ago`;
    const mins = Math.floor(secs / 60);
    if (mins < 60) return `${mins}m ago`;
    return `${Math.floor(mins / 60)}h ago`;
  });

  readonly launchLabel = computed(() => {
    const w = this.worker();
    return w.launch === 'ssh' ? `over ssh (${w.ssh_target || w.name})` : 'on the hub\'s machine';
  });

  stopAll(): void {
    if (!window.confirm(`Stop every managed session on ${this.worker().name}?`)) return;
    void this.control.workerBulk(this.worker().id, 'stop-all');
  }

  retryStart(): void {
    void this.fleet.retryStart(this.worker().id);
  }

  formatMB(mb: number): string {
    if (!mb) return '0 MB';
    if (mb >= 1024) return `${(mb / 1024).toFixed(1)} GB`;
    return `${mb} MB`;
  }

  async copyWorkerId(): Promise<void> {
    try {
      await navigator.clipboard.writeText(this.worker().sid || this.worker().id);
      this.copiedId.set(true);
      setTimeout(() => this.copiedId.set(false), 1600);
    } catch {
      // ignore
    }
  }

  async confirmDeleteMachine(): Promise<void> {
    const w = this.worker();
    this.removing.set(true);
    this.removeError.set('');
    try {
      await this.api.deleteWorker(w.id);
      this.fleet.removeWorker(w.id);
      this.removed.emit(w.id);
      this.close.emit();
    } catch (err) {
      this.removeError.set(err instanceof Error ? err.message : 'Failed to remove machine');
    } finally {
      this.removing.set(false);
    }
  }
}

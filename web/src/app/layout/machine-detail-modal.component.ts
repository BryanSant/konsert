import { Component, computed, inject, input, output, signal } from '@angular/core';
import { Worker } from '../core/models/konsert.models';
import { isVersionMismatch } from '../core/models/version';
import { ApiClientService } from '../core/services/api-client.service';
import { FleetStore } from '../core/state/fleet.store';
import { TabsStore } from '../core/state/tabs.store';
import { IconComponent } from '../shared/icon.component';
import { ModalDialogComponent } from '../shared/modal-dialog.component';
import { TooltipDirective } from '../shared/tooltip.directive';

@Component({
  selector: 'app-machine-detail-modal',
  standalone: true,
  imports: [IconComponent, ModalDialogComponent, TooltipDirective],
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
  private readonly tabs = inject(TabsStore);

  readonly confirmRemove = signal(false);
  readonly removing = signal(false);
  readonly removeError = signal('');
  readonly copiedId = signal(false);

  readonly activeSessionsCount = computed(() => {
    const wid = this.worker().id;
    return this.fleet
      .tasks()
      .filter((t) => t.worker_id === wid && (t.status === 'running' || t.status === 'waiting_input' || t.status === 'assigned'))
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
      const workerSpaces = this.fleet.spaces().filter((s) => s.worker_id === w.id);
      await this.api.deleteWorker(w.id);
      this.fleet.removeWorker(w.id);
      const curSpace = this.tabs.activeSpaceId();
      if (curSpace && workerSpaces.some((s) => s.id === curSpace)) {
        this.tabs.activeSpaceId.set(null);
      }
      this.removed.emit(w.id);
      this.close.emit();
    } catch (err) {
      this.removeError.set(err instanceof Error ? err.message : 'Failed to remove machine');
    } finally {
      this.removing.set(false);
    }
  }
}

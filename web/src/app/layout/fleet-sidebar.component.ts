import { Component, OnDestroy, OnInit, computed, inject, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Space, Task, Worker } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { AuthService } from '../core/services/auth.service';
import { FleetStore } from '../core/state/fleet.store';
import { ORCHESTRATOR_TAB_ID, TabsStore } from '../core/state/tabs.store';
import { IconComponent } from '../shared/icon.component';

@Component({
  selector: 'app-fleet-sidebar',
  standalone: true,
  imports: [FormsModule, IconComponent],
  template: `
    <div class="flex flex-col h-full w-full bg-[#16171a] text-[#d4d4d8] select-none">
      <!-- Top Search & Orchestrator Navigation -->
      <div class="px-2.5 pt-2.5 pb-2 space-y-1.5 shrink-0">
        <div class="relative">
          <app-icon name="search" [size]="13" class="absolute left-2.5 top-1/2 -translate-y-1/2 text-[#686a72]" />
          <input
            type="text"
            [ngModel]="query()"
            (ngModelChange)="query.set($event)"
            placeholder="Search spaces, agents..."
            class="w-full rounded-md border border-transparent bg-[#1f2025] focus:border-[#343741] pl-8 pr-2.5 py-1.5 text-[12px] text-[#e4e4e7] placeholder-[#686a72] focus:outline-none transition"
          />
        </div>

        <button
          type="button"
          (click)="selectOrchestrator()"
          class="w-full flex items-center justify-between rounded-md px-2 py-1.5 text-[12px] transition cursor-pointer"
          [class.bg-[#24262d]]="tabs.activeTabId() === orchestratorId"
          [class.text-[#f4f4f5]]="tabs.activeTabId() === orchestratorId"
          [class.text-[#a1a3ab]]="tabs.activeTabId() !== orchestratorId"
          [class.hover:bg-[#1d1f24]]="tabs.activeTabId() !== orchestratorId"
        >
          <span class="inline-flex items-center gap-2 font-medium">
            <app-icon name="orchestrator" [size]="13" class="text-[#60a5fa]" />
            <span>Orchestrator</span>
          </span>
          @if (fleet.activeTasksCount() > 0) {
            <span class="rounded-full bg-[#2a2d36] px-1.5 py-0.5 text-[10px] font-mono-ide text-[#a1a3ab]">
              {{ fleet.activeTasksCount() }}
            </span>
          }
        </button>
      </div>

      <!-- Herdr-style Spaces / Machines Tree (Upper Section) -->
      <div class="flex-1 min-h-0 overflow-y-auto px-2 py-1 space-y-0.5">
        <div class="flex items-center justify-between px-1.5 py-1 text-[11px] font-medium text-[#787a82]">
          <span>Spaces</span>
          <button
            type="button"
            (click)="openNewSpace.emit(null)"
            title="New space on a machine"
            class="rounded hover:bg-[#24262d] p-0.5 text-[#787a82] hover:text-[#ececed] transition cursor-pointer"
          >
            <app-icon name="plus" [size]="13" />
          </button>
        </div>

        @if (filteredGroups().length === 0) {
          <div class="px-2 py-4 text-[12px] text-[#686a72] leading-relaxed">
            No machines connected yet. Click <span class="text-[#a1a3ab]">+ Add machine</span> below to connect a host or container.
          </div>
        }

        @for (group of filteredGroups(); track group.hostGroup) {
          @for (node of group.workers; track node.worker.id) {
            <div class="space-y-0.5">
              <!-- Machine Row -->
              <div class="group flex items-center justify-between gap-1 rounded-md px-1.5 py-1 hover:bg-[#1e2026] transition">
                <button
                  type="button"
                  (click)="toggleWorker(node.worker.id)"
                  class="flex items-center gap-2 min-w-0 flex-1 text-left cursor-pointer"
                >
                  <app-icon
                    [name]="workerIcon(node.worker)"
                    [size]="13"
                    class="text-[#787a82] group-hover:text-[#b4b6be]"
                  />
                  <span class="text-[12px] text-[#d4d4d8] truncate">{{ node.worker.name }}</span>
                  @if (node.worker.isolation === 'container') {
                    <span class="text-[9px] font-mono-ide text-[#686a72] shrink-0">ctr</span>
                  } @else if (node.worker.isolation === 'vm') {
                    <span class="text-[9px] font-mono-ide text-[#686a72] shrink-0">vm</span>
                  }
                </button>

                <div class="flex items-center gap-1.5 shrink-0">
                  <button
                    type="button"
                    (click)="openNewSpace.emit(node.worker)"
                    title="New space on {{ node.worker.name }}"
                    class="opacity-70 group-hover:opacity-100 rounded hover:bg-[#2a2d36] p-0.5 text-[#8e9098] hover:text-[#ececed] transition cursor-pointer"
                  >
                    <app-icon name="plus" [size]="12" />
                  </button>
                  <span
                    class="h-1.5 w-1.5 rounded-full"
                    [class.bg-[#69b879]]="node.worker.status === 'online' && node.effectiveStatus !== 'waiting_input'"
                    [class.bg-[#d9a043]]="node.effectiveStatus === 'waiting_input'"
                    [class.bg-[#484a52]]="node.worker.status !== 'online'"
                  ></span>
                  <button
                    type="button"
                    (click)="toggleWorker(node.worker.id)"
                    class="text-[#686a72] hover:text-[#b4b6be] cursor-pointer transition-transform duration-150"
                    [class.-rotate-90]="isCollapsed(node.worker.id)"
                  >
                    <app-icon name="chevron-down" [size]="12" />
                  </button>
                </div>
              </div>

              <!-- Nested Persisted Spaces on this Machine (Herdr exact feel!) -->
              @if (!isCollapsed(node.worker.id)) {
                <div class="pl-2 space-y-0.5 pb-1">
                  @for (sp of node.spaces; track sp.id) {
                    <div
                      (click)="onSelectSpace(sp, node.worker)"
                      class="group/sp w-full flex items-center justify-between gap-2 rounded-md px-2 py-1.5 text-left transition cursor-pointer"
                      [class.bg-[#24262e]]="tabs.activeSpaceId() === sp.id"
                      [class.text-[#f4f4f5]]="tabs.activeSpaceId() === sp.id"
                      [class.hover:bg-[#1c1e24]]="tabs.activeSpaceId() !== sp.id"
                      [class.text-[#b4b6be]]="tabs.activeSpaceId() !== sp.id"
                    >
                      <div class="flex items-center gap-2 min-w-0">
                        <span class="h-4 w-4 rounded flex items-center justify-center text-[10px] font-semibold bg-[#1f8a8c]/85 text-white shrink-0">
                          {{ spaceInitial(sp.name) }}
                        </span>
                        <span class="text-[12px] truncate">{{ sp.name }}</span>
                      </div>

                      <div class="flex items-center gap-1 shrink-0">
                        @if (sp.branch) {
                          <span class="text-[10px] font-mono-ide text-[#686a72] truncate max-w-[70px]">
                            {{ sp.branch }}
                          </span>
                        }
                        <button
                          type="button"
                          (click)="$event.stopPropagation(); deleteSpace(sp)"
                          title="Remove space"
                          class="opacity-0 group-hover/sp:opacity-100 rounded p-0.5 hover:bg-white/10 text-[#686a72] hover:text-[#f09595] transition cursor-pointer"
                        >
                          <app-icon name="close" [size]="10" />
                        </button>
                      </div>
                    </div>
                  }

                  @if (node.worker.status === 'online') {
                    <button
                      type="button"
                      (click)="openNewSpace.emit(node.worker)"
                      class="w-full flex items-center gap-2 rounded-md px-2 py-1 text-left text-[11px] text-[#686a72] hover:text-[#a1a3ab] hover:bg-[#1c1e24] transition cursor-pointer"
                    >
                      <app-icon name="plus" [size]="11" />
                      <span>New space there</span>
                    </button>
                  }
                </div>
              }
            </div>
          }
        }
      </div>

      <!-- Herdr-style Live Agents Roster (Lower Section) -->
      <div class="h-[36%] min-h-[140px] max-h-[260px] flex flex-col border-t border-[#26282e] px-2 pt-2 pb-1 shrink-0">
        <div class="flex items-center justify-between px-1.5 pb-1.5 text-[11px] font-medium text-[#787a82] shrink-0">
          <span>Agents</span>
          <app-icon name="clock" [size]="12" class="text-[#5e6068]" />
        </div>

        <div class="flex-1 overflow-y-auto space-y-0.5">
          @if (filteredRoster().length === 0) {
            <div class="px-1.5 py-2 text-[11px] text-[#5e6068] leading-relaxed">
              No active or recent agents yet. Open a Space or ask the Orchestrator to dispatch a job.
            </div>
          } @else {
            @if (activeRosterCount() > 0) {
              <div class="px-1.5 pt-0.5 pb-1 text-[10px] text-[#686a72]">
                Running {{ activeRosterCount() }}
              </div>
            }
            @for (item of filteredRoster(); track item.task.id) {
              <button
                type="button"
                (click)="selectTask(item.task, fleet.workersById()[item.task.worker_id])"
                class="w-full flex items-center justify-between gap-2 rounded-md px-1.5 py-1.5 text-left transition cursor-pointer"
                [class.bg-[#24262e]]="tabs.activeTabId() === item.task.id"
                [class.hover:bg-[#1e2026]]="tabs.activeTabId() !== item.task.id"
              >
                <div class="flex items-center gap-2 min-w-0">
                  <app-icon [name]="item.task.agent" [size]="13" />
                  <span class="text-[12px] text-[#e4e4e7] truncate">{{ item.label }}</span>
                </div>

                <div class="flex items-center gap-1.5 shrink-0 text-[10px] font-mono-ide text-[#686a72]">
                  <span class="max-w-[68px] truncate">{{ item.workerName }}</span>
                  <span>{{ elapsedLabel(item.task) }}</span>
                  @if (item.task.status === 'running' || item.task.status === 'assigned') {
                    <span class="h-2.5 w-2.5 rounded-full border-[1.5px] border-[#60a5fa] border-t-transparent ks-ring-spin"></span>
                  } @else if (item.task.status === 'waiting_input') {
                    <span class="h-2 w-2 rounded-full bg-[#d9a043]" title="Needs you"></span>
                  } @else if (item.task.status === 'failed') {
                    <span class="h-2 w-2 rounded-full bg-[#e05d5d]"></span>
                  } @else {
                    <span class="h-2 w-2 rounded-full border border-[#5e6068]"></span>
                  }
                </div>
              </button>
            }
          }
        </div>
      </div>

      <!-- Herdr-style Bottom Pinned Controls -->
      <div class="border-t border-[#26282e] px-2 py-2 space-y-0.5 shrink-0">
        <button
          type="button"
          (click)="openSettings.emit()"
          class="w-full flex items-center justify-between rounded-md px-2 py-1 text-[12px] text-[#8e9098] hover:text-[#ececed] hover:bg-[#1e2026] transition cursor-pointer"
        >
          <span class="inline-flex items-center gap-2">
            <app-icon name="settings" [size]="13" />
            <span>Settings</span>
          </span>
          @if (auth.authRequired()) {
            <span class="text-[10px] text-[#5e6068]">{{ auth.user() }}</span>
          }
        </button>
        <button
          type="button"
          (click)="openAddMachine.emit()"
          class="w-full flex items-center gap-2 rounded-md px-2 py-1 text-[12px] text-[#8e9098] hover:text-[#ececed] hover:bg-[#1e2026] transition cursor-pointer"
        >
          <app-icon name="plus" [size]="13" />
          <span>Add machine</span>
        </button>
      </div>
    </div>
  `,
})
export class FleetSidebarComponent implements OnInit, OnDestroy {
  readonly taskSelected = output<void>();
  readonly openNewSpace = output<Worker | null>();
  readonly spaceSelected = output<{ space: Space; worker: Worker }>();
  readonly openAddMachine = output<void>();
  readonly openSettings = output<void>();

  readonly orchestratorId = ORCHESTRATOR_TAB_ID;
  readonly fleet = inject(FleetStore);
  readonly tabs = inject(TabsStore);
  readonly auth = inject(AuthService);
  private readonly api = inject(ApiClientService);

  readonly query = signal('');
  private readonly collapsedWorkers = signal<Record<string, boolean>>({});
  private readonly nowTick = signal(Date.now());
  private timer: ReturnType<typeof setInterval> | null = null;

  readonly filteredGroups = computed(() => {
    const q = this.query().trim().toLowerCase();
    const groups = this.fleet.groupedFleet();
    if (!q) return groups;
    return groups
      .map((g) => ({
        ...g,
        workers: g.workers.filter(
          (n) =>
            n.worker.name.toLowerCase().includes(q) ||
            n.worker.host_group.toLowerCase().includes(q) ||
            n.spaces.some((s) => s.name.toLowerCase().includes(q) || s.path.toLowerCase().includes(q)) ||
            n.worker.agents.some((a) => a.toLowerCase().includes(q))
        ),
      }))
      .filter((g) => g.workers.length > 0);
  });

  readonly filteredRoster = computed(() => {
    const q = this.query().trim().toLowerCase();
    const list = this.fleet.agentsRoster();
    if (!q) return list;
    return list.filter(
      (i) =>
        i.label.toLowerCase().includes(q) ||
        i.workerName.toLowerCase().includes(q) ||
        i.task.agent.toLowerCase().includes(q)
    );
  });

  readonly activeRosterCount = computed(() =>
    this.filteredRoster().filter((i) => i.isActive).length
  );

  ngOnInit(): void {
    this.timer = setInterval(() => this.nowTick.set(Date.now()), 5000);
  }

  ngOnDestroy(): void {
    if (this.timer) clearInterval(this.timer);
  }

  selectOrchestrator(): void {
    this.tabs.selectTab(ORCHESTRATOR_TAB_ID);
    this.taskSelected.emit();
  }

  toggleWorker(workerId: string): void {
    this.collapsedWorkers.update((m) => ({ ...m, [workerId]: !m[workerId] }));
  }

  isCollapsed(workerId: string): boolean {
    return !!this.collapsedWorkers()[workerId];
  }

  workerIcon(w: Worker): string {
    if (w.isolation === 'container') return 'container';
    if (w.os === 'darwin') return 'monitor';
    return 'server';
  }

  spaceInitial(name: string): string {
    if (name === '~') return '~';
    return name.slice(0, 1).toUpperCase() || 'S';
  }

  onSelectSpace(space: Space, worker: Worker): void {
    this.spaceSelected.emit({ space, worker });
    this.taskSelected.emit();
  }

  async deleteSpace(space: Space): Promise<void> {
    try {
      await this.api.deleteSpace(space.id);
      this.fleet.removeSpace(space.id);
      if (this.tabs.activeSpaceId() === space.id) {
        this.tabs.activeSpaceId.set(null);
      }
    } catch {
      // ignore
    }
  }

  selectTask(task: Task, worker?: Worker): void {
    this.tabs.openTaskTab(task, worker?.name);
    this.taskSelected.emit();
  }

  elapsedLabel(task: Task): string {
    const start = new Date(task.started_at || task.created_at).getTime();
    const end = task.completed_at ? new Date(task.completed_at).getTime() : this.nowTick();
    const secs = Math.max(0, Math.floor((end - start) / 1000));
    if (secs < 60) return '<1m';
    const mins = Math.floor(secs / 60);
    if (mins < 60) return `${mins}m`;
    return `${Math.floor(mins / 60)}h`;
  }
}

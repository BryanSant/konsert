import { Component, ElementRef, OnDestroy, OnInit, ViewChild, computed, inject, signal } from '@angular/core';
import { Repo, SessionKind, Space, Worker, Worktree } from '../core/models/konsert.models';
import { AuthService } from '../core/services/auth.service';
import { KonsertSocketService } from '../core/services/konsert-socket.service';
import { NotificationService } from '../core/services/notification.service';
import { FleetStore } from '../core/state/fleet.store';
import { ORCHESTRATOR_TAB_ID, TabsStore } from '../core/state/tabs.store';
import { OrchestratorChatComponent } from '../features/orchestrator/orchestrator-chat.component';
import { XtermPaneComponent } from '../features/terminal/xterm-pane.component';
import { IconComponent } from '../shared/icon.component';
import { AddMachineModalComponent } from './add-machine-modal.component';
import { CommandPaletteComponent, PaletteCommand } from './command-palette.component';
import { FleetSidebarComponent } from './fleet-sidebar.component';
import { FloatingScratchComponent } from './floating-scratch.component';
import { HotkeyAction, HotkeyService } from './hotkey.service';
import { MachineDetailModalComponent } from './machine-detail-modal.component';
import { NewSpaceModalComponent } from './new-space-modal.component';
import { SessionDispatchModalComponent, SessionDispatchRequest } from './session-dispatch-modal.component';
import { SessionLauncherService } from './session-launcher.service';
import { SettingsModalComponent } from './settings-modal.component';
import { StatusFooterComponent } from './status-footer.component';
import { TabStripComponent } from './tab-strip.component';
import { InspectorPanel, TelemetryDrawerComponent } from './telemetry-drawer.component';

@Component({
  selector: 'app-shell',
  standalone: true,
  imports: [
    IconComponent,
    FleetSidebarComponent,
    TelemetryDrawerComponent,
    StatusFooterComponent,
    FloatingScratchComponent,
    MachineDetailModalComponent,
    SettingsModalComponent,
    OrchestratorChatComponent,
    XtermPaneComponent,
    CommandPaletteComponent,
    AddMachineModalComponent,
    SessionDispatchModalComponent,
    NewSpaceModalComponent,
    TabStripComponent,
  ],
  template: `
    <div class="flex flex-col h-screen w-screen overflow-hidden bg-[#111214] text-[#ececed]">
      <!-- Top Unified IDE Chrome Bar (36px) -->
      <header class="h-9 flex items-center justify-between border-b border-[#26282e] bg-[#16171a] shrink-0 select-none">
        <!-- Left Brand & Sidebar Toggle -->
        <div
          class="flex items-center gap-2 pl-2.5 pr-3 h-full border-r border-[#26282e] shrink-0 transition-[width] duration-200 ease-[cubic-bezier(0.16,1,0.3,1)]"
          [class.md:w-[250px]]="leftOpen()"
        >
          <button
            type="button"
            (click)="mobileFleetOpen.set(!mobileFleetOpen())"
            aria-label="Toggle fleet sidebar"
            [attr.aria-expanded]="mobileFleetOpen()"
            class="md:hidden rounded p-1 text-[#8e9098] hover:text-[#ececed] hover:bg-[#23252b] active:scale-95 transition cursor-pointer"
          >
            <app-icon name="sidebar-left" [size]="15" />
          </button>

          <img src="icon.svg" alt="Konsert" class="h-4 w-4 rounded-sm" />
          <span class="text-[13px] font-semibold tracking-tight text-[#ececed]">Konsert</span>

          <div class="flex-1"></div>

          <button
            type="button"
            (click)="commandPaletteOpen.set(true)"
            title="Search / Commands (⌘K)"
            class="hidden md:inline-flex rounded p-1 text-[#8a8c94] hover:text-[#ececed] hover:bg-[#23252b] active:scale-95 transition cursor-pointer"
          >
            <app-icon name="search" [size]="14" />
          </button>
          <button
            type="button"
            (click)="leftOpen.set(!leftOpen())"
            title="Toggle left sidebar"
            [attr.aria-expanded]="leftOpen()"
            aria-controls="ks-left-pane"
            class="hidden md:inline-flex rounded p-1 text-[#8a8c94] hover:text-[#ececed] hover:bg-[#23252b] active:scale-95 transition cursor-pointer"
          >
            <app-icon name="sidebar-left" [size]="14" />
          </button>
        </div>

        <!-- Center Minimal Herdr-style Tab Strip (scoped to active Space when selected) -->
        <app-tab-strip (newTab)="openNewTabInActiveContext()" (toggleSplit)="toggleSplitPane()" />

        <!-- Right Orca-style Inspector Tab Icons & Collapse Toggle -->
        <div class="flex items-center gap-1 pr-2.5 pl-2 h-full border-l border-[#26282e] shrink-0">
          @for (p of inspectorPanels; track p.id) {
            <button
              type="button"
              (click)="selectInspector(p.id)"
              [title]="p.title"
              [attr.aria-label]="p.title"
              [attr.aria-pressed]="rightOpen() && inspectorPanel() === p.id"
              class="hidden md:inline-flex rounded p-1.5 active:scale-95 transition cursor-pointer hover:text-[#ececed]"
              [class.bg-[#24262d]]="rightOpen() && inspectorPanel() === p.id"
              [class.text-[#ececed]]="rightOpen() && inspectorPanel() === p.id"
              [class.text-[#8a8c94]]="!rightOpen() || inspectorPanel() !== p.id"
            >
              <app-icon [name]="p.icon" [size]="14" />
            </button>
          }

          <button
            type="button"
            (click)="rightOpen.set(!rightOpen())"
            title="Toggle right inspector"
            [attr.aria-expanded]="rightOpen()"
            aria-controls="ks-right-pane"
            class="hidden md:inline-flex rounded p-1.5 text-[#8a8c94] hover:text-[#ececed] hover:bg-[#23252b] active:scale-95 transition cursor-pointer ml-1"
          >
            <app-icon name="sidebar-right" [size]="14" />
          </button>

          <!-- Mobile Telemetry Toggle -->
          <button
            type="button"
            (click)="mobileTelemetryOpen.set(!mobileTelemetryOpen())"
            aria-label="Toggle inspector"
            [attr.aria-expanded]="mobileTelemetryOpen()"
            class="md:hidden rounded p-1 text-[#8e9098] hover:text-[#ececed] active:scale-95 transition cursor-pointer"
          >
            <app-icon name="sidebar-right" [size]="15" />
          </button>
        </div>
      </header>

      <!-- Main 3-Pane Workspace (Animated Sidebars!) -->
      <div class="flex flex-1 min-h-0 relative overflow-hidden">
        <!-- Desktop Left Sidebar (smooth width/opacity collapse) -->
        <aside
          id="ks-left-pane"
          aria-label="Fleet"
          [attr.inert]="leftOpen() ? null : ''"
          class="hidden md:block shrink-0 h-full overflow-hidden transition-[width,opacity] duration-200 ease-[cubic-bezier(0.16,1,0.3,1)]"
          [class.w-[250px]]="leftOpen()"
          [class.opacity-100]="leftOpen()"
          [class.border-r]="leftOpen()"
          [class.border-[#26282e]]="leftOpen()"
          [class.w-0]="!leftOpen()"
          [class.opacity-0]="!leftOpen()"
        >
          <div class="w-[250px] h-full">
            <app-fleet-sidebar
              (openNewSpace)="openNewSpaceModal($event)"
              (spaceSelected)="handleSpaceSelected($event)"
              (worktreeSelected)="handleWorktreeSelected($event)"
              (openMachineDetails)="openMachineDetailsModal($event)"
              (openAddMachine)="addMachineOpen.set(true)"
              (openSettings)="settingsOpen.set(true)"
            />
          </div>
        </aside>

        <!-- Mobile Slide-Over Fleet Drawer (< 768px) -->
        @if (mobileFleetOpen()) {
          <div class="md:hidden fixed inset-0 z-40 flex" role="dialog" aria-modal="true" aria-label="Fleet">
            <div class="ks-drawer-left w-[82vw] max-w-[290px] h-full border-r border-[#26282e] shadow-2xl z-10">
              <app-fleet-sidebar
                (taskSelected)="mobileFleetOpen.set(false)"
                (openNewSpace)="mobileFleetOpen.set(false); openNewSpaceModal($event)"
                (spaceSelected)="mobileFleetOpen.set(false); handleSpaceSelected($event)"
                (worktreeSelected)="mobileFleetOpen.set(false); handleWorktreeSelected($event)"
                (openMachineDetails)="mobileFleetOpen.set(false); openMachineDetailsModal($event)"
                (openAddMachine)="mobileFleetOpen.set(false); addMachineOpen.set(true)"
                (openSettings)="mobileFleetOpen.set(false); settingsOpen.set(true)"
              />
            </div>
            <div class="ks-backdrop flex-1" (click)="mobileFleetOpen.set(false)"></div>
          </div>
        }

        <!-- Center Active Pane Container (Supports Side-by-Side Split Terminal Panes!) -->
        <main #mainPane aria-label="Sessions" class="flex-1 min-w-0 h-full relative overflow-hidden bg-[#111214]">
          @let splitActive = isSplitVisible();
          @for (tab of tabs.openTabs(); track tab.id) {
            @let isPrimary = tabs.activeTabId() === tab.id;
            @let isSecondary = splitActive && tabs.splitTabId() === tab.id;
            <div
              role="tabpanel"
              [id]="'ks-tabpanel-' + tab.id"
              [attr.aria-labelledby]="'ks-tab-' + tab.id"
              class="absolute top-0 bottom-0"
              [class.hidden]="!isPrimary && !isSecondary"
              [style.left]="isSecondary ? 'calc(' + (tabs.splitRatio() * 100) + '% + 2px)' : '0'"
              [style.right]="isPrimary && splitActive ? 'auto' : '0'"
              [style.width]="isPrimary && splitActive ? 'calc(' + (tabs.splitRatio() * 100) + '% - 2px)' : null"
            >
              @if (tab.id === orchestratorTabId) {
                <app-orchestrator-chat
                  (openNewChat)="openSessionModal('chat', null)"
                  (openNewTerminal)="openSessionModal('terminal', null)"
                />
              } @else if (tab.taskId) {
                <!-- xterm + addons load as their own chunk, only once a terminal tab exists. -->
                @defer (on immediate) {
                  <app-xterm-pane [taskId]="tab.taskId" [visible]="isPrimary || isSecondary" />
                } @placeholder {
                  <div class="h-full w-full flex items-center justify-center bg-[#111214] text-[12px] text-[#86888f]" role="status">Loading terminal...</div>
                }
              }
            </div>
          }

          @if (splitActive) {
            <!-- Draggable Vertical Split Divider -->
            <div
              (pointerdown)="startSplitDrag($event)"
              (keydown)="onSplitKeydown($event)"
              title="Drag to resize split panes"
              role="separator"
              tabindex="0"
              aria-orientation="vertical"
              aria-label="Resize split panes"
              [attr.aria-valuenow]="Math.round(tabs.splitRatio() * 100)"
              aria-valuemin="25"
              aria-valuemax="75"
              class="absolute top-0 bottom-0 w-1 -ml-0.5 z-20 bg-[#26282e] hover:bg-[#4a668a] transition-colors cursor-col-resize"
              [style.left.%]="tabs.splitRatio() * 100"
            ></div>
            <button
              type="button"
              (click)="tabs.closeSplit()"
              title="Close split pane (⌘\)"
              aria-label="Close split pane"
              class="absolute top-1.5 right-2 z-20 rounded bg-[#1b1c20]/90 border border-[#2d3039] hover:bg-[#272a33] p-1 text-[#8e9098] hover:text-[#ececed] transition cursor-pointer"
            >
              <app-icon name="close" [size]="11" />
            </button>
          }
        </main>

        <!-- Desktop Right Inspector Pane (smooth width/opacity collapse) -->
        <aside
          id="ks-right-pane"
          aria-label="Inspector"
          [attr.inert]="rightOpen() ? null : ''"
          class="hidden md:block shrink-0 h-full overflow-hidden transition-[width,opacity] duration-200 ease-[cubic-bezier(0.16,1,0.3,1)]"
          [class.w-[300px]]="rightOpen()"
          [class.opacity-100]="rightOpen()"
          [class.border-l]="rightOpen()"
          [class.border-[#26282e]]="rightOpen()"
          [class.w-0]="!rightOpen()"
          [class.opacity-0]="!rightOpen()"
        >
          <div class="w-[300px] h-full">
            <app-telemetry-drawer [panel]="inspectorPanel()" [enabled]="rightOpen()" />
          </div>
        </aside>

        <!-- Mobile Bottom-Sheet Inspector (< 768px) -->
        @if (mobileTelemetryOpen()) {
          <div class="md:hidden fixed inset-0 z-40 flex flex-col justify-end" role="dialog" aria-modal="true" aria-label="Inspector">
            <div class="ks-backdrop flex-1" (click)="mobileTelemetryOpen.set(false)"></div>
            <div class="ks-drawer-bottom h-[68vh] border-t border-[#26282e] bg-[#16171a] shadow-2xl rounded-t-xl flex flex-col overflow-hidden z-10">
              <div class="flex items-center justify-around border-b border-[#26282e] py-1.5 text-[11px]">
                @for (p of inspectorPanels; track p.id) {
                  <button type="button" (click)="inspectorPanel.set(p.id)" [attr.aria-pressed]="inspectorPanel() === p.id" [class.text-[#ececed]]="inspectorPanel() === p.id" class="px-3 py-1 text-[#8e9098]">{{ p.label }}</button>
                }
              </div>
              <div class="flex-1 min-h-0">
                <app-telemetry-drawer [panel]="inspectorPanel()" />
              </div>
            </div>
          </div>
        }
      </div>

      <!-- Orca-style Bottom Status Bar (28px) -->
      <app-status-footer
        (openCommands)="commandPaletteOpen.set(true)"
        (openMachineDetails)="openMachineDetailsModal($event)"
      />

      <!-- Orca-style Floating Scratch Workspace -->
      <app-floating-scratch />

      <!-- Machine Details & Removal Dialog -->
      @if (inspectedWorker(); as iw) {
        <app-machine-detail-modal
          [worker]="iw"
          (close)="inspectedWorkerId.set(null)"
          (removed)="onMachineRemoved($event)"
          (startChat)="inspectedWorkerId.set(null); openSessionModal('chat', $event)"
          (startTerminal)="inspectedWorkerId.set(null); openSessionModal('terminal', $event)"
        />
      }

      <!-- Settings Modal (KON-0Q95Z) -->
      @if (settingsOpen()) {
        <app-settings-modal (close)="settingsOpen.set(false)" />
      }

      @if (newSpaceFor(); as req) {
        <app-new-space-modal [worker]="req.worker" (close)="newSpaceFor.set(null)" />
      }

      @if (sessionRequest(); as req) {
        <app-session-dispatch-modal [request]="req" (close)="sessionRequest.set(null)" />
      }

      @if (addMachineOpen()) {
        <app-add-machine-modal
          (close)="addMachineOpen.set(false)"
          (inspectMachine)="addMachineOpen.set(false); inspectedWorkerId.set($event)"
        />
      }

      @if (commandPaletteOpen()) {
        <app-command-palette (close)="commandPaletteOpen.set(false)" (command)="onPaletteCommand($event)" />
      }
    </div>
  `,
})
export class AppShellComponent implements OnInit, OnDestroy {
  @ViewChild('mainPane') mainPane?: ElementRef<HTMLElement>;

  readonly orchestratorTabId = ORCHESTRATOR_TAB_ID;
  protected readonly Math = Math;

  readonly auth = inject(AuthService);
  readonly fleet = inject(FleetStore);
  readonly tabs = inject(TabsStore);
  readonly socket = inject(KonsertSocketService);
  readonly notifications = inject(NotificationService);
  private readonly launcher = inject(SessionLauncherService);

  readonly leftOpen = signal(true);
  readonly rightOpen = signal(true);
  readonly inspectorPanel = signal<InspectorPanel>('agent');
  readonly mobileFleetOpen = signal(false);
  readonly mobileTelemetryOpen = signal(false);
  readonly inspectorPanels: ReadonlyArray<{ id: InspectorPanel; label: string; title: string; icon: string }> = [
    { id: 'agent', label: 'Agent', title: 'Agent Activity & History', icon: 'agent-history' },
    { id: 'git', label: 'Git', title: 'Git Changes', icon: 'git-branch' },
    { id: 'checks', label: 'Checks', title: 'Checks & Runtime', icon: 'checks' },
  ];

  // Modals: each is its own component, mounted while its signal is set.
  readonly sessionRequest = signal<SessionDispatchRequest | null>(null);
  readonly newSpaceFor = signal<{ worker: Worker | null } | null>(null);
  readonly addMachineOpen = signal(false);
  readonly commandPaletteOpen = signal(false);
  readonly settingsOpen = signal(false);
  readonly inspectedWorkerId = signal<string | null>(null);
  readonly inspectedWorker = computed(() => {
    const id = this.inspectedWorkerId();
    return id ? this.fleet.workersById()[id] ?? null : null;
  });

  readonly activeSpace = computed(() => {
    const sid = this.tabs.activeSpaceId();
    return sid ? this.fleet.spacesById()[sid] : undefined;
  });

  readonly isSplitVisible = computed(() => {
    const active = this.tabs.activeTab();
    const split = this.tabs.splitTab();
    return !!split && active?.kind === 'terminal' && split.kind === 'terminal';
  });

  constructor() {
    inject(HotkeyService).register((action) => this.onHotkey(action));
  }

  ngOnInit(): void {
    void this.fleet.refresh();
    void this.fleet.loadSettings();
    this.socket.connect();
  }

  ngOnDestroy(): void {
    // Also runs when a session expires: leaving the authenticated view stops the socket.
    this.socket.disconnect();
  }

  private onHotkey(action: HotkeyAction): boolean {
    switch (action.kind) {
      case 'palette':
        this.commandPaletteOpen.set(true);
        return true;
      case 'scratch':
        this.tabs.toggleScratch();
        return true;
      case 'select-tab': {
        const tab = this.tabs.visibleTabs()[action.index];
        if (!tab) return false;
        this.selectTab(tab.id);
        return true;
      }
      case 'close-tab': {
        const active = this.tabs.activeTab();
        if (!active || active.pinned) return false;
        this.tabs.closeTab(active.id);
        return true;
      }
      case 'toggle-split':
        void this.toggleSplitPane();
        return true;
      case 'escape':
        this.closeAllModals();
        return false;
    }
  }

  closeAllModals(): void {
    this.commandPaletteOpen.set(false);
    this.addMachineOpen.set(false);
    this.sessionRequest.set(null);
    this.newSpaceFor.set(null);
    this.settingsOpen.set(false);
    this.inspectedWorkerId.set(null);
  }

  onPaletteCommand(cmd: PaletteCommand): void {
    switch (cmd.kind) {
      case 'open-session':
        this.openSessionModal(cmd.sessionKind, cmd.worker);
        break;
      case 'open-worktree':
        this.handleWorktreeSelected({ repo: cmd.repo, worktree: cmd.worktree });
        break;
      case 'toggle-split':
        void this.toggleSplitPane();
        break;
      case 'add-machine':
        this.addMachineOpen.set(true);
        break;
      case 'settings':
        this.settingsOpen.set(true);
        break;
      case 'show-left':
        this.leftOpen.set(true);
        break;
      case 'toggle-left':
        this.leftOpen.update((v) => !v);
        break;
      case 'toggle-right':
        this.rightOpen.update((v) => !v);
        break;
    }
  }

  selectInspector(panel: InspectorPanel): void {
    if (this.rightOpen() && this.inspectorPanel() === panel) {
      this.rightOpen.set(false);
    } else {
      this.inspectorPanel.set(panel);
      this.rightOpen.set(true);
    }
  }

  selectTab(id: string): void {
    this.tabs.selectTab(id);
    this.notifications.markRead(id);
  }

  async toggleSplitPane(): Promise<void> {
    if (this.tabs.toggleSplit()) return;
    // No second terminal tab yet: spawn a shell in the active context, side-by-side.
    await this.launcher.openSplitShell(this.activeSpace()?.path);
  }

  /** Keyboard resizing for the split separator (arrows: 5%, Home/End: extremes). */
  onSplitKeydown(ev: KeyboardEvent): void {
    const step = ev.key === 'ArrowLeft' ? -0.05 : ev.key === 'ArrowRight' ? 0.05 : 0;
    if (step) {
      ev.preventDefault();
      this.tabs.setSplitRatio(this.tabs.splitRatio() + step);
    } else if (ev.key === 'Home' || ev.key === 'End') {
      ev.preventDefault();
      this.tabs.setSplitRatio(ev.key === 'Home' ? 0 : 1);
    }
  }

  startSplitDrag(ev: PointerEvent): void {
    const container = this.mainPane?.nativeElement;
    if (!container) return;
    ev.preventDefault();
    const rect = container.getBoundingClientRect();
    if (rect.width <= 0) return;

    const onMove = (moveEv: PointerEvent) => {
      this.tabs.setSplitRatio((moveEv.clientX - rect.left) / rect.width);
    };
    const onUp = () => {
      window.removeEventListener('pointermove', onMove);
      window.removeEventListener('pointerup', onUp);
    };
    window.addEventListener('pointermove', onMove);
    window.addEventListener('pointerup', onUp);
  }

  handleWorktreeSelected(ev: { repo: Repo; worktree: Worktree | null }): void {
    const worker = this.fleet.workersById()[ev.repo.worker_id] ?? null;
    this.sessionRequest.set({ kind: 'chat', worker, repo: ev.repo, worktree: ev.worktree });
  }

  openNewTabInActiveContext(): void {
    const sp = this.activeSpace();
    const w = sp ? this.fleet.workersById()[sp.worker_id] ?? null : null;
    this.openSessionModal('chat', w, sp ?? null);
  }

  openNewSpaceModal(worker: Worker | null): void {
    this.newSpaceFor.set({ worker });
  }

  openSessionModal(kind: SessionKind, worker: Worker | null, space: Space | null = null): void {
    this.sessionRequest.set({ kind, worker, space });
  }

  async handleSpaceSelected(ev: { space: Space; worker: Worker }): Promise<void> {
    const opened = await this.launcher.openSpace(ev.space, ev.worker);
    if (!opened) this.openSessionModal('terminal', ev.worker, ev.space);
  }

  openMachineDetailsModal(worker: Worker): void {
    this.inspectedWorkerId.set(worker.id);
  }

  onMachineRemoved(_workerId: string): void {
    this.inspectedWorkerId.set(null);
  }
}

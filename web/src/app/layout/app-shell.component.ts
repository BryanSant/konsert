import { Component, ElementRef, OnDestroy, OnInit, ViewChild, computed, effect, inject, signal, untracked } from '@angular/core';
import { Repo, SessionKind, Space, Worker, Worktree } from '../core/models/konsert.models';
import { AuthService } from '../core/services/auth.service';
import { KonsertSocketService } from '../core/services/konsert-socket.service';
import { NotificationService } from '../core/services/notification.service';
import { FleetStore } from '../core/state/fleet.store';
import { SetupStore } from '../core/state/setup.store';
import { ORCHESTRATOR_TAB_ID, TabsStore } from '../core/state/tabs.store';
import { ChatViewComponent } from '../features/chat/chat-view.component';
import { ReloginDialogComponent } from '../features/chat/relogin-dialog.component';
import { XtermPaneComponent } from '../features/terminal/xterm-pane.component';
import { IconComponent } from '../shared/icon.component';
import { TooltipDirective } from '../shared/tooltip.directive';
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
import { PauseEverythingComponent, PausedBannerComponent } from '../features/control/pause-everything.component';
import { ControlStore } from '../core/state/control.store';
import { InspectorPanel, TelemetryDrawerComponent } from './telemetry-drawer.component';

@Component({
  selector: 'app-shell',
  standalone: true,
  imports: [TooltipDirective, 
    IconComponent,
    FleetSidebarComponent,
    TelemetryDrawerComponent,
    StatusFooterComponent,
    FloatingScratchComponent,
    MachineDetailModalComponent,
    SettingsModalComponent,
    ChatViewComponent,
    ReloginDialogComponent,
    XtermPaneComponent,
    CommandPaletteComponent,
    AddMachineModalComponent,
    SessionDispatchModalComponent,
    NewSpaceModalComponent,
    TabStripComponent,
    PauseEverythingComponent,
    PausedBannerComponent,
  ],
  template: `
    <div class="flex flex-col h-screen w-screen overflow-hidden bg-[#111214] text-[#ececed]">
      <!-- Top Unified IDE Chrome Bar (44px) -->
      <header class="h-11 flex items-center justify-between bg-[var(--ks-bg-chrome)] shrink-0 select-none">
        <!-- Left Brand & Sidebar Toggle -->
        <div
          class="flex items-center gap-1.5 pl-3.5 pr-2.5 h-full shrink-0 transition-[width] duration-200 ease-[cubic-bezier(0.16,1,0.3,1)]"
          [class.md:w-[250px]]="leftOpen()"
        >
          <button
            type="button"
            (click)="mobileFleetOpen.set(!mobileFleetOpen())"
            aria-label="Toggle fleet sidebar"
            [attr.aria-expanded]="mobileFleetOpen()"
            class="ks-icon-btn md:hidden"
          >
            <app-icon name="sidebar-left" [size]="15" />
          </button>

          <img src="icon.svg" alt="Konsert" class="h-4 w-4 rounded-sm" />
          <h1 class="text-[13px] font-semibold tracking-tight text-[#ececed]">Konsert</h1>

          <div class="flex-1"></div>

          <button
            type="button"
            (click)="commandPaletteOpen.set(true)"
            ksTooltip="Search / Commands (⌘K)" aria-label="Search / Commands (⌘K)"
            class="ks-icon-btn hidden md:inline-flex"
          >
            <app-icon name="search" [size]="15" />
          </button>
          <button
            type="button"
            (click)="leftOpen.set(!leftOpen())"
            ksTooltip="Toggle left sidebar" aria-label="Toggle left sidebar"
            [attr.aria-expanded]="leftOpen()"
            aria-controls="ks-left-pane"
            class="ks-icon-btn hidden md:inline-flex"
          >
            <app-icon name="sidebar-left" [size]="15" />
          </button>
        </div>

        <!-- Center Minimal Herdr-style Tab Strip (scoped to active Space when selected) -->
        <app-tab-strip (newTab)="openNewTabInActiveContext()" (toggleSplit)="toggleSplitPane()" />

        <!-- Right Orca-style Inspector Tab Icons & Collapse Toggle -->
        <div class="flex items-center gap-0.5 pr-2.5 pl-2 h-full shrink-0">
          <!-- Pause everything: always visible (PRD §5.8a) -->
          <ks-pause-everything class="mr-1.5" />
          @for (p of inspectorPanels; track p.id) {
            <button
              type="button"
              (click)="selectInspector(p.id)"
              [ksTooltip]="p.title"
              [attr.aria-label]="p.title"
              [attr.aria-pressed]="rightOpen() && inspectorPanel() === p.id"
              [attr.data-testid]="'inspector-' + p.id"
              class="ks-icon-btn hidden md:inline-flex relative"
            >
              <app-icon [name]="p.icon" [size]="15" />
              @if (p.id === 'control' && control.attentionCount() > 0) {
                <span class="absolute -top-0.5 -right-0.5 min-w-[14px] h-[14px] rounded-full bg-[var(--ks-amber)] px-1 text-[9px] font-semibold leading-[14px] text-[#111214]">
                  {{ control.attentionCount() }}
                </span>
              }
            </button>
          }

          <button
            type="button"
            (click)="rightOpen.set(!rightOpen())"
            ksTooltip="Toggle right inspector" aria-label="Toggle right inspector"
            [attr.aria-expanded]="rightOpen()"
            aria-controls="ks-right-pane"
            class="ks-icon-btn hidden md:inline-flex ml-1.5"
          >
            <app-icon name="sidebar-right" [size]="15" />
          </button>

          <!-- Mobile Telemetry Toggle -->
          <button
            type="button"
            (click)="mobileTelemetryOpen.set(!mobileTelemetryOpen())"
            aria-label="Toggle inspector"
            [attr.aria-expanded]="mobileTelemetryOpen()"
            class="ks-icon-btn md:hidden"
          >
            <app-icon name="sidebar-right" [size]="15" />
          </button>
        </div>
      </header>

      <ks-paused-banner />

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
          [class.border-white/[0.05]]="leftOpen()"
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
            <div class="ks-drawer-left w-[82vw] max-w-[290px] h-full shadow-2xl z-10">
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
                <app-chat-view (openNewTerminal)="openSessionModal('terminal', null)" />
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
              ksTooltip="Drag to resize split panes"
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
              ksTooltip="Close split pane (⌘\)"
              aria-label="Close split pane"
              class="ks-icon-btn ks-icon-btn-sm absolute top-2 right-2 z-20 bg-[#1b1c20]/90 shadow-[var(--ks-shadow-raised)]"
            >
              <app-icon name="close" [size]="13" />
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
          [class.border-white/[0.05]]="rightOpen()"
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
            <div class="ks-drawer-bottom h-[68vh] bg-[#16171a] shadow-2xl rounded-t-xl flex flex-col overflow-hidden z-10">
              <div class="flex items-center justify-around py-2 text-[12px]">
                @for (p of inspectorPanels; track p.id) {
                  <button type="button" (click)="inspectorPanel.set(p.id)" [attr.aria-pressed]="inspectorPanel() === p.id" [class.text-[#ececed]]="inspectorPanel() === p.id" class="ks-btn ks-btn-sm">{{ p.label }}</button>
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
      <ks-relogin-dialog />

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
          [firstRun]="setup.needsSetup()"
          (close)="closeAddMachine()"
          (inspectMachine)="closeAddMachine(); inspectedWorkerId.set($event)"
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
  readonly setup = inject(SetupStore);
  readonly control = inject(ControlStore);

  readonly leftOpen = signal(true);
  readonly rightOpen = signal(true);
  readonly inspectorPanel = signal<InspectorPanel>('agent');
  readonly mobileFleetOpen = signal(false);
  readonly mobileTelemetryOpen = signal(false);
  readonly inspectorPanels: ReadonlyArray<{ id: InspectorPanel; label: string; title: string; icon: string }> = [
    { id: 'agent', label: 'Agent', title: 'Agent Activity & History', icon: 'agent-history' },
    { id: 'git', label: 'Git', title: 'Git Changes', icon: 'git-branch' },
    { id: 'checks', label: 'Checks', title: 'Checks & Runtime', icon: 'checks' },
    { id: 'control', label: 'Actions', title: 'Your actions, holds & requests', icon: 'hand' },
    { id: 'audit', label: 'Audit', title: 'Audit Log', icon: 'shield' },
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
    effect(() => {
      const tasks = this.fleet.tasks();
      untracked(() => this.tabs.closeExitedShells(tasks));
    });
  }

  ngOnInit(): void {
    // Once the first snapshot is in, close restored tabs whose task is gone.
    void this.fleet
      .refresh()
      .then(() => this.tabs.pruneMissingTasks(this.fleet.tasks()))
      .catch(() => undefined);
    void this.fleet.loadSettings();
    void this.control.load();
    this.socket.connect();
    // First run (persisted by the hub): open Add Machine with this machine on top.
    void this.setup.load().then((s) => {
      if (s?.needs_setup) this.addMachineOpen.set(true);
    });
  }

  /** Closing Add Machine also dismisses first-run setup, so it does not reopen on load. */
  closeAddMachine(): void {
    this.addMachineOpen.set(false);
    void this.setup.dismiss();
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
    if (this.addMachineOpen()) this.closeAddMachine();
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

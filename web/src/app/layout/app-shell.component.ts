import { Component, ElementRef, HostListener, OnDestroy, OnInit, ViewChild, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { PermissionMode, Repo, SSHHost, SessionKind, Space, Worker, Worktree } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { AuthService } from '../core/services/auth.service';
import { KonsertSocketService } from '../core/services/konsert-socket.service';
import { NotificationService } from '../core/services/notification.service';
import { FleetStore, basenamePath, guessModelForAgent } from '../core/state/fleet.store';
import { ORCHESTRATOR_TAB_ID, TabsStore } from '../core/state/tabs.store';
import { OrchestratorChatComponent } from '../features/orchestrator/orchestrator-chat.component';
import { XtermPaneComponent } from '../features/terminal/xterm-pane.component';
import { IconComponent } from '../shared/icon.component';
import { RemoteDirPickerComponent } from '../shared/remote-dir-picker.component';
import { FleetSidebarComponent } from './fleet-sidebar.component';
import { FloatingScratchComponent } from './floating-scratch.component';
import { MachineDetailModalComponent } from './machine-detail-modal.component';
import { SettingsModalComponent } from './settings-modal.component';
import { StatusFooterComponent } from './status-footer.component';
import { InspectorPanel, TelemetryDrawerComponent } from './telemetry-drawer.component';

interface PaletteItem {
  id: string;
  section: 'sessions' | 'taxonomy' | 'machines' | 'actions';
  label: string;
  sublabel?: string;
  icon: string;
  dotOnline?: boolean;
  pill?: string;
  badge?: string;
  action: () => void;
}

@Component({
  selector: 'app-shell',
  standalone: true,
  imports: [
    FormsModule,
    IconComponent,
    RemoteDirPickerComponent,
    FleetSidebarComponent,
    TelemetryDrawerComponent,
    StatusFooterComponent,
    FloatingScratchComponent,
    MachineDetailModalComponent,
    SettingsModalComponent,
    OrchestratorChatComponent,
    XtermPaneComponent,
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
            class="md:hidden rounded p-1 text-[#8e9098] hover:text-[#ececed] hover:bg-[#23252b] active:scale-95 transition cursor-pointer"
          >
            <app-icon name="sidebar-left" [size]="15" />
          </button>

          <img src="icon.svg" alt="Konsert" class="h-4 w-4 rounded-sm" />
          <span class="text-[13px] font-semibold tracking-tight text-[#ececed]">Konsert</span>

          <div class="flex-1"></div>

          <button
            type="button"
            (click)="openCommandPalette()"
            title="Search / Commands (⌘K)"
            class="hidden md:inline-flex rounded p-1 text-[#787a82] hover:text-[#ececed] hover:bg-[#23252b] active:scale-95 transition cursor-pointer"
          >
            <app-icon name="search" [size]="14" />
          </button>
          <button
            type="button"
            (click)="leftOpen.set(!leftOpen())"
            title="Toggle left sidebar"
            class="hidden md:inline-flex rounded p-1 text-[#787a82] hover:text-[#ececed] hover:bg-[#23252b] active:scale-95 transition cursor-pointer"
          >
            <app-icon name="sidebar-left" [size]="14" />
          </button>
        </div>

        <!-- Center Minimal Herdr-style Tab Strip (scoped to active Space when selected) -->
        <div class="flex-1 flex items-center gap-1 px-2 h-full overflow-x-auto min-w-0">
          @if (activeSpace(); as sp) {
            <span class="inline-flex items-center gap-1.5 rounded bg-[#1d2026] border border-[#2c2f38] px-2 py-0.5 text-[11px] text-[#b4b6be] shrink-0 mr-1">
              <span class="h-3.5 w-3.5 rounded flex items-center justify-center text-[9px] font-semibold bg-[#1f8a8c]/85 text-white">
                {{ sp.name === '~' ? '~' : sp.name.slice(0, 1).toUpperCase() }}
              </span>
              <span class="font-medium text-[#e4e4e7]">{{ sp.name }}</span>
            </span>
          }

          @for (tab of tabs.visibleTabs(); track tab.id) {
            <div
              (click)="selectTab(tab.id)"
              class="group flex items-center gap-1.5 px-2.5 py-1 text-[12px] rounded-md cursor-pointer shrink-0 transition-all duration-150"
              [class.bg-[#24262d]]="tabs.activeTabId() === tab.id || tabs.splitTabId() === tab.id"
              [class.text-[#f4f4f5]]="tabs.activeTabId() === tab.id"
              [class.text-[#b4b6be]]="tabs.splitTabId() === tab.id && tabs.activeTabId() !== tab.id"
              [class.text-[#8e9098]]="tabs.activeTabId() !== tab.id && tabs.splitTabId() !== tab.id"
              [class.hover:bg-[#1e2026]]="tabs.activeTabId() !== tab.id && tabs.splitTabId() !== tab.id"
              [class.hover:text-[#d4d4d8]]="tabs.activeTabId() !== tab.id && tabs.splitTabId() !== tab.id"
            >
              @if (tab.kind === 'orchestrator') {
                <app-icon name="orchestrator" [size]="12" class="text-[#60a5fa]" />
              } @else {
                <app-icon [name]="tab.agent || 'shell'" [size]="12" />
              }
              <span class="max-w-[150px] truncate">{{ tab.title }}</span>
              @if (notifications.isUnread(tab.id) && tabs.activeTabId() !== tab.id) {
                <span class="h-1.5 w-1.5 rounded-full bg-[#d9a043] shrink-0" title="Attention needed"></span>
              }
              @if (tabs.splitTabId() === tab.id) {
                <span class="rounded bg-[#1a1c22] border border-[#32353f] px-1 text-[9px] font-mono-ide text-[#8e9098]">split</span>
              }
              @if (!tab.pinned) {
                <button
                  type="button"
                  (click)="$event.stopPropagation(); tabs.closeTab(tab.id)"
                  class="rounded hover:bg-white/10 p-0.5 text-[#686a72] hover:text-[#ececed] transition cursor-pointer"
                >
                  <app-icon name="close" [size]="11" />
                </button>
              }
            </div>
          }

          <button
            type="button"
            (click)="openNewTabInActiveContext()"
            [title]="activeSpace() ? 'New session in ' + activeSpace()!.name : 'New chat or terminal session'"
            class="rounded-md p-1 text-[#787a82] hover:text-[#ececed] hover:bg-[#23252b] active:scale-95 transition cursor-pointer shrink-0"
          >
            <app-icon name="plus" [size]="14" />
          </button>

          @if (tabs.activeTab()?.kind === 'terminal' || tabs.splitTabId()) {
            <button
              type="button"
              (click)="toggleSplitPane()"
              [title]="tabs.splitTabId() ? 'Close split pane (⌘\\)' : 'Split terminal side-by-side (⌘\\)'"
              class="hidden md:inline-flex rounded-md p-1 active:scale-95 transition cursor-pointer shrink-0"
              [class.bg-[#24262d]]="!!tabs.splitTabId()"
              [class.text-[#ececed]]="!!tabs.splitTabId()"
              [class.text-[#787a82]]="!tabs.splitTabId()"
              [class.hover:text-[#ececed]]="!tabs.splitTabId()"
              [class.hover:bg-[#23252b]]="!tabs.splitTabId()"
            >
              <app-icon name="split" [size]="14" />
            </button>
          }
        </div>

        <!-- Right Orca-style Inspector Tab Icons & Collapse Toggle -->
        <div class="flex items-center gap-1 pr-2.5 pl-2 h-full border-l border-[#26282e] shrink-0">
          <button
            type="button"
            (click)="selectInspector('agent')"
            title="Agent Activity & History"
            class="hidden md:inline-flex rounded p-1.5 active:scale-95 transition cursor-pointer"
            [class.bg-[#24262d]]="rightOpen() && inspectorPanel() === 'agent'"
            [class.text-[#ececed]]="rightOpen() && inspectorPanel() === 'agent'"
            [class.text-[#787a82]]="!rightOpen() || inspectorPanel() !== 'agent'"
            [class.hover:text-[#ececed]]="true"
          >
            <app-icon name="agent-history" [size]="14" />
          </button>
          <button
            type="button"
            (click)="selectInspector('git')"
            title="Git Changes"
            class="hidden md:inline-flex rounded p-1.5 active:scale-95 transition cursor-pointer"
            [class.bg-[#24262d]]="rightOpen() && inspectorPanel() === 'git'"
            [class.text-[#ececed]]="rightOpen() && inspectorPanel() === 'git'"
            [class.text-[#787a82]]="!rightOpen() || inspectorPanel() !== 'git'"
            [class.hover:text-[#ececed]]="true"
          >
            <app-icon name="git-branch" [size]="14" />
          </button>
          <button
            type="button"
            (click)="selectInspector('checks')"
            title="Checks & Runtime"
            class="hidden md:inline-flex rounded p-1.5 active:scale-95 transition cursor-pointer"
            [class.bg-[#24262d]]="rightOpen() && inspectorPanel() === 'checks'"
            [class.text-[#ececed]]="rightOpen() && inspectorPanel() === 'checks'"
            [class.text-[#787a82]]="!rightOpen() || inspectorPanel() !== 'checks'"
            [class.hover:text-[#ececed]]="true"
          >
            <app-icon name="checks" [size]="14" />
          </button>

          <button
            type="button"
            (click)="rightOpen.set(!rightOpen())"
            title="Toggle right inspector"
            class="hidden md:inline-flex rounded p-1.5 text-[#787a82] hover:text-[#ececed] hover:bg-[#23252b] active:scale-95 transition cursor-pointer ml-1"
          >
            <app-icon name="sidebar-right" [size]="14" />
          </button>

          <!-- Mobile Telemetry Toggle -->
          <button
            type="button"
            (click)="mobileTelemetryOpen.set(!mobileTelemetryOpen())"
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
              (openAddMachine)="openAddMachineModal()"
              (openSettings)="settingsOpen.set(true)"
            />
          </div>
        </aside>

        <!-- Mobile Slide-Over Fleet Drawer (< 768px) -->
        @if (mobileFleetOpen()) {
          <div class="md:hidden fixed inset-0 z-40 flex">
            <div class="ks-drawer-left w-[82vw] max-w-[290px] h-full border-r border-[#26282e] shadow-2xl z-10">
              <app-fleet-sidebar
                (taskSelected)="mobileFleetOpen.set(false)"
                (openNewSpace)="mobileFleetOpen.set(false); openNewSpaceModal($event)"
                (spaceSelected)="mobileFleetOpen.set(false); handleSpaceSelected($event)"
                (worktreeSelected)="mobileFleetOpen.set(false); handleWorktreeSelected($event)"
                (openMachineDetails)="mobileFleetOpen.set(false); openMachineDetailsModal($event)"
                (openAddMachine)="mobileFleetOpen.set(false); openAddMachineModal()"
                (openSettings)="mobileFleetOpen.set(false); settingsOpen.set(true)"
              />
            </div>
            <div class="ks-backdrop flex-1" (click)="mobileFleetOpen.set(false)"></div>
          </div>
        }

        <!-- Center Active Pane Container (Supports Side-by-Side Split Terminal Panes!) -->
        <main #mainPane class="flex-1 min-w-0 h-full relative overflow-hidden bg-[#111214]">
          @let splitActive = isSplitVisible();
          @for (tab of tabs.openTabs(); track tab.id) {
            @let isPrimary = tabs.activeTabId() === tab.id;
            @let isSecondary = splitActive && tabs.splitTabId() === tab.id;
            <div
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
                <app-xterm-pane [taskId]="tab.taskId" [visible]="isPrimary || isSecondary" />
              }
            </div>
          }

          @if (splitActive) {
            <!-- Draggable Vertical Split Divider -->
            <div
              (pointerdown)="startSplitDrag($event)"
              title="Drag to resize split panes"
              class="absolute top-0 bottom-0 w-1 -ml-0.5 z-20 bg-[#26282e] hover:bg-[#4a668a] transition-colors cursor-col-resize"
              [style.left.%]="tabs.splitRatio() * 100"
            ></div>
            <button
              type="button"
              (click)="tabs.closeSplit()"
              title="Close split pane (⌘\)"
              class="absolute top-1.5 right-2 z-20 rounded bg-[#1b1c20]/90 border border-[#2d3039] hover:bg-[#272a33] p-1 text-[#8e9098] hover:text-[#ececed] transition cursor-pointer"
            >
              <app-icon name="close" [size]="11" />
            </button>
          }
        </main>

        <!-- Desktop Right Inspector Pane (smooth width/opacity collapse) -->
        <aside
          class="hidden md:block shrink-0 h-full overflow-hidden transition-[width,opacity] duration-200 ease-[cubic-bezier(0.16,1,0.3,1)]"
          [class.w-[300px]]="rightOpen()"
          [class.opacity-100]="rightOpen()"
          [class.border-l]="rightOpen()"
          [class.border-[#26282e]]="rightOpen()"
          [class.w-0]="!rightOpen()"
          [class.opacity-0]="!rightOpen()"
        >
          <div class="w-[300px] h-full">
            <app-telemetry-drawer [panel]="inspectorPanel()" />
          </div>
        </aside>

        <!-- Mobile Bottom-Sheet Inspector (< 768px) -->
        @if (mobileTelemetryOpen()) {
          <div class="md:hidden fixed inset-0 z-40 flex flex-col justify-end">
            <div class="ks-backdrop flex-1" (click)="mobileTelemetryOpen.set(false)"></div>
            <div class="ks-drawer-bottom h-[68vh] border-t border-[#26282e] bg-[#16171a] shadow-2xl rounded-t-xl flex flex-col overflow-hidden z-10">
              <div class="flex items-center justify-around border-b border-[#26282e] py-1.5 text-[11px]">
                <button type="button" (click)="inspectorPanel.set('agent')" [class.text-[#ececed]]="inspectorPanel() === 'agent'" class="px-3 py-1 text-[#8e9098]">Agent</button>
                <button type="button" (click)="inspectorPanel.set('git')" [class.text-[#ececed]]="inspectorPanel() === 'git'" class="px-3 py-1 text-[#8e9098]">Git</button>
                <button type="button" (click)="inspectorPanel.set('checks')" [class.text-[#ececed]]="inspectorPanel() === 'checks'" class="px-3 py-1 text-[#8e9098]">Checks</button>
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
        (openCommands)="openCommandPalette()"
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

      <!-- NEW SPACE MODAL (Herdr-style per-machine persisted Space) -->
      @if (newSpaceModalOpen()) {
        <div class="ks-backdrop fixed inset-0 z-50 flex items-center justify-center px-4" (click)="newSpaceModalOpen.set(false)">
          <div class="ks-modal w-full max-w-lg rounded-xl border border-[#2d3038] bg-[#1b1c20] p-5 space-y-4" (click)="$event.stopPropagation()">
            <div class="flex items-center justify-between">
              <div>
                <h3 class="text-[14px] font-semibold text-[#ececed]">New Space</h3>
                <p class="text-[11px] text-[#8e9098]">Save a folder or worktree on a machine with multi-tab sessions.</p>
              </div>
              <button type="button" (click)="newSpaceModalOpen.set(false)" class="ks-kbd cursor-pointer">Esc</button>
            </div>

            <form (ngSubmit)="submitNewSpace()" class="space-y-3 text-[12px]">
              <div>
                <label class="block text-[11px] text-[#8e9098] mb-1">Machine</label>
                <select
                  name="spWorker"
                  [ngModel]="newSpaceWorkerId()"
                  (ngModelChange)="newSpaceWorkerId.set($event)"
                  class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed]"
                >
                  @for (w of fleet.workers(); track w.id) {
                    <option [value]="w.id">{{ w.name }} ({{ w.status }})</option>
                  }
                </select>
              </div>

              <div>
                <label class="block text-[11px] text-[#8e9098] mb-1">Directory Path</label>
                <app-remote-dir-picker
                  [workerId]="newSpaceWorkerId()"
                  [value]="newSpacePath()"
                  (valueChange)="newSpacePath.set($event)"
                  inputName="spPath"
                  placeholder="~ or /home/user/src/my-repo"
                />
              </div>

              <div class="grid grid-cols-2 gap-3">
                <div>
                  <label class="block text-[11px] text-[#8e9098] mb-1">Space Name (optional)</label>
                  <input
                    type="text"
                    name="spName"
                    [ngModel]="newSpaceName()"
                    (ngModelChange)="newSpaceName.set($event)"
                    placeholder="auto from folder"
                    class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed]"
                  />
                </div>
                <div>
                  <label class="block text-[11px] text-[#8e9098] mb-1">First Tab</label>
                  <select
                    name="spAgent"
                    [ngModel]="newSpaceInitialAgent()"
                    (ngModelChange)="newSpaceInitialAgent.set($event)"
                    class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed]"
                  >
                    <option value="shell">shell (zsh / bash)</option>
                    <option value="claude">claude</option>
                    <option value="agy">agy</option>
                    <option value="grok">grok</option>
                    <option value="mock">mock</option>
                  </select>
                </div>
              </div>

              @if (newSpaceError()) {
                <div class="rounded-md border border-[#e05d5d]/30 bg-[#e05d5d]/10 px-3 py-2 text-[#f09595]">
                  {{ newSpaceError() }}
                </div>
              }

              <div class="flex justify-end gap-2 pt-1">
                <button type="button" (click)="newSpaceModalOpen.set(false)" class="rounded-md border border-[#2c2e36] px-3 py-1.5 text-[#a1a3ab] cursor-pointer">
                  Cancel
                </button>
                <button
                  type="submit"
                  [disabled]="creatingSpace() || !newSpaceWorkerId() || !newSpacePath().trim()"
                  class="rounded-md bg-[#ececed] hover:bg-white disabled:opacity-40 px-4 py-1.5 font-medium text-[#111214] cursor-pointer"
                >
                  {{ creatingSpace() ? 'Creating...' : 'Create Space' }}
                </button>
              </div>
            </form>
          </div>
        </div>
      }

      <!-- UNIFIED SESSION MODAL: CHAT SESSION vs TERMINAL (Machine/Agent/Model Cascade + Remote Dir Picker) -->
      @if (dispatchModalOpen()) {
        <div class="ks-backdrop fixed inset-0 z-50 flex items-center justify-center px-4" (click)="dispatchModalOpen.set(false)">
          <div class="ks-modal w-full max-w-2xl rounded-xl border border-[#2d3038] bg-[#1b1c20] p-5 space-y-4" (click)="$event.stopPropagation()">
            <div class="flex items-center justify-between">
              <div class="inline-flex rounded-lg bg-[#111214] p-0.5 border border-[#26282e] text-[12px]">
                <button
                  type="button"
                  (click)="switchSessionKind('chat')"
                  class="rounded-md px-3 py-1 font-medium transition cursor-pointer"
                  [class.bg-[#262830]]="sessionKind() === 'chat'"
                  [class.text-[#ececed]]="sessionKind() === 'chat'"
                  [class.text-[#787a82]]="sessionKind() !== 'chat'"
                >
                  Chat Session
                </button>
                <button
                  type="button"
                  (click)="switchSessionKind('terminal')"
                  class="rounded-md px-3 py-1 font-medium transition cursor-pointer"
                  [class.bg-[#262830]]="sessionKind() === 'terminal'"
                  [class.text-[#ececed]]="sessionKind() === 'terminal'"
                  [class.text-[#787a82]]="sessionKind() !== 'terminal'"
                >
                  Terminal
                </button>
              </div>

              <button type="button" (click)="dispatchModalOpen.set(false)" class="text-[#787a82] hover:text-[#ececed] transition cursor-pointer">
                <app-icon name="close" [size]="15" />
              </button>
            </div>

            <form (ngSubmit)="submitDispatch()" class="space-y-3 text-[12px]">
              @if (sessionKind() === 'chat') {
                <div class="grid grid-cols-1 sm:grid-cols-3 gap-3">
                  <div>
                    <div class="flex items-center justify-between mb-1">
                      <label class="text-[11px] text-[#8e9098]">Machine</label>
                      @if (machineSourceHint()) {
                        <span class="text-[10px] text-[#686a72]">{{ machineSourceHint() }}</span>
                      }
                    </div>
                    <select
                      name="worker"
                      [ngModel]="dispatchWorkerId()"
                      (ngModelChange)="onSessionMachineChange($event)"
                      class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed] focus:border-[#4a668a] focus:outline-none"
                    >
                      @if (fleet.workers().length === 0) {
                        <option value="">localhost (no daemon connected)</option>
                      }
                      @for (w of fleet.workers(); track w.id) {
                        <option [value]="w.id">{{ w.name }} ({{ w.status }})</option>
                      }
                    </select>
                  </div>

                  <div>
                    <div class="flex items-center justify-between mb-1">
                      <label class="text-[11px] text-[#8e9098]">Agent</label>
                      @if (agentSourceHint()) {
                        <span class="text-[10px] text-[#686a72]">{{ agentSourceHint() }}</span>
                      }
                    </div>
                    <select
                      name="agent"
                      [ngModel]="dispatchAgent()"
                      (ngModelChange)="onSessionAgentChange($event)"
                      class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed] focus:border-[#4a668a] focus:outline-none"
                    >
                      @for (a of chatAgentOptions(); track a) {
                        <option [value]="a">{{ a }}</option>
                      }
                    </select>
                  </div>

                  <div>
                    <div class="flex items-center justify-between mb-1">
                      <label class="text-[11px] text-[#8e9098]">Model</label>
                      @if (modelSourceHint()) {
                        <span class="text-[10px] text-[#686a72]">{{ modelSourceHint() }}</span>
                      }
                    </div>
                    <input
                      type="text"
                      name="sessionModel"
                      list="ks-model-suggestions"
                      [ngModel]="dispatchModel()"
                      (ngModelChange)="onSessionModelChange($event)"
                      placeholder="Auto"
                      class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 font-mono-ide text-[#ececed] focus:border-[#4a668a] focus:outline-none"
                    />
                    <datalist id="ks-model-suggestions">
                      @for (m of modelSuggestions(); track m) {
                        <option [value]="m"></option>
                      }
                    </datalist>
                  </div>
                </div>
              } @else {
                <div>
                  <label class="block text-[11px] text-[#8e9098] mb-1">Machine</label>
                  <select
                    name="worker"
                    [ngModel]="dispatchWorkerId()"
                    (ngModelChange)="onSessionMachineChange($event)"
                    class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed] focus:border-[#4a668a] focus:outline-none"
                  >
                    @if (fleet.workers().length === 0) {
                      <option value="">localhost (no daemon connected)</option>
                    }
                    @for (w of fleet.workers(); track w.id) {
                      <option [value]="w.id">{{ w.name }} ({{ w.status }})</option>
                    }
                  </select>
                </div>
              }

              <div>
                <label class="block text-[11px] text-[#8e9098] mb-1">Workspace Directory</label>
                <app-remote-dir-picker
                  [workerId]="dispatchWorkerId()"
                  [value]="dispatchRepo()"
                  (valueChange)="dispatchRepo.set($event)"
                  inputName="repo"
                  placeholder="~ or /path/to/repo"
                />
              </div>

              @if (sessionKind() === 'chat') {
                <div>
                  <label class="block text-[11px] text-[#8e9098] mb-1">Initial Prompt (optional)</label>
                  <textarea
                    rows="3"
                    name="prompt"
                    [ngModel]="dispatchPrompt()"
                    (ngModelChange)="dispatchPrompt.set($event)"
                    placeholder="Ask the agent anything, or leave blank to start an interactive session..."
                    class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed] focus:border-[#4a668a] focus:outline-none"
                  ></textarea>
                </div>
              }

              <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
                <div>
                  <label class="block text-[11px] text-[#8e9098] mb-1">Git Branch (optional worktree)</label>
                  <input
                    type="text"
                    name="branch"
                    [ngModel]="dispatchBranch()"
                    (ngModelChange)="dispatchBranch.set($event)"
                    placeholder="feat/my-branch"
                    class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 font-mono-ide text-[#ececed] focus:border-[#4a668a] focus:outline-none"
                  />
                </div>
                <div>
                  <label class="block text-[11px] text-[#8e9098] mb-1">Permission Mode</label>
                  <select
                    name="perm"
                    [ngModel]="dispatchPerm()"
                    (ngModelChange)="dispatchPerm.set($event)"
                    class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed]"
                  >
                    <option value="full_access">Full Access (Default)</option>
                    <option value="sandboxed">Sandboxed (Opt-In)</option>
                  </select>
                </div>
              </div>

              @if (dispatchError()) {
                <div class="rounded-md border border-[#e05d5d]/30 bg-[#e05d5d]/10 px-3 py-2 text-[#f09595]">
                  {{ dispatchError() }}
                </div>
              }

              <div class="flex justify-end gap-2 pt-1">
                <button
                  type="button"
                  (click)="dispatchModalOpen.set(false)"
                  class="rounded-md border border-[#2c2e36] px-3 py-1.5 text-[#a1a3ab] hover:text-[#ececed] transition cursor-pointer"
                >
                  Cancel
                </button>
                <button
                  type="submit"
                  [disabled]="dispatching() || !canSubmitDispatch()"
                  class="rounded-md bg-[#ececed] hover:bg-white active:scale-[0.98] disabled:opacity-40 px-4 py-1.5 font-medium text-[#111214] transition cursor-pointer"
                >
                  {{ dispatching() ? 'Starting...' : sessionKind() === 'chat' ? 'Start Chat' : 'Open Terminal' }}
                </button>
              </div>
            </form>
          </div>
        </div>
      }

      <!-- HERDR-STYLE + ADD MACHINE MODAL (SSH Config Discovery + 1-Click Bootstrap + Manual Fallback) -->
      @if (addMachineOpen()) {
        <div class="ks-backdrop fixed inset-0 z-50 flex items-center justify-center px-4" (click)="addMachineOpen.set(false)">
          <div class="ks-modal w-full max-w-[540px] rounded-xl border border-[#2d3038] bg-[#1b1c20] p-5 space-y-4" (click)="$event.stopPropagation()">
            <div class="flex items-start justify-between gap-3">
              <div>
                <h3 class="text-[15px] font-semibold text-[#ececed]">Add a machine</h3>
                <p class="text-[12px] text-[#8e9098] mt-1 leading-relaxed">
                  A VPS, a Mac mini, a box at home: Konsert reaches it over ssh, and agents there keep running while your laptop sleeps.
                </p>
              </div>
              <button
                type="button"
                (click)="addMachineOpen.set(false)"
                class="text-[11px] text-[#6d7078] hover:text-[#ececed] transition cursor-pointer shrink-0 pt-0.5"
              >
                Esc
              </button>
            </div>

            <!-- From your ssh config -->
            <div class="space-y-2">
              <div class="text-[11px] text-[#787a82]">From your ssh config</div>

              @if (sshHostsLoading()) {
                <div class="py-6 text-center text-[12px] text-[#6d7078]">Reading ~/.ssh/config...</div>
              } @else if (sshHosts().length === 0) {
                <div class="rounded-lg border border-[#26282f] bg-[#151619] px-3 py-3 text-[12px] text-[#6d7078]">
                  No hosts found in <code class="font-mono-ide text-[#9a9ca4]">~/.ssh/config</code>. Enter an SSH target below or use the manual command.
                </div>
              } @else {
                <div class="max-h-[290px] overflow-y-auto -mx-1 px-1 space-y-1">
                  @for (host of sshHosts(); track host.alias) {
                    <div class="flex items-center justify-between gap-3 rounded-lg px-2 py-1.5 hover:bg-[#22242a]/80 transition">
                      <div class="flex items-center gap-2.5 min-w-0">
                        <app-icon name="server" [size]="14" class="text-[#787a82]" />
                        <span class="text-[12.5px] font-medium text-[#e4e4e7] shrink-0">{{ host.alias }}</span>
                        <span class="text-[12px] text-[#6d7078] truncate">{{ host.display }}</span>
                      </div>

                      <div class="shrink-0">
                        @if (host.added) {
                          <button
                            type="button"
                            (click)="inspectSSHHostMachine(host)"
                            class="inline-flex items-center gap-1.5 rounded-md border border-transparent hover:border-[#32353f] hover:bg-[#24262d] px-2 py-1 text-[11px] text-[#8e9098] hover:text-[#ececed] transition cursor-pointer"
                            title="View machine details or remove"
                          >
                            <app-icon name="check-circle" [size]="13" />
                            <span>Added · Details</span>
                          </button>
                        } @else if (bootstrappingTarget() === host.alias) {
                          <span class="inline-flex items-center gap-1.5 rounded-md border border-[#32353f] bg-[#24262d] px-2.5 py-1 text-[11px] text-[#c8cad0]">
                            <span class="h-2.5 w-2.5 rounded-full border-[1.5px] border-[#60a5fa] border-t-transparent ks-ring-spin"></span>
                            <span>{{ sshStageLabel() || 'Connecting...' }}</span>
                          </span>
                        } @else {
                          <button
                            type="button"
                            [disabled]="!!bootstrappingTarget()"
                            (click)="bootstrapSSHHost(host.alias)"
                            class="rounded-md border border-[#32353f] bg-[#24262d] hover:bg-[#2d3039] disabled:opacity-45 px-3 py-1 text-[11px] font-medium text-[#e4e4e7] transition cursor-pointer"
                          >
                            Add
                          </button>
                        }
                      </div>
                    </div>
                  }
                </div>
              }
            </div>

            <!-- Or type how ssh reaches it -->
            <div class="pt-1 space-y-1.5">
              <div class="text-[11px] text-[#787a82]">Or type how ssh reaches it</div>
              <form (ngSubmit)="bootstrapCustomSSH()" class="flex items-center gap-2">
                <input
                  type="text"
                  name="customSshTarget"
                  [ngModel]="customSshTarget()"
                  (ngModelChange)="customSshTarget.set($event)"
                  placeholder="user@host"
                  autocomplete="off"
                  spellcheck="false"
                  class="flex-1 rounded-lg border border-[#383c48] focus:border-[#4a668a] bg-[#141518] px-3 py-2 text-[12.5px] text-[#ececed] placeholder-[#5e6068] focus:outline-none transition"
                />
                <button
                  type="submit"
                  [disabled]="!customSshTarget().trim() || !!bootstrappingTarget()"
                  class="rounded-lg border border-[#32353f] bg-[#2a2c34] hover:bg-[#343741] disabled:opacity-45 px-4 py-2 text-[12px] font-medium text-[#e4e4e7] transition cursor-pointer shrink-0"
                >
                  {{ bootstrappingTarget() && bootstrappingTarget() === customSshTarget().trim() ? 'Adding...' : 'Add' }}
                </button>
              </form>
            </div>

            @if (bootstrappingTarget() && sshStageDetail()) {
              <div class="rounded-lg border border-[#2d3039] bg-[#141518] px-3 py-2 flex items-center gap-2 text-[11.5px] text-[#b4b6be]">
                <span class="h-2.5 w-2.5 rounded-full border-[1.5px] border-[#60a5fa] border-t-transparent ks-ring-spin shrink-0"></span>
                <span class="font-medium text-[#e4e4e7]">{{ sshStageLabel() }}:</span>
                <span class="truncate">{{ sshStageDetail() }}</span>
              </div>
            }
            @if (sshStatusMsg()) {
              <div class="rounded-lg border border-[#69b879]/30 bg-[#69b879]/10 px-3 py-2 text-[11.5px] text-[#98dba5]">
                {{ sshStatusMsg() }}
              </div>
            }
            @if (sshErrorMsg()) {
              <div class="rounded-lg border border-[#e05d5d]/30 bg-[#e05d5d]/10 px-3 py-2 text-[11.5px] text-[#f09595] break-words">
                {{ sshErrorMsg() }}
              </div>
            }

            <!-- Collapsible manual konsertd command for containers / Windows / no-SSH hosts -->
            <div class="pt-1 border-t border-[#26282f]">
              <button
                type="button"
                (click)="showManualConnect.set(!showManualConnect())"
                class="inline-flex items-center gap-1.5 text-[11px] text-[#787a82] hover:text-[#c8cad0] transition cursor-pointer"
              >
                <app-icon [name]="showManualConnect() ? 'chevron-down' : 'chevron-right'" [size]="12" />
                <span>Manual konsertd command (containers, Windows, or outbound-only hosts)</span>
              </button>

              @if (showManualConnect()) {
                <div class="mt-2.5 space-y-2.5 text-[12px]">
                  <div class="flex items-center gap-3 text-[11px]">
                    <label class="inline-flex items-center gap-1.5 text-[#b4b6be] cursor-pointer">
                      <input type="checkbox" [ngModel]="addMachineSandbox()" (ngModelChange)="addMachineSandbox.set($event)" class="accent-[#4a668a]" />
                      <span>Opt-in sandboxed mode</span>
                    </label>
                    <label class="inline-flex items-center gap-1.5 text-[#b4b6be] cursor-pointer">
                      <input type="checkbox" [ngModel]="addMachineMock()" (ngModelChange)="addMachineMock.set($event)" class="accent-[#4a668a]" />
                      <span>Include mock driver</span>
                    </label>
                  </div>

                  <div class="rounded-lg border border-[#2b2d35] bg-[#111214] p-3 space-y-2">
                    <div class="flex items-center justify-between text-[11px] text-[#787a82]">
                      <span>Connect command</span>
                      <button
                        type="button"
                        (click)="copyConnectCmd()"
                        class="inline-flex items-center gap-1 rounded px-2 py-0.5 bg-[#202228] hover:bg-[#2a2c34] active:scale-95 text-[#d4d4d8] transition cursor-pointer"
                      >
                        <app-icon name="copy" [size]="11" />
                        <span>{{ copiedCmd() ? 'Copied' : 'Copy' }}</span>
                      </button>
                    </div>
                    <pre class="font-mono-ide text-[11px] text-[#e4e4e7] whitespace-pre-wrap break-all">{{ connectCommand() }}</pre>
                  </div>
                </div>
              }
            </div>
          </div>
        </div>
      }

      <!-- ORCA-STYLE FROSTED SPOTLIGHT SEARCH / COMMAND PALETTE (⌘K) -->
      @if (commandPaletteOpen()) {
        <div class="ks-backdrop fixed inset-0 z-50 flex items-start justify-center pt-[12vh] px-4" (click)="commandPaletteOpen.set(false)">
          <div
            class="ks-modal w-full max-w-xl rounded-xl border border-[#31333d] bg-[#191a1e]/95 backdrop-blur-2xl overflow-hidden"
            (click)="$event.stopPropagation()"
          >
            <!-- Inner Rounded Search Input -->
            <div class="p-2.5 border-b border-[#25272e]">
              <div class="flex items-center gap-2.5 rounded-lg border border-[#2c2e37] bg-[#141518] px-3 py-2 focus-within:border-[#454958] transition">
                <app-icon name="search" [size]="15" class="text-[#787a82]" />
                <input
                  type="text"
                  [ngModel]="commandQuery()"
                  (ngModelChange)="onPaletteQuery($event)"
                  (keydown)="onPaletteKeydown($event)"
                  placeholder="Search spaces, projects, repos, worktrees, machines, sessions..."
                  autofocus
                  class="flex-1 bg-transparent text-[13px] text-[#ececed] placeholder-[#686a72] focus:outline-none"
                />
                <span class="rounded border border-[#2d3039] bg-[#1c1e24] px-2 py-0.5 text-[11px] text-[#8e9098]">
                  {{ filteredCommands().length }}
                </span>
              </div>
            </div>

            <!-- Grouped Results -->
            <div class="max-h-[360px] overflow-y-auto px-2 py-2 space-y-3 text-[12px]">
              @if (filteredCommands().length === 0) {
                <div class="py-8 text-center text-[#686a72]">No matching sessions, machines, or actions.</div>
              }

              @if (paletteSessions().length > 0) {
                <div class="space-y-0.5">
                  <div class="px-2.5 pb-1 text-[10px] font-semibold uppercase tracking-wider text-[#686a72]">
                    Recent Sessions &amp; Terminals
                  </div>
                  @for (item of paletteSessions(); track item.id) {
                    <button
                      type="button"
                      (mouseenter)="paletteIndex.set(item.flatIndex)"
                      (click)="runCommand(item.action)"
                      class="w-full flex items-center justify-between gap-2 rounded-lg px-2.5 py-2 text-left transition-colors duration-100 cursor-pointer"
                      [class.bg-[#2c2e36]]="paletteIndex() === item.flatIndex"
                      [class.text-[#f4f4f5]]="paletteIndex() === item.flatIndex"
                      [class.text-[#c8cad0]]="paletteIndex() !== item.flatIndex"
                    >
                      <div class="flex items-center gap-2.5 min-w-0">
                        <app-icon [name]="item.icon" [size]="14" />
                        <span class="font-medium truncate">{{ item.label }}</span>
                        @if (item.sublabel) {
                          <span class="text-[11px] text-[#787a82] shrink-0">{{ item.sublabel }}</span>
                        }
                      </div>
                      <div class="flex items-center gap-1.5 shrink-0">
                        @if (item.pill) {
                          <span class="rounded border border-[#343640] bg-[#1a1b20] px-1.5 py-0.5 text-[10px] font-mono-ide text-[#a1a3ab]">
                            {{ item.pill }}
                          </span>
                        }
                      </div>
                    </button>
                  }
                </div>
              }

              @if (paletteTaxonomy().length > 0) {
                <div class="space-y-0.5">
                  <div class="px-2.5 pb-1 text-[10px] font-semibold uppercase tracking-wider text-[#686a72]">
                    Spaces, Projects, Repos &amp; Worktrees
                  </div>
                  @for (item of paletteTaxonomy(); track item.id) {
                    <button
                      type="button"
                      (mouseenter)="paletteIndex.set(item.flatIndex)"
                      (click)="runCommand(item.action)"
                      class="w-full flex items-center justify-between gap-2 rounded-lg px-2.5 py-2 text-left transition-colors duration-100 cursor-pointer"
                      [class.bg-[#2c2e36]]="paletteIndex() === item.flatIndex"
                      [class.text-[#f4f4f5]]="paletteIndex() === item.flatIndex"
                      [class.text-[#c8cad0]]="paletteIndex() !== item.flatIndex"
                    >
                      <div class="flex items-center gap-2.5 min-w-0">
                        <app-icon [name]="item.icon" [size]="13" class="text-[#8e9098]" />
                        <span class="font-medium truncate">{{ item.label }}</span>
                        @if (item.sublabel) {
                          <span class="text-[11px] text-[#787a82] truncate">{{ item.sublabel }}</span>
                        }
                      </div>
                      @if (item.pill) {
                        <span class="rounded border border-[#343640] bg-[#1a1b20] px-1.5 py-0.5 text-[10px] font-mono-ide text-[#a1a3ab] shrink-0">
                          {{ item.pill }}
                        </span>
                      }
                    </button>
                  }
                </div>
              }

              @if (paletteMachines().length > 0) {
                <div class="space-y-0.5">
                  <div class="px-2.5 pb-1 text-[10px] font-semibold uppercase tracking-wider text-[#686a72]">
                    Machines
                  </div>
                  @for (item of paletteMachines(); track item.id) {
                    <button
                      type="button"
                      (mouseenter)="paletteIndex.set(item.flatIndex)"
                      (click)="runCommand(item.action)"
                      class="w-full flex items-center justify-between gap-2 rounded-lg px-2.5 py-2 text-left transition-colors duration-100 cursor-pointer"
                      [class.bg-[#2c2e36]]="paletteIndex() === item.flatIndex"
                      [class.text-[#f4f4f5]]="paletteIndex() === item.flatIndex"
                      [class.text-[#c8cad0]]="paletteIndex() !== item.flatIndex"
                    >
                      <div class="flex items-center gap-2.5 min-w-0">
                        <span class="h-2 w-2 rounded-full shrink-0" [class.bg-[#69b879]]="item.dotOnline" [class.bg-[#4c4e56]]="!item.dotOnline"></span>
                        <span class="font-medium truncate">{{ item.label }}</span>
                        @if (item.sublabel) {
                          <span class="rounded border border-[#32343d] bg-[#202127] px-1.5 py-0.5 text-[10px] text-[#8e9098]">
                            {{ item.sublabel }}
                          </span>
                        }
                      </div>
                      @if (item.pill) {
                        <span class="rounded border border-[#343640] bg-[#1a1b20] px-1.5 py-0.5 text-[10px] font-mono-ide text-[#a1a3ab]">
                          {{ item.pill }}
                        </span>
                      }
                    </button>
                  }
                </div>
              }

              @if (paletteActions().length > 0) {
                <div class="space-y-0.5">
                  <div class="px-2.5 pb-1 text-[10px] font-semibold uppercase tracking-wider text-[#686a72]">
                    Actions
                  </div>
                  @for (item of paletteActions(); track item.id) {
                    <button
                      type="button"
                      (mouseenter)="paletteIndex.set(item.flatIndex)"
                      (click)="runCommand(item.action)"
                      class="w-full flex items-center justify-between gap-2 rounded-lg px-2.5 py-2 text-left transition-colors duration-100 cursor-pointer"
                      [class.bg-[#2c2e36]]="paletteIndex() === item.flatIndex"
                      [class.text-[#f4f4f5]]="paletteIndex() === item.flatIndex"
                      [class.text-[#c8cad0]]="paletteIndex() !== item.flatIndex"
                    >
                      <div class="flex items-center gap-2.5 min-w-0">
                        <app-icon [name]="item.icon" [size]="13" class="text-[#8e9098]" />
                        <span class="truncate">{{ item.label }}</span>
                      </div>
                      @if (item.pill) {
                        <span class="ks-kbd">{{ item.pill }}</span>
                      }
                    </button>
                  }
                </div>
              }
            </div>

            <!-- Orca-Style Keyboard Hint Footer -->
            <div class="flex items-center justify-end gap-3 px-3 py-2 border-t border-[#24262d] bg-[#15161a]/90 text-[11px] text-[#6d7078]">
              <span class="inline-flex items-center gap-1"><span class="ks-kbd">Enter</span> Open</span>
              <span class="inline-flex items-center gap-1"><span class="ks-kbd">Esc</span> Close</span>
              <span class="inline-flex items-center gap-1"><span class="ks-kbd">↑↓</span> Move</span>
              <span class="inline-flex items-center gap-1"><span class="ks-kbd">⌘J</span> Scratch</span>
            </div>
          </div>
        </div>
      }
    </div>
  `,
})
export class AppShellComponent implements OnInit, OnDestroy {
  @ViewChild('mainPane') mainPane?: ElementRef<HTMLElement>;

  readonly orchestratorTabId = ORCHESTRATOR_TAB_ID;

  readonly auth = inject(AuthService);
  readonly fleet = inject(FleetStore);
  readonly tabs = inject(TabsStore);
  readonly socket = inject(KonsertSocketService);
  readonly notifications = inject(NotificationService);
  private readonly api = inject(ApiClientService);

  readonly leftOpen = signal(true);
  readonly rightOpen = signal(true);
  readonly inspectorPanel = signal<InspectorPanel>('agent');
  readonly mobileFleetOpen = signal(false);
  readonly mobileTelemetryOpen = signal(false);

  // Unified session modal (Chat Session vs Terminal with Machine/Agent/Model cascade)
  readonly dispatchModalOpen = signal(false);
  readonly sessionKind = signal<SessionKind>('chat');
  readonly dispatchSpaceId = signal<string | null>(null);
  readonly dispatchRepoId = signal<string | null>(null);
  readonly dispatchWorktreeId = signal<string | null>(null);
  readonly dispatchAgent = signal('claude');
  readonly dispatchModel = signal('');
  readonly dispatchWorkerId = signal('');
  readonly dispatchPerm = signal<PermissionMode>('full_access');
  readonly dispatchRepo = signal('~');
  readonly dispatchBranch = signal('');
  readonly dispatchPrompt = signal('');
  readonly machineSourceHint = signal('');
  readonly agentSourceHint = signal('');
  readonly modelSourceHint = signal('');
  readonly dispatching = signal(false);
  readonly dispatchError = signal('');

  // New Space modal
  readonly newSpaceModalOpen = signal(false);
  readonly newSpaceWorkerId = signal('');
  readonly newSpacePath = signal('~');
  readonly newSpaceName = signal('');
  readonly newSpaceInitialAgent = signal('shell');
  readonly creatingSpace = signal(false);
  readonly newSpaceError = signal('');

  // Settings & Machine Details modals
  readonly settingsOpen = signal(false);
  readonly inspectedWorkerId = signal<string | null>(null);
  readonly inspectedWorker = computed(() => {
    const id = this.inspectedWorkerId();
    return id ? this.fleet.workersById()[id] ?? null : null;
  });

  // Add Machine modal (Herdr-style SSH config discovery + 1-click bootstrap + manual fallback)
  readonly addMachineOpen = signal(false);
  readonly sshHosts = signal<SSHHost[]>([]);
  readonly sshHostsLoading = signal(false);
  readonly customSshTarget = signal('');
  readonly bootstrappingTarget = signal('');
  readonly sshStageLabel = signal('');
  readonly sshStageDetail = signal('');
  readonly sshStatusMsg = signal('');
  readonly sshErrorMsg = signal('');
  readonly showManualConnect = signal(false);
  readonly addMachineSandbox = signal(false);
  readonly addMachineMock = signal(false);
  readonly copiedCmd = signal(false);

  // Spotlight / Command palette
  readonly commandPaletteOpen = signal(false);
  readonly commandQuery = signal('');
  readonly paletteIndex = signal(0);

  readonly chatAgentOptions = computed(() => {
    const w = this.fleet.workersById()[this.dispatchWorkerId()];
    const detected = (w?.agents ?? []).filter((a) => a !== 'shell');
    const base = ['agy', 'claude', 'codex', 'grok', 'mock'];
    const merged = Array.from(new Set([...detected, ...base]));
    return merged;
  });

  readonly modelSuggestions = computed(() => {
    switch (this.dispatchAgent().toLowerCase()) {
      case 'claude':
        return ['claude-sonnet-4-5', 'claude-opus-4-1', 'claude-haiku-3-5'];
      case 'agy':
        return ['gemini-2.5-pro', 'gemini-2.5-flash'];
      case 'codex':
        return ['o4-mini', 'o3', 'gpt-4.1'];
      case 'grok':
        return ['grok-code-fast-1', 'grok-4'];
      default:
        return ['claude-sonnet-4-5', 'gemini-2.5-pro', 'o4-mini', 'grok-code-fast-1'];
    }
  });

  readonly activeSpace = computed(() => {
    const sid = this.tabs.activeSpaceId();
    return sid ? this.fleet.spacesById()[sid] : undefined;
  });

  readonly connectCommand = computed(() => {
    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const url = `${proto}//${window.location.host}/ws/worker`;
    const parts = [`konsertd run --controller ${url}`];
    parts.push('--token "$KONSERT_FLEET_TOKEN"');
    if (this.addMachineSandbox()) parts.push('--sandbox');
    if (this.addMachineMock()) parts.push('--enable-mock-driver');
    return parts.join(' ');
  });

  readonly filteredCommands = computed<Array<PaletteItem & { flatIndex: number }>>(() => {
    const q = this.commandQuery().trim().toLowerCase();
    const items: PaletteItem[] = [];

    // 1. Recent sessions & terminals
    for (const r of this.fleet.agentsRoster().slice(0, 6)) {
      const dir = basenamePath(r.task.effective_dir || r.task.workdir);
      const pill = r.task.branch ? `${r.workerName} · ${r.task.branch}` : r.workerName;
      items.push({
        id: `sess-${r.task.id}`,
        section: 'sessions',
        label: r.task.prompt ? r.task.prompt.slice(0, 48) : `${r.task.agent} · ${dir || '~'}`,
        sublabel: r.task.status,
        icon: r.task.agent,
        pill,
        action: () => this.tabs.openTaskTab(r.task, r.workerName),
      });
    }

    // 1b. Taxonomy: Workspaces (Spaces), Projects, Repos, Worktrees
    const wsById = new Map(this.fleet.workspaces().map((ws) => [ws.id, ws]));
    const projById = new Map(this.fleet.projects().map((p) => [p.id, p]));
    const repoById = new Map(this.fleet.repos().map((r) => [r.id, r]));

    for (const ws of this.fleet.workspaces()) {
      items.push({
        id: `tax-ws-${ws.id}`,
        section: 'taxonomy',
        label: ws.name,
        sublabel: ws.is_default ? 'Default Space' : 'Space',
        icon: 'workspace',
        pill: 'Space',
        action: () => {
          this.leftOpen.set(true);
        },
      });
    }
    for (const proj of this.fleet.projects()) {
      const wsName = wsById.get(proj.workspace_id)?.name || 'Default';
      items.push({
        id: `tax-proj-${proj.id}`,
        section: 'taxonomy',
        label: proj.name,
        sublabel: wsName,
        icon: 'project',
        pill: 'Project',
        action: () => {
          this.leftOpen.set(true);
        },
      });
    }
    for (const repo of this.fleet.repos()) {
      const projName = projById.get(repo.project_id)?.name || 'Default';
      const wkName = this.fleet.workersById()[repo.worker_id]?.name || repo.worker_id;
      items.push({
        id: `tax-repo-${repo.id}`,
        section: 'taxonomy',
        label: repo.name,
        sublabel: `${projName} · ${repo.path}`,
        icon: 'repo',
        pill: wkName,
        action: () => this.handleWorktreeSelected({ repo, worktree: null }),
      });
    }
    for (const wt of this.fleet.worktrees()) {
      const repo = repoById.get(wt.repo_id);
      if (!repo) continue;
      items.push({
        id: `tax-wt-${wt.id}`,
        section: 'taxonomy',
        label: `${repo.name} · ${wt.branch || basenamePath(wt.path)}`,
        sublabel: wt.path,
        icon: 'git-branch',
        pill: wt.is_primary ? 'primary' : 'worktree',
        action: () => this.handleWorktreeSelected({ repo, worktree: wt }),
      });
    }

    // 2. Machines
    for (const w of this.fleet.workers()) {
      items.push({
        id: `machine-${w.id}`,
        section: 'machines',
        label: w.name,
        sublabel: w.isolation,
        icon: w.isolation === 'container' ? 'container' : 'server',
        dotOnline: w.status === 'online',
        pill: `${w.os}/${w.arch}`,
        action: () => this.openSessionModal('chat', w),
      });
    }

    // 3. Actions
    items.push(
      {
        id: 'act-chat',
        label: 'New Chat Session on a Machine',
        section: 'actions',
        icon: 'orchestrator',
        pill: 'Chat',
        action: () => this.openSessionModal('chat', null),
      },
      {
        id: 'act-terminal',
        label: 'New Terminal on a Machine',
        section: 'actions',
        icon: 'shell',
        pill: 'Term',
        action: () => this.openSessionModal('terminal', null),
      },
      {
        id: 'act-split',
        label: 'Toggle Side-by-Side Split Terminal',
        section: 'actions',
        icon: 'split',
        pill: '⌘\\',
        action: () => this.toggleSplitPane(),
      },
      {
        id: 'act-scratch',
        label: 'Toggle Floating Scratch Workspace',
        section: 'actions',
        icon: 'scratch',
        pill: '⌘J',
        action: () => this.tabs.toggleScratch(),
      },
      {
        id: 'act-orch',
        label: 'Focus Orchestrator Chat',
        section: 'actions',
        icon: 'orchestrator',
        action: () => this.tabs.selectTab(ORCHESTRATOR_TAB_ID),
      },
      {
        id: 'act-add',
        label: 'Add Machine (SSH or konsertd)',
        section: 'actions',
        icon: 'plus',
        action: () => this.openAddMachineModal(),
      },
      {
        id: 'act-settings',
        label: 'Open Settings (LLM, Terminal, Alerts)',
        section: 'actions',
        icon: 'settings',
        action: () => this.settingsOpen.set(true),
      },
      {
        id: 'act-left',
        label: 'Toggle Left Sidebar',
        section: 'actions',
        icon: 'sidebar-left',
        action: () => this.leftOpen.update((v) => !v),
      },
      {
        id: 'act-right',
        label: 'Toggle Right Inspector',
        section: 'actions',
        icon: 'sidebar-right',
        action: () => this.rightOpen.update((v) => !v),
      }
    );

    const matched = q
      ? items.filter(
          (i) =>
            i.label.toLowerCase().includes(q) ||
            (i.sublabel || '').toLowerCase().includes(q) ||
            (i.pill || '').toLowerCase().includes(q)
        )
      : items;

    return matched.map((item, idx) => ({ ...item, flatIndex: idx }));
  });

  readonly paletteSessions = computed(() =>
    this.filteredCommands().filter((i) => i.section === 'sessions')
  );
  readonly paletteTaxonomy = computed(() =>
    this.filteredCommands().filter((i) => i.section === 'taxonomy')
  );
  readonly paletteMachines = computed(() =>
    this.filteredCommands().filter((i) => i.section === 'machines')
  );
  readonly paletteActions = computed(() =>
    this.filteredCommands().filter((i) => i.section === 'actions')
  );

  readonly isSplitVisible = computed(() => {
    const active = this.tabs.activeTab();
    const split = this.tabs.splitTab();
    return !!split && active?.kind === 'terminal' && split.kind === 'terminal';
  });

  @HostListener('window:keydown', ['$event'])
  onKeyDown(ev: KeyboardEvent): void {
    const meta = ev.metaKey || ev.ctrlKey;
    if (meta && (ev.key.toLowerCase() === 'k' || (ev.shiftKey && ev.key.toLowerCase() === 'p'))) {
      ev.preventDefault();
      this.openCommandPalette();
      return;
    }
    if (meta && ev.key.toLowerCase() === 'j') {
      ev.preventDefault();
      this.tabs.toggleScratch();
      return;
    }
    // Cmd+1-9, Cmd+W, Cmd+\ use metaKey (never Ctrl) so interactive PTY Ctrl+W / Ctrl+\ never clash.
    if (ev.metaKey && !ev.ctrlKey && !ev.altKey) {
      if (ev.key >= '1' && ev.key <= '9') {
        const idx = Number(ev.key) - 1;
        const visible = this.tabs.visibleTabs();
        if (visible[idx]) {
          ev.preventDefault();
          this.selectTab(visible[idx].id);
        }
        return;
      }
      if (ev.key.toLowerCase() === 'w') {
        const active = this.tabs.activeTab();
        if (active && !active.pinned) {
          ev.preventDefault();
          this.tabs.closeTab(active.id);
        }
        return;
      }
      if (ev.key === '\\') {
        ev.preventDefault();
        void this.toggleSplitPane();
        return;
      }
    }
    if (ev.key === 'Escape') {
      this.commandPaletteOpen.set(false);
      this.addMachineOpen.set(false);
      this.dispatchModalOpen.set(false);
      this.newSpaceModalOpen.set(false);
      this.settingsOpen.set(false);
      this.inspectedWorkerId.set(null);
    }
  }

  ngOnInit(): void {
    void this.fleet.refresh();
    void this.fleet.loadSettings();
    this.socket.connect();
  }

  ngOnDestroy(): void {
    this.socket.disconnect();
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
    if (this.tabs.toggleSplit()) {
      return;
    }
    // No second terminal tab open yet — spawn a quick shell in the active context and pair it side-by-side!
    const active = this.tabs.activeTab();
    const activeTask = active?.taskId ? this.fleet.tasksById()[active.taskId] : undefined;
    const workerId = active?.workerId || activeTask?.worker_id || this.fleet.resolveSessionDefaults().machine_id;
    const repoPath = activeTask?.effective_dir || activeTask?.workdir || this.activeSpace()?.path || '~';
    if (!workerId) return;
    try {
      const res = await this.api.createSession({
        worker_selector: { worker_id: workerId },
        space_id: active?.spaceId || undefined,
        session_kind: 'terminal',
        agent: 'shell',
        repo_path: repoPath,
        mode: 'interactive',
        origin: 'direct',
        permission_mode: this.fleet.settings().default_permission_mode,
      });
      await this.fleet.refresh();
      const newTask = this.fleet.tasksById()[res.task_id];
      const w = this.fleet.workersById()[workerId];
      if (newTask) {
        this.tabs.openTaskTab(newTask, w?.name, false);
        this.tabs.toggleSplit(newTask.id);
      }
    } catch {
      // ignore
    }
  }

  startSplitDrag(ev: PointerEvent): void {
    const container = this.mainPane?.nativeElement;
    if (!container) return;
    ev.preventDefault();
    const rect = container.getBoundingClientRect();
    if (rect.width <= 0) return;

    const onMove = (moveEv: PointerEvent) => {
      const ratio = (moveEv.clientX - rect.left) / rect.width;
      this.tabs.setSplitRatio(ratio);
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
    this.openSessionModal('chat', worker, null);
    this.dispatchRepoId.set(ev.repo.id);
    this.dispatchWorktreeId.set(ev.worktree?.id || null);
    this.dispatchRepo.set(ev.worktree?.path || ev.repo.path || '~');
    this.dispatchBranch.set(ev.worktree?.branch || '');
  }

  openCommandPalette(): void {
    this.commandQuery.set('');
    this.paletteIndex.set(0);
    this.commandPaletteOpen.set(true);
  }

  onPaletteQuery(val: string): void {
    this.commandQuery.set(val);
    this.paletteIndex.set(0);
  }

  onPaletteKeydown(ev: KeyboardEvent): void {
    const list = this.filteredCommands();
    if (list.length === 0) return;
    if (ev.key === 'ArrowDown') {
      ev.preventDefault();
      this.paletteIndex.update((i) => (i + 1) % list.length);
    } else if (ev.key === 'ArrowUp') {
      ev.preventDefault();
      this.paletteIndex.update((i) => (i - 1 + list.length) % list.length);
    } else if (ev.key === 'Enter') {
      ev.preventDefault();
      const target = list[this.paletteIndex()] ?? list[0];
      if (target) this.runCommand(target.action);
    }
  }

  runCommand(fn: () => void): void {
    this.commandPaletteOpen.set(false);
    fn();
  }

  openNewTabInActiveContext(): void {
    const sp = this.activeSpace();
    if (sp) {
      const w = this.fleet.workersById()[sp.worker_id] ?? null;
      this.openSessionModal('chat', w, sp);
      return;
    }
    this.openSessionModal('chat', null);
  }

  openNewSpaceModal(worker: Worker | null): void {
    this.newSpaceError.set('');
    const target = worker || this.fleet.workers().find((w) => w.status === 'online') || this.fleet.workers()[0];
    this.newSpaceWorkerId.set(target?.id || '');
    this.newSpacePath.set('~');
    this.newSpaceName.set('');
    this.newSpaceInitialAgent.set('shell');
    this.newSpaceModalOpen.set(true);
  }

  async submitNewSpace(): Promise<void> {
    this.creatingSpace.set(true);
    this.newSpaceError.set('');
    try {
      const sp = await this.api.createSpace({
        worker_id: this.newSpaceWorkerId(),
        path: this.newSpacePath().trim() || '~',
        name: this.newSpaceName().trim() || undefined,
      });
      this.fleet.upsertSpace(sp);
      const res = await this.api.dispatchTask({
        space_id: sp.id,
        agent: this.newSpaceInitialAgent(),
        mode: 'interactive',
        origin: 'direct',
        permission_mode: this.fleet.settings().default_permission_mode,
      });
      await this.fleet.refresh();
      const task = this.fleet.tasksById()[res.task_id];
      const w = this.fleet.workersById()[sp.worker_id];
      if (task) {
        this.tabs.openTaskTab(task, w?.name);
      }
      this.newSpaceModalOpen.set(false);
    } catch (err) {
      this.newSpaceError.set(err instanceof Error ? err.message : 'Failed to create space');
    } finally {
      this.creatingSpace.set(false);
    }
  }

  async handleSpaceSelected(ev: { space: Space; worker: Worker }): Promise<void> {
    const spaceTasks = this.fleet
      .tasks()
      .filter((t) => t.space_id === ev.space.id && (t.status === 'running' || t.status === 'waiting_input' || t.status === 'assigned'));
    if (spaceTasks.length > 0) {
      this.tabs.selectSpace(ev.space, spaceTasks, ev.worker.name);
      return;
    }
    // Check if there are already mounted tabs for this space
    const existingTabs = this.tabs.openTabs().filter((t) => t.spaceId === ev.space.id);
    if (existingTabs.length > 0) {
      this.tabs.activeSpaceId.set(ev.space.id);
      this.tabs.activeTabId.set(existingTabs[0].id);
      return;
    }
    // Otherwise launch a fresh interactive shell tab inside this Space automatically (Herdr feel!)
    this.tabs.activeSpaceId.set(ev.space.id);
    if (ev.worker.status === 'online') {
      try {
        const res = await this.api.dispatchTask({
          space_id: ev.space.id,
          agent: 'shell',
          mode: 'interactive',
          origin: 'direct',
          permission_mode: this.fleet.settings().default_permission_mode,
        });
        await this.fleet.refresh();
        const task = this.fleet.tasksById()[res.task_id];
        if (task) {
          this.tabs.openTaskTab(task, ev.worker.name);
        }
      } catch {
        this.openSessionModal('terminal', ev.worker, ev.space);
      }
    }
  }

  private formatSourceHint(src: string): string {
    switch (src) {
      case 'localhost':
        return 'localhost';
      case 'default':
        return 'default';
      case 'last_used':
        return 'last used';
      case 'guess':
        return 'auto';
      default:
        return '';
    }
  }

  openSessionModal(kind: SessionKind, worker: Worker | null, space: Space | null = null): void {
    this.dispatchError.set('');
    this.sessionKind.set(kind);
    this.dispatchSpaceId.set(space?.id || null);
    this.dispatchRepoId.set(null);
    this.dispatchWorktreeId.set(null);
    this.dispatchPerm.set(this.fleet.settings().default_permission_mode || 'full_access');
    this.dispatchRepo.set(space?.path || '~');
    this.dispatchBranch.set(space?.branch || '');

    const explicitMachine = space?.worker_id || worker?.id || '';
    const resolved = this.fleet.resolveSessionDefaults(explicitMachine);
    this.dispatchWorkerId.set(resolved.machine_id);
    this.machineSourceHint.set(this.formatSourceHint(resolved.machine_source));
    this.dispatchAgent.set(resolved.agent);
    this.agentSourceHint.set(this.formatSourceHint(resolved.agent_source));
    this.dispatchModel.set(resolved.model);
    this.modelSourceHint.set(this.formatSourceHint(resolved.model_source));
    this.dispatchModalOpen.set(true);
  }

  switchSessionKind(kind: SessionKind): void {
    this.sessionKind.set(kind);
  }

  onSessionMachineChange(workerId: string): void {
    this.dispatchWorkerId.set(workerId);
    this.machineSourceHint.set('');
    const resolved = this.fleet.resolveSessionDefaults(workerId);
    if (this.agentSourceHint() !== '') {
      this.dispatchAgent.set(resolved.agent);
      this.agentSourceHint.set(this.formatSourceHint(resolved.agent_source));
    }
  }

  onSessionAgentChange(agent: string): void {
    this.dispatchAgent.set(agent);
    this.agentSourceHint.set('');
    if (this.modelSourceHint() === 'auto' || !this.dispatchModel().trim()) {
      this.dispatchModel.set(guessModelForAgent(agent, this.fleet.settings().llm_model));
      this.modelSourceHint.set('auto');
    }
  }

  onSessionModelChange(model: string): void {
    this.dispatchModel.set(model);
    this.modelSourceHint.set('');
  }

  canSubmitDispatch(): boolean {
    if (!this.dispatchRepo().trim()) return false;
    if (this.sessionKind() === 'chat' && !this.dispatchAgent().trim()) return false;
    return true;
  }

  async submitDispatch(): Promise<void> {
    this.dispatching.set(true);
    this.dispatchError.set('');
    try {
      const isChat = this.sessionKind() === 'chat';
      const res = await this.api.createSession({
        worker_selector: this.dispatchWorkerId() ? { worker_id: this.dispatchWorkerId() } : undefined,
        space_id: this.dispatchSpaceId() || undefined,
        repo_id: this.dispatchRepoId() || undefined,
        worktree_id: this.dispatchWorktreeId() || undefined,
        session_kind: this.sessionKind(),
        agent: isChat ? this.dispatchAgent() : 'shell',
        model: isChat ? this.dispatchModel().trim() || undefined : undefined,
        repo_path: this.dispatchRepo().trim() || '~',
        branch: this.dispatchBranch().trim() || undefined,
        use_worktree: !!this.dispatchBranch().trim(),
        prompt: isChat ? this.dispatchPrompt().trim() : '',
        mode: 'interactive',
        origin: 'direct',
        permission_mode: this.dispatchPerm(),
      });
      await Promise.all([this.fleet.refresh(), this.fleet.loadSettings()]);
      const task = this.fleet.tasksById()[res.task_id];
      if (task) {
        const w = this.fleet.workersById()[task.worker_id];
        this.tabs.openTaskTab(task, w?.name);
      }
      this.dispatchPrompt.set('');
      this.dispatchModalOpen.set(false);
    } catch (err) {
      this.dispatchError.set(err instanceof Error ? err.message : 'Failed to start session');
    } finally {
      this.dispatching.set(false);
    }
  }

  openMachineDetailsModal(worker: Worker): void {
    this.inspectedWorkerId.set(worker.id);
  }

  inspectSSHHostMachine(host: SSHHost): void {
    const match = this.fleet.workers().find(
      (w) =>
        w.name.toLowerCase() === host.alias.toLowerCase() ||
        w.host_group.toLowerCase() === host.alias.toLowerCase() ||
        (host.hostname && w.name.toLowerCase() === host.hostname.toLowerCase())
    );
    if (match) {
      this.addMachineOpen.set(false);
      this.inspectedWorkerId.set(match.id);
    }
  }

  onMachineRemoved(_workerId: string): void {
    this.inspectedWorkerId.set(null);
    // Refresh SSH host Added status if user re-opens Add Machine modal
    this.sshHosts.update((list) =>
      list.map((h) => ({
        ...h,
        added: this.fleet.workers().some(
          (w) =>
            w.name.toLowerCase() === h.alias.toLowerCase() ||
            w.host_group.toLowerCase() === h.alias.toLowerCase() ||
            (h.hostname && w.name.toLowerCase() === h.hostname.toLowerCase())
        ),
      }))
    );
  }

  async openAddMachineModal(): Promise<void> {
    this.addMachineOpen.set(true);
    this.sshStatusMsg.set('');
    this.sshErrorMsg.set('');
    this.customSshTarget.set('');
    this.sshHostsLoading.set(true);
    try {
      const res = await this.api.listSSHHosts();
      this.sshHosts.set(res.hosts ?? []);
    } catch {
      this.sshHosts.set([]);
    } finally {
      this.sshHostsLoading.set(false);
    }
  }

  async bootstrapSSHHost(target: string): Promise<void> {
    const clean = target.trim();
    if (!clean) return;
    this.bootstrappingTarget.set(clean);
    this.sshStageLabel.set('Connecting');
    this.sshStageDetail.set(`Reaching ${clean} over SSH...`);
    this.sshStatusMsg.set('');
    this.sshErrorMsg.set('');
    try {
      const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
      const controllerUrl = `${proto}//${window.location.host}/ws/worker`;
      const resp = await fetch('/api/ssh/bootstrap?stream=1', {
        method: 'POST',
        credentials: 'same-origin',
        headers: {
          'Content-Type': 'application/json',
          Accept: 'text/event-stream',
        },
        body: JSON.stringify({
          target: clean,
          controller_url: controllerUrl,
          sandbox: this.addMachineSandbox(),
          enable_mock: this.addMachineMock(),
        }),
      });

      let finalResult: { ok: boolean; message?: string; error?: string; stage?: string } | null = null;
      const contentType = resp.headers.get('content-type') || '';
      if (contentType.includes('text/event-stream') && resp.body) {
        const reader = resp.body.getReader();
        const decoder = new TextDecoder();
        let buf = '';
        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += decoder.decode(value, { stream: true });
          const parts = buf.split('\n\n');
          buf = parts.pop() || '';
          for (const part of parts) {
            const line = part.trim();
            if (!line.startsWith('data:')) continue;
            try {
              const ev = JSON.parse(line.slice(5).trim()) as {
                type: string;
                label?: string;
                detail?: string;
                result?: { ok: boolean; message?: string; error?: string; stage?: string };
              };
              if (ev.type === 'stage') {
                this.sshStageLabel.set(ev.label || 'Working');
                this.sshStageDetail.set(ev.detail || '');
              } else if (ev.type === 'done' && ev.result) {
                finalResult = ev.result;
              }
            } catch {
              // ignore malformed SSE chunk
            }
          }
        }
      } else {
        finalResult = (await resp.json()) as { ok: boolean; message?: string; error?: string; stage?: string };
      }

      if (!finalResult || !finalResult.ok) {
        const prefix = finalResult?.stage ? `[${finalResult.stage}] ` : '';
        this.sshErrorMsg.set(`${prefix}${finalResult?.error || `Failed to add ${clean}`}`);
        return;
      }
      this.sshStatusMsg.set(finalResult.message || `konsertd ready on ${clean}.`);
      this.sshHosts.update((list) =>
        list.map((h) => (h.alias === clean || h.display === clean ? { ...h, added: true } : h))
      );
      setTimeout(() => void this.fleet.refresh(), 1200);
    } catch (err) {
      this.sshErrorMsg.set(err instanceof Error ? err.message : `SSH bootstrap failed for ${clean}`);
    } finally {
      this.bootstrappingTarget.set('');
      this.sshStageLabel.set('');
      this.sshStageDetail.set('');
    }
  }

  async bootstrapCustomSSH(): Promise<void> {
    const target = this.customSshTarget().trim();
    if (!target) return;
    await this.bootstrapSSHHost(target);
    if (!this.sshErrorMsg()) {
      this.customSshTarget.set('');
    }
  }

  async copyConnectCmd(): Promise<void> {
    try {
      await navigator.clipboard.writeText(this.connectCommand());
      this.copiedCmd.set(true);
      setTimeout(() => this.copiedCmd.set(false), 1800);
    } catch {
      // ignore
    }
  }
}

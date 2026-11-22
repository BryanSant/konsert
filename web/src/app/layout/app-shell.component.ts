import { Component, HostListener, OnDestroy, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { PermissionMode, Space, Worker } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { AuthService } from '../core/services/auth.service';
import { KonsertSocketService } from '../core/services/konsert-socket.service';
import { FleetStore, basenamePath } from '../core/state/fleet.store';
import { ORCHESTRATOR_TAB_ID, TabsStore } from '../core/state/tabs.store';
import { OrchestratorChatComponent } from '../features/orchestrator/orchestrator-chat.component';
import { XtermPaneComponent } from '../features/terminal/xterm-pane.component';
import { IconComponent } from '../shared/icon.component';
import { FleetSidebarComponent } from './fleet-sidebar.component';
import { FloatingScratchComponent } from './floating-scratch.component';
import { SettingsModalComponent } from './settings-modal.component';
import { StatusFooterComponent } from './status-footer.component';
import { InspectorPanel, TelemetryDrawerComponent } from './telemetry-drawer.component';

interface PaletteItem {
  id: string;
  section: 'sessions' | 'machines' | 'actions';
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
    FleetSidebarComponent,
    TelemetryDrawerComponent,
    StatusFooterComponent,
    FloatingScratchComponent,
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
              (click)="tabs.selectTab(tab.id)"
              class="group flex items-center gap-1.5 px-2.5 py-1 text-[12px] rounded-md cursor-pointer shrink-0 transition-all duration-150"
              [class.bg-[#24262d]]="tabs.activeTabId() === tab.id"
              [class.text-[#f4f4f5]]="tabs.activeTabId() === tab.id"
              [class.text-[#8e9098]]="tabs.activeTabId() !== tab.id"
              [class.hover:bg-[#1e2026]]="tabs.activeTabId() !== tab.id"
              [class.hover:text-[#d4d4d8]]="tabs.activeTabId() !== tab.id"
            >
              @if (tab.kind === 'orchestrator') {
                <app-icon name="orchestrator" [size]="12" class="text-[#60a5fa]" />
              } @else {
                <app-icon [name]="tab.agent || 'shell'" [size]="12" />
              }
              <span class="max-w-[150px] truncate">{{ tab.title }}</span>
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
            [title]="activeSpace() ? 'New tab in ' + activeSpace()!.name : 'New direct session'"
            class="rounded-md p-1 text-[#787a82] hover:text-[#ececed] hover:bg-[#23252b] active:scale-95 transition cursor-pointer shrink-0"
          >
            <app-icon name="plus" [size]="14" />
          </button>
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
              (openAddMachine)="addMachineOpen.set(true)"
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
                (openAddMachine)="mobileFleetOpen.set(false); addMachineOpen.set(true)"
                (openSettings)="mobileFleetOpen.set(false); settingsOpen.set(true)"
              />
            </div>
            <div class="ks-backdrop flex-1" (click)="mobileFleetOpen.set(false)"></div>
          </div>
        }

        <!-- Center Active Pane Container -->
        <main class="flex-1 min-w-0 h-full relative overflow-hidden bg-[#111214]">
          @for (tab of tabs.openTabs(); track tab.id) {
            <div class="absolute inset-0" [class.hidden]="tabs.activeTabId() !== tab.id">
              @if (tab.id === orchestratorTabId) {
                <app-orchestrator-chat
                  (openDispatch)="openSessionModal('orchestrator', null)"
                  (openDirectSession)="openSessionModal('direct', null)"
                />
              } @else if (tab.taskId) {
                <app-xterm-pane [taskId]="tab.taskId" [visible]="tabs.activeTabId() === tab.id" />
              }
            </div>
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
      <app-status-footer (openCommands)="openCommandPalette()" />

      <!-- Orca-style Floating Scratch Workspace -->
      <app-floating-scratch />

      <!-- Settings Modal (KON-0Q95Z) -->
      @if (settingsOpen()) {
        <app-settings-modal (close)="settingsOpen.set(false)" />
      }

      <!-- NEW SPACE MODAL (Herdr-style per-machine persisted Space) -->
      @if (newSpaceModalOpen()) {
        <div class="ks-backdrop fixed inset-0 z-50 flex items-center justify-center px-4" (click)="newSpaceModalOpen.set(false)">
          <div class="ks-modal w-full max-w-md rounded-xl border border-[#30323c] bg-[#18191d]/95 backdrop-blur-xl p-5 space-y-4" (click)="$event.stopPropagation()">
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
                <input
                  type="text"
                  name="spPath"
                  [ngModel]="newSpacePath()"
                  (ngModelChange)="newSpacePath.set($event)"
                  placeholder="~ or /home/user/src/my-repo"
                  class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 font-mono-ide text-[#ececed]"
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

      <!-- DUAL-MODE MODAL: DIRECT SESSION vs ORCHESTRATED JOB (Frosted Blur + Springy Entrance) -->
      @if (dispatchModalOpen()) {
        <div class="ks-backdrop fixed inset-0 z-50 flex items-center justify-center px-4" (click)="dispatchModalOpen.set(false)">
          <div class="ks-modal w-full max-w-lg rounded-xl border border-[#30323c] bg-[#18191d]/95 backdrop-blur-xl p-5 space-y-4" (click)="$event.stopPropagation()">
            <div class="flex items-center justify-between">
              <div class="inline-flex rounded-lg bg-[#111214] p-0.5 border border-[#26282e] text-[12px]">
                <button
                  type="button"
                  (click)="switchModalOrigin('direct')"
                  class="rounded-md px-3 py-1 font-medium transition cursor-pointer"
                  [class.bg-[#262830]]="modalOrigin() === 'direct'"
                  [class.text-[#ececed]]="modalOrigin() === 'direct'"
                  [class.text-[#787a82]]="modalOrigin() !== 'direct'"
                >
                  Direct Session
                </button>
                <button
                  type="button"
                  (click)="switchModalOrigin('orchestrator')"
                  class="rounded-md px-3 py-1 font-medium transition cursor-pointer"
                  [class.bg-[#262830]]="modalOrigin() === 'orchestrator'"
                  [class.text-[#ececed]]="modalOrigin() === 'orchestrator'"
                  [class.text-[#787a82]]="modalOrigin() !== 'orchestrator'"
                >
                  Orchestrated Job
                </button>
              </div>

              <button type="button" (click)="dispatchModalOpen.set(false)" class="text-[#787a82] hover:text-[#ececed] transition cursor-pointer">
                <app-icon name="close" [size]="15" />
              </button>
            </div>

            <form (ngSubmit)="submitDispatch()" class="space-y-3 text-[12px]">
              <div class="grid grid-cols-2 gap-3">
                <div>
                  <label class="block text-[11px] text-[#8e9098] mb-1">Machine</label>
                  <select
                    name="worker"
                    [ngModel]="dispatchWorkerId()"
                    (ngModelChange)="dispatchWorkerId.set($event)"
                    class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed] focus:border-[#4a668a] focus:outline-none"
                  >
                    <option value="">Auto-schedule (best online)</option>
                    @for (w of fleet.workers(); track w.id) {
                      <option [value]="w.id">{{ w.name }} ({{ w.status }})</option>
                    }
                  </select>
                </div>

                <div>
                  <label class="block text-[11px] text-[#8e9098] mb-1">Agent / Shell</label>
                  <select
                    name="agent"
                    [ngModel]="dispatchAgent()"
                    (ngModelChange)="dispatchAgent.set($event)"
                    class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed] focus:border-[#4a668a] focus:outline-none"
                  >
                    <option value="shell">shell (interactive terminal)</option>
                    <option value="claude">claude</option>
                    <option value="agy">agy</option>
                    <option value="grok">grok</option>
                    <option value="mock">mock</option>
                  </select>
                </div>
              </div>

              <div class="grid grid-cols-2 gap-3">
                <div>
                  <label class="block text-[11px] text-[#8e9098] mb-1">Workspace Directory</label>
                  <input
                    type="text"
                    name="repo"
                    [ngModel]="dispatchRepo()"
                    (ngModelChange)="dispatchRepo.set($event)"
                    placeholder="~ or /path/to/repo"
                    class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 font-mono-ide text-[#ececed] focus:border-[#4a668a] focus:outline-none"
                  />
                </div>
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
              </div>

              @if (modalOrigin() === 'orchestrator' || dispatchAgent() !== 'shell') {
                <div>
                  <label class="block text-[11px] text-[#8e9098] mb-1">
                    {{ modalOrigin() === 'direct' ? 'Initial Prompt (optional)' : 'Task Prompt' }}
                  </label>
                  <textarea
                    rows="3"
                    name="prompt"
                    [ngModel]="dispatchPrompt()"
                    (ngModelChange)="dispatchPrompt.set($event)"
                    [placeholder]="modalOrigin() === 'direct' ? 'Leave blank to start an interactive session...' : 'Describe the task for the autonomous agent...'"
                    class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed] focus:border-[#4a668a] focus:outline-none"
                  ></textarea>
                </div>
              }

              <div class="grid grid-cols-2 gap-3">
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
                <div>
                  <label class="block text-[11px] text-[#8e9098] mb-1">Lifecycle</label>
                  <select
                    name="mode"
                    [ngModel]="dispatchMode()"
                    (ngModelChange)="dispatchMode.set($event)"
                    class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed]"
                  >
                    <option value="interactive">Interactive (persistent PTY)</option>
                    <option value="oneshot">Oneshot (run & exit)</option>
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
                  {{ dispatching() ? 'Starting...' : modalOrigin() === 'direct' ? 'Open Session' : 'Dispatch Job' }}
                </button>
              </div>
            </form>
          </div>
        </div>
      }

      <!-- HERDR-STYLE + ADD MACHINE MODAL (Frosted Blur + Springy Entrance) -->
      @if (addMachineOpen()) {
        <div class="ks-backdrop fixed inset-0 z-50 flex items-center justify-center px-4" (click)="addMachineOpen.set(false)">
          <div class="ks-modal w-full max-w-lg rounded-xl border border-[#30323c] bg-[#1c1d21]/95 backdrop-blur-xl p-5 space-y-4" (click)="$event.stopPropagation()">
            <div class="flex items-start justify-between gap-2">
              <div>
                <h3 class="text-[15px] font-semibold text-[#ececed]">Add a machine</h3>
                <p class="text-[12px] text-[#8e9098] mt-0.5">
                  Run <code class="font-mono-ide text-[#d4d4d8]">konsertd</code> on any Linux, macOS, Windows host, VM, or Podman container. It connects outbound—no inbound ports or SSH tunnels required.
                </p>
              </div>
              <button type="button" (click)="addMachineOpen.set(false)" class="ks-kbd cursor-pointer hover:text-[#ececed]">
                Esc
              </button>
            </div>

            <div class="space-y-3 text-[12px]">
              <div class="flex items-center gap-3">
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

              <div class="text-[11px] text-[#787a82]">
                Verify detected CLIs &amp; PTY support on the target host with:
                <code class="font-mono-ide text-[#b4b6be]">konsertd doctor</code>
              </div>
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
                  placeholder="Search sessions, machines, tasks, and actions..."
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
  readonly orchestratorTabId = ORCHESTRATOR_TAB_ID;

  readonly auth = inject(AuthService);
  readonly fleet = inject(FleetStore);
  readonly tabs = inject(TabsStore);
  readonly socket = inject(KonsertSocketService);
  private readonly api = inject(ApiClientService);

  readonly leftOpen = signal(true);
  readonly rightOpen = signal(true);
  readonly inspectorPanel = signal<InspectorPanel>('agent');
  readonly mobileFleetOpen = signal(false);
  readonly mobileTelemetryOpen = signal(false);

  // Dual-mode session / job modal
  readonly dispatchModalOpen = signal(false);
  readonly modalOrigin = signal<'direct' | 'orchestrator'>('direct');
  readonly dispatchSpaceId = signal<string | null>(null);
  readonly dispatchAgent = signal('shell');
  readonly dispatchMode = signal<'oneshot' | 'interactive'>('interactive');
  readonly dispatchWorkerId = signal('');
  readonly dispatchPerm = signal<PermissionMode>('full_access');
  readonly dispatchRepo = signal('~');
  readonly dispatchBranch = signal('');
  readonly dispatchPrompt = signal('');
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

  // Settings modal
  readonly settingsOpen = signal(false);

  // Add Machine modal
  readonly addMachineOpen = signal(false);
  readonly addMachineSandbox = signal(false);
  readonly addMachineMock = signal(false);
  readonly copiedCmd = signal(false);

  // Spotlight / Command palette
  readonly commandPaletteOpen = signal(false);
  readonly commandQuery = signal('');
  readonly paletteIndex = signal(0);

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
        action: () => this.openSessionModal('direct', w),
      });
    }

    // 3. Actions
    items.push(
      {
        id: 'act-direct',
        label: 'New Direct Session on a Machine',
        section: 'actions',
        icon: 'shell',
        pill: 'Direct',
        action: () => this.openSessionModal('direct', null),
      },
      {
        id: 'act-job',
        label: 'Dispatch Orchestrated Worktree Job',
        section: 'actions',
        icon: 'git-branch',
        pill: 'Fleet',
        action: () => this.openSessionModal('orchestrator', null),
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
        label: 'Add Machine (Connect konsertd)',
        section: 'actions',
        icon: 'plus',
        action: () => this.addMachineOpen.set(true),
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
  readonly paletteMachines = computed(() =>
    this.filteredCommands().filter((i) => i.section === 'machines')
  );
  readonly paletteActions = computed(() =>
    this.filteredCommands().filter((i) => i.section === 'actions')
  );

  @HostListener('window:keydown', ['$event'])
  onKeyDown(ev: KeyboardEvent): void {
    const meta = ev.metaKey || ev.ctrlKey;
    if (meta && (ev.key.toLowerCase() === 'k' || (ev.shiftKey && ev.key.toLowerCase() === 'p'))) {
      ev.preventDefault();
      this.openCommandPalette();
    } else if (meta && ev.key.toLowerCase() === 'j') {
      ev.preventDefault();
      this.tabs.toggleScratch();
    } else if (ev.key === 'Escape') {
      this.commandPaletteOpen.set(false);
      this.addMachineOpen.set(false);
      this.dispatchModalOpen.set(false);
      this.newSpaceModalOpen.set(false);
      this.settingsOpen.set(false);
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
      this.openSessionModal('direct', w, sp);
      return;
    }
    this.openSessionModal('direct', null);
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
        this.openSessionModal('direct', ev.worker, ev.space);
      }
    }
  }

  openSessionModal(origin: 'direct' | 'orchestrator', worker: Worker | null, space: Space | null = null): void {
    this.dispatchError.set('');
    this.modalOrigin.set(origin);
    this.dispatchSpaceId.set(space?.id || null);
    this.dispatchWorkerId.set(space?.worker_id || worker?.id || '');
    this.dispatchPerm.set(this.fleet.settings().default_permission_mode || 'full_access');
    if (origin === 'direct') {
      this.dispatchMode.set('interactive');
      this.dispatchRepo.set(space?.path || '~');
      this.dispatchBranch.set(space?.branch || '');
      this.dispatchAgent.set('shell');
    } else {
      this.dispatchMode.set('oneshot');
      this.dispatchRepo.set(space?.path || '~');
      const target = worker || this.fleet.workers().find((w) => w.status === 'online');
      const preferred = target?.agents.find((a) => a !== 'shell') || 'mock';
      this.dispatchAgent.set(preferred);
    }
    this.dispatchModalOpen.set(true);
  }

  switchModalOrigin(origin: 'direct' | 'orchestrator'): void {
    this.modalOrigin.set(origin);
    if (origin === 'direct') {
      this.dispatchMode.set('interactive');
    } else {
      this.dispatchMode.set('oneshot');
      if (this.dispatchAgent() === 'shell') {
        const online = this.fleet.workers().find((w) => w.status === 'online');
        this.dispatchAgent.set(online?.agents.find((a) => a !== 'shell') || 'mock');
      }
    }
  }

  canSubmitDispatch(): boolean {
    if (!this.dispatchRepo().trim()) return false;
    if (this.modalOrigin() === 'orchestrator' && this.dispatchAgent() !== 'shell' && this.dispatchMode() === 'oneshot') {
      return !!this.dispatchPrompt().trim();
    }
    return true;
  }

  async submitDispatch(): Promise<void> {
    this.dispatching.set(true);
    this.dispatchError.set('');
    try {
      const res = await this.api.dispatchTask({
        worker_selector: this.dispatchWorkerId() ? { worker_id: this.dispatchWorkerId() } : undefined,
        space_id: this.dispatchSpaceId() || undefined,
        agent: this.dispatchAgent(),
        repo_path: this.dispatchRepo().trim() || '~',
        branch: this.dispatchBranch().trim() || undefined,
        use_worktree: !!this.dispatchBranch().trim(),
        prompt: this.dispatchPrompt().trim(),
        mode: this.dispatchMode(),
        origin: this.modalOrigin(),
        permission_mode: this.dispatchPerm(),
      });
      await this.fleet.refresh();
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

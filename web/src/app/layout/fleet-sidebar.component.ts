import { Component, OnDestroy, OnInit, computed, inject, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { DEFAULT_PROJECT_ID, Project, Repo, Space, Task, Worker, Worktree } from '../core/models/konsert.models';
import { isVersionMismatch } from '../core/models/version';
import { ApiClientService } from '../core/services/api-client.service';
import { AuthService } from '../core/services/auth.service';
import { NotificationService } from '../core/services/notification.service';
import { FleetStore, basenamePath } from '../core/state/fleet.store';
import { ORCHESTRATOR_TAB_ID, TabsStore } from '../core/state/tabs.store';
import { IconComponent } from '../shared/icon.component';
import { TooltipDirective } from '../shared/tooltip.directive';

@Component({
  selector: 'app-fleet-sidebar',
  standalone: true,
  imports: [TooltipDirective, FormsModule, IconComponent],
  template: `
    <div class="flex flex-col h-full w-full bg-[#16171a] text-[#d4d4d8] select-none">
      <!-- Top Search & Orchestrator Navigation -->
      <div class="px-3 pt-3 pb-2.5 space-y-2 shrink-0">
        <div class="relative">
          <app-icon name="search" [size]="13" class="absolute left-2.5 top-1/2 -translate-y-1/2 text-[#86888f]" />
          <input
            type="text"
            [ngModel]="query()"
            (ngModelChange)="query.set($event)"
            placeholder="Search spaces, repos, agents..."
            class="ks-field w-full pl-8 pr-2.5 text-[12px]"
          />
        </div>

        <button
          type="button"
          (click)="selectOrchestrator()"
          class="w-full flex items-center justify-between rounded-md px-2.5 py-2 text-[12.5px] transition cursor-pointer"
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

        <!-- Segmented Switcher: Workspaces (4-level taxonomy) vs Machines (Herdr host view) -->
        <div class="grid grid-cols-2 rounded-[var(--ks-radius-md)] bg-black/25 p-0.5 text-[11.5px]">
          <button
            type="button"
            (click)="navMode.set('workspaces')"
            class="rounded py-1.5 font-medium transition cursor-pointer"
            [class.bg-[#23252d]]="navMode() === 'workspaces'"
            [class.text-[#ececed]]="navMode() === 'workspaces'"
            [class.text-[#8a8c94]]="navMode() !== 'workspaces'"
          >
            Workspaces
          </button>
          <button
            type="button"
            (click)="navMode.set('machines')"
            class="rounded py-1.5 font-medium transition cursor-pointer"
            [class.bg-[#23252d]]="navMode() === 'machines'"
            [class.text-[#ececed]]="navMode() === 'machines'"
            [class.text-[#8a8c94]]="navMode() !== 'machines'"
          >
            Machines
          </button>
        </div>
      </div>

      <!-- Upper Section: Workspaces (Space > Project > Repo > Worktree) OR Machines -->
      <div class="flex-1 min-h-0 overflow-y-auto px-2.5 py-1.5 space-y-1.5">
        @if (navMode() === 'workspaces') {
          <!-- Workspace (Space) Selector Row -->
          <div class="flex items-center gap-1 px-1 pb-1">
            <select
              aria-label="Space"
              class="ks-field ks-field-sm flex-1 min-w-0 text-[11.5px] font-medium"
              [ngModel]="activeWorkspaceId()"
              (ngModelChange)="selectedWorkspaceId.set($event)"
            >
              @for (ws of fleet.workspaces(); track ws.id) {
                <option [value]="ws.id">{{ ws.name }}</option>
              }
            </select>
            <button
              type="button"
              (click)="toggleCreateForm('workspace')"
              ksTooltip="New Workspace (Space)" aria-label="New Workspace (Space)"
              class="ks-icon-btn ks-icon-btn-sm shrink-0"
            >
              <app-icon name="plus" [size]="13" />
            </button>
          </div>

          @if (createMode() === 'workspace') {
            <form (ngSubmit)="submitCreateWorkspace()" class="mx-1 mb-1.5 rounded-md bg-[#131418] p-3 space-y-1.5">
              <input
                type="text"
                name="newWsName"
                [ngModel]="formName()"
                (ngModelChange)="formName.set($event)"
                placeholder="Space name (e.g. Work)"
                autofocus
                class="ks-field ks-field-sm w-full text-[11.5px]"
              />
              <div class="flex justify-end gap-1">
                <button type="button" (click)="createMode.set(null)" class="ks-btn ks-btn-sm">Cancel</button>
                <button type="submit" [disabled]="!formName().trim()" class="ks-btn ks-btn-sm ks-btn-primary">Add</button>
              </div>
            </form>
          }

          <!-- Projects Section Header -->
          <div class="flex items-center justify-between px-1.5 pt-0.5 text-[10.5px] font-medium text-[#8a8c94]">
            <span>Projects</span>
            <button
              type="button"
              (click)="toggleCreateForm('project')"
              ksTooltip="New Project in Space" aria-label="New Project in Space"
              class="ks-icon-btn ks-icon-btn-sm"
            >
              <app-icon name="plus" [size]="13" />
            </button>
          </div>

          @if (createMode() === 'project') {
            <form (ngSubmit)="submitCreateProject()" class="mx-1 mb-1.5 rounded-md bg-[#131418] p-3 space-y-1.5">
              <input
                type="text"
                name="newProjName"
                [ngModel]="formName()"
                (ngModelChange)="formName.set($event)"
                placeholder="Project name (multi-repo)"
                autofocus
                class="ks-field ks-field-sm w-full text-[11.5px]"
              />
              <div class="flex justify-end gap-1">
                <button type="button" (click)="createMode.set(null)" class="ks-btn ks-btn-sm">Cancel</button>
                <button type="submit" [disabled]="!formName().trim()" class="ks-btn ks-btn-sm ks-btn-primary">Add</button>
              </div>
            </form>
          }

          <!-- Projects > Repos > Worktrees Tree -->
          @for (proj of filteredProjects(); track proj.id) {
            <div class="space-y-0.5">
              <div class="group flex items-center justify-between gap-1 rounded-md px-2 py-1.5 hover:bg-[#1e2026] transition">
                <button
                  type="button"
                  (click)="toggleProject(proj.id)"
                  class="flex items-center gap-1.5 min-w-0 flex-1 text-left cursor-pointer"
                >
                  <app-icon
                    name="chevron-down"
                    [size]="11"
                    class="text-[#86888f] transition-transform duration-150"
                    [class.-rotate-90]="isProjectCollapsed(proj.id)"
                  />
                  <app-icon name="project" [size]="12" class="text-[#8e9098]" />
                  <span class="text-[12px] font-medium text-[#d4d4d8] truncate">{{ proj.name }}</span>
                </button>
                <div class="flex items-center gap-0.5 shrink-0">
                  <button
                    type="button"
                    (click)="openAddRepoForm(proj.id)"
                    ksTooltip="Add Repo to {{ proj.name }}" attr.aria-label="Add Repo to {{ proj.name }}"
                    class="ks-icon-btn ks-icon-btn-sm opacity-70 group-hover:opacity-100"
                  >
                    <app-icon name="plus" [size]="13" />
                  </button>
                  @if (!proj.is_default) {
                    <button
                      type="button"
                      (click)="$event.stopPropagation(); deleteProject(proj)"
                      ksTooltip="Delete Project" aria-label="Delete Project"
                      class="ks-icon-btn ks-icon-btn-sm ks-btn-danger opacity-0 group-hover:opacity-100"
                    >
                      <app-icon name="trash" [size]="13" />
                    </button>
                  }
                </div>
              </div>

              @if (createMode() === 'repo' && formParentId() === proj.id) {
                <form (ngSubmit)="submitCreateRepo(proj.id)" class="mx-2 mb-1 rounded-md bg-[#131418] p-3 space-y-1.5 text-[11px]">
                  <select
                    name="repoWorker"
                    aria-label="Machine"
                    [ngModel]="formWorkerId()"
                    (ngModelChange)="formWorkerId.set($event)"
                    class="ks-field ks-field-sm w-full"
                  >
                    @for (w of fleet.workers(); track w.id) {
                      <option [value]="w.id">{{ w.name }}</option>
                    }
                  </select>
                  <input
                    type="text"
                    name="repoPath"
                    [ngModel]="formPath()"
                    (ngModelChange)="formPath.set($event)"
                    placeholder="/path/to/repo"
                    class="ks-field ks-field-sm w-full font-mono-ide"
                  />
                  <input
                    type="text"
                    name="repoName"
                    [ngModel]="formName()"
                    (ngModelChange)="formName.set($event)"
                    placeholder="Repo name (optional)"
                    class="ks-field ks-field-sm w-full"
                  />
                  <div class="flex justify-end gap-1">
                    <button type="button" (click)="createMode.set(null)" class="ks-btn ks-btn-sm">Cancel</button>
                    <button type="submit" [disabled]="!formPath().trim() || !formWorkerId()" class="ks-btn ks-btn-sm ks-btn-primary">Add Repo</button>
                  </div>
                </form>
              }

              @if (!isProjectCollapsed(proj.id)) {
                <div class="pl-3 space-y-0.5">
                  @for (repo of reposForProject(proj.id); track repo.id) {
                    <div class="space-y-0.5">
                      <div class="group/repo flex items-center justify-between gap-1 rounded-md px-2 py-1.5 hover:bg-[#1d1f25] transition">
                        <button
                          type="button"
                          (click)="openWorktreeSession(repo, null)"
                          class="flex items-center gap-1.5 min-w-0 flex-1 text-left cursor-pointer"
                        >
                          <app-icon name="repo" [size]="11" class="text-[#7aa8e8]" />
                          <span class="text-[11.5px] text-[#d4d4d8] truncate">{{ repo.name }}</span>
                          <span class="text-[9.5px] font-mono-ide text-[#86888f] truncate">
                            {{ fleet.workersById()[repo.worker_id]?.name || '' }}
                          </span>
                        </button>
                        <div class="flex items-center gap-0.5 shrink-0">
                          <button
                            type="button"
                            (click)="$event.stopPropagation(); openAddWorktreeForm(repo.id, repo.path)"
                            ksTooltip="Add Worktree" aria-label="Add Worktree"
                            class="ks-icon-btn ks-icon-btn-sm opacity-0 group-hover/repo:opacity-100"
                          >
                            <app-icon name="plus" [size]="13" />
                          </button>
                          <button
                            type="button"
                            (click)="$event.stopPropagation(); deleteRepo(repo)"
                            ksTooltip="Remove Repo" aria-label="Remove Repo"
                            class="ks-icon-btn ks-icon-btn-sm ks-btn-danger opacity-0 group-hover/repo:opacity-100"
                          >
                            <app-icon name="close" [size]="13" />
                          </button>
                        </div>
                      </div>

                      @if (createMode() === 'worktree' && formParentId() === repo.id) {
                        <form (ngSubmit)="submitCreateWorktree(repo.id)" class="mx-2 mb-1 rounded-md bg-[#131418] p-3 space-y-1.5 text-[11px]">
                          <input
                            type="text"
                            name="wtBranch"
                            [ngModel]="formName()"
                            (ngModelChange)="formName.set($event)"
                            placeholder="Branch (e.g. feat/auth)"
                            autofocus
                            class="ks-field ks-field-sm w-full font-mono-ide"
                          />
                          <input
                            type="text"
                            name="wtPath"
                            [ngModel]="formPath()"
                            (ngModelChange)="formPath.set($event)"
                            placeholder="Worktree path"
                            class="ks-field ks-field-sm w-full font-mono-ide"
                          />
                          <div class="flex justify-end gap-1">
                            <button type="button" (click)="createMode.set(null)" class="ks-btn ks-btn-sm">Cancel</button>
                            <button type="submit" [disabled]="!formName().trim()" class="ks-btn ks-btn-sm ks-btn-primary">Create</button>
                          </div>
                        </form>
                      }

                      <!-- Worktrees inside Repo -->
                      @for (wt of worktreesForRepo(repo.id); track wt.id) {
                        <div
                          (click)="openWorktreeSession(repo, wt)"
                          class="group/wt ml-3 flex items-center justify-between gap-1.5 rounded-md px-2 py-1 hover:bg-[#1e2027] text-[11px] text-[#9a9ca4] hover:text-[#ececed] transition cursor-pointer"
                        >
                          <span class="inline-flex items-center gap-1.5 min-w-0">
                            <app-icon name="git-branch" [size]="10" class="text-[#69b879]" />
                            <span class="font-mono-ide truncate">{{ wt.branch || baseName(wt.path) }}</span>
                          </span>
                          @if (!wt.is_primary) {
                            <button
                              type="button"
                              (click)="$event.stopPropagation(); deleteWorktree(wt)"
                              ksTooltip="Remove worktree entry" aria-label="Remove worktree entry"
                              class="ks-icon-btn ks-icon-btn-sm ks-btn-danger opacity-0 group-hover/wt:opacity-100"
                            >
                              <app-icon name="close" [size]="13" />
                            </button>
                          }
                        </div>
                      }
                    </div>
                  }

                  <!-- Default Project shows uncategorized & passively discovered (external) sessions -->
                  @if (proj.is_default && defaultUncategorizedSessions().length > 0) {
                    <div class="pt-1 space-y-0.5">
                      <div class="px-1.5 text-[10px] text-[#86888f]">Uncategorized &amp; Discovered</div>
                      @for (t of defaultUncategorizedSessions(); track t.id) {
                        <button
                          type="button"
                          (click)="selectTask(t, fleet.workersById()[t.worker_id])"
                          class="w-full flex items-center justify-between gap-1.5 rounded-md px-2 py-1.5 text-left text-[11.5px] hover:bg-[#1e2027] transition cursor-pointer"
                          [class.bg-[#24262e]]="tabs.activeTabId() === t.id"
                        >
                          <span class="inline-flex items-center gap-1.5 min-w-0">
                            <app-icon [name]="t.agent" [size]="11" />
                            <span class="truncate text-[#c8cad0]">{{ baseName(t.effective_dir || t.workdir) || t.agent }}</span>
                          </span>
                          @if (t.origin === 'external') {
                            <span class="rounded bg-[#262832] px-1 text-[9px] font-mono-ide text-[#8e9098] shrink-0">ssh</span>
                          }
                        </button>
                      }
                    </div>
                  }

                  @if (reposForProject(proj.id).length === 0 && (!proj.is_default || defaultUncategorizedSessions().length === 0)) {
                    <div class="px-2 py-1 text-[11px] text-[#86888f]">No repos yet</div>
                  }
                </div>
              }
            </div>
          }
        } @else {
          <!-- Herdr-style Machines & Per-Machine Spaces Tree -->
          <div class="flex items-center justify-between px-1.5 py-1 text-[11px] font-medium text-[#8a8c94]">
            <span>Machines</span>
            <button
              type="button"
              (click)="openNewSpace.emit(null)"
              ksTooltip="New folder shortcut on a machine" aria-label="New folder shortcut on a machine"
              class="ks-icon-btn ks-icon-btn-sm"
            >
              <app-icon name="plus" [size]="13" />
            </button>
          </div>

          @if (filteredGroups().length === 0) {
            <div class="px-2 py-4 text-[12px] text-[#86888f] leading-relaxed">
              No machines connected yet. Click <span class="text-[#a1a3ab]">+ Add machine</span> below to connect a host or container.
            </div>
          }

          @for (group of filteredGroups(); track group.hostGroup) {
            @for (node of group.workers; track node.worker.id) {
              <div class="space-y-0.5">
                <!-- Machine Row -->
                <div class="group flex items-center justify-between gap-1 rounded-md px-2 py-1.5 hover:bg-[#1e2026] transition">
                  <button
                    type="button"
                    (click)="toggleWorker(node.worker.id)"
                    class="flex items-center gap-2 min-w-0 flex-1 text-left cursor-pointer"
                  >
                    <app-icon
                      [name]="workerIcon(node.worker)"
                      [size]="13"
                      class="text-[#8a8c94] group-hover:text-[#b4b6be]"
                    />
                    <span class="text-[12px] text-[#d4d4d8] truncate">{{ node.worker.name }}</span>
                    @if (node.worker.isolation === 'container') {
                      <span class="text-[9px] font-mono-ide text-[#86888f] shrink-0">ctr</span>
                    } @else if (node.worker.isolation === 'vm') {
                      <span class="text-[9px] font-mono-ide text-[#86888f] shrink-0">vm</span>
                    }
                    @if (hasVersionMismatch(node.worker)) {
                      <span
                        role="img"
                        class="rounded px-1 text-[9px] font-mono-ide leading-[14px] text-[#e6b566] bg-[#d9a043]/15 shrink-0"
                        [ksTooltip]="versionMismatchLabel(node.worker)"
                        [attr.aria-label]="versionMismatchLabel(node.worker)"
                        data-testid="version-mismatch-badge"
                      >ver</span>
                    }
                  </button>

                  <div class="flex items-center gap-1 shrink-0">
                    <button
                      type="button"
                      (click)="openNewSpace.emit(node.worker)"
                      ksTooltip="New space on {{ node.worker.name }}" attr.aria-label="New space on {{ node.worker.name }}"
                      class="ks-icon-btn ks-icon-btn-sm opacity-70 group-hover:opacity-100"
                    >
                      <app-icon name="plus" [size]="13" />
                    </button>
                    <button
                      type="button"
                      (click)="$event.stopPropagation(); openMachineDetails.emit(node.worker)"
                      ksTooltip="Machine details &amp; remove {{ node.worker.name }}" attr.aria-label="Machine details &amp; remove {{ node.worker.name }}"
                      class="ks-icon-btn ks-icon-btn-sm opacity-70 group-hover:opacity-100"
                    >
                      <app-icon name="info" [size]="13" />
                    </button>
                    @if (node.worker.status !== 'online' && node.worker.launch_state === 'failed') {
                      <button
                        type="button"
                        (click)="$event.stopPropagation(); retryStart(node.worker)"
                        [ksTooltip]="launchFailedLabel(node.worker)"
                        [attr.aria-label]="launchFailedLabel(node.worker)"
                        class="ks-icon-btn ks-icon-btn-sm text-[var(--ks-red)]"
                        data-testid="launch-retry"
                      >
                        <app-icon name="refresh" [size]="12" />
                      </button>
                    }
                    @if (node.worker.status !== 'online' && node.worker.launch_state === 'starting') {
                      <span
                        role="img"
                        class="h-2 w-2 rounded-full border-[1.5px] border-[var(--ks-blue)] border-t-transparent ks-ring-spin mx-0.5"
                        ksTooltip="Starting konsertd on {{ node.worker.name }}"
                        attr.aria-label="Starting konsertd on {{ node.worker.name }}"
                        data-testid="launch-starting"
                      ></span>
                    } @else {
                      <span
                        class="h-1.5 w-1.5 rounded-full mx-0.5"
                        [class.bg-[#69b879]]="node.worker.status === 'online' && node.effectiveStatus !== 'waiting_input'"
                        [class.bg-[#d9a043]]="node.effectiveStatus === 'waiting_input'"
                        [class.bg-[var(--ks-red)]]="node.worker.status !== 'online' && node.worker.launch_state === 'failed'"
                        [class.bg-[#484a52]]="node.worker.status !== 'online' && node.worker.launch_state !== 'failed'"
                      ></span>
                    }
                    <button
                      type="button"
                      (click)="toggleWorker(node.worker.id)"
                      [attr.aria-label]="(isCollapsed(node.worker.id) ? 'Expand ' : 'Collapse ') + node.worker.name"
                      [attr.aria-expanded]="!isCollapsed(node.worker.id)"
                      class="ks-icon-btn ks-icon-btn-sm transition-transform duration-150"
                      [class.-rotate-90]="isCollapsed(node.worker.id)"
                    >
                      <app-icon name="chevron-down" [size]="13" />
                    </button>
                  </div>
                </div>

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
                            <span class="text-[10px] font-mono-ide text-[#86888f] truncate max-w-[70px]">
                              {{ sp.branch }}
                            </span>
                          }
                          <button
                            type="button"
                            (click)="$event.stopPropagation(); deleteSpace(sp)"
                            ksTooltip="Remove space" aria-label="Remove space"
                            class="ks-icon-btn ks-icon-btn-sm ks-btn-danger opacity-0 group-hover/sp:opacity-100"
                          >
                            <app-icon name="close" [size]="13" />
                          </button>
                        </div>
                      </div>
                    }

                    @if (node.worker.status === 'online') {
                      <button
                        type="button"
                        (click)="openNewSpace.emit(node.worker)"
                        class="w-full flex items-center gap-2 rounded-md px-2 py-1.5 text-left text-[11.5px] text-[#86888f] hover:text-[#a1a3ab] hover:bg-[#1c1e24] transition cursor-pointer"
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
        }
      </div>

      <!-- Herdr-style Live Agents Roster (Lower Section) -->
      <div class="h-[36%] min-h-[140px] max-h-[260px] flex flex-col border-t border-white/[0.06] px-2.5 pt-2.5 pb-1.5 shrink-0">
        <div class="flex items-center justify-between px-1.5 pb-1.5 text-[11px] font-medium text-[#8a8c94] shrink-0">
          <span>Agents</span>
          <app-icon name="clock" [size]="12" class="text-[#86888f]" />
        </div>

        <div class="flex-1 overflow-y-auto space-y-0.5">
          @if (filteredRoster().length === 0) {
            <div class="px-1.5 py-2 text-[11px] text-[#86888f] leading-relaxed">
              No active or recent agents yet. Open a Space or ask the Orchestrator to start a session.
            </div>
          } @else {
            @if (workingRoster().length > 0) {
              <div class="px-1.5 pt-0.5 pb-1 text-[10px] text-[#86888f]">
                Working {{ workingRoster().length }}
              </div>
              @for (item of workingRoster(); track item.task.id) {
                <button
                  type="button"
                  (click)="selectTask(item.task, fleet.workersById()[item.task.worker_id])"
                  class="w-full flex items-center justify-between gap-2 rounded-md px-2 py-[7px] text-left transition cursor-pointer"
                  [class.bg-[#24262e]]="tabs.activeTabId() === item.task.id"
                  [class.hover:bg-[#1e2026]]="tabs.activeTabId() !== item.task.id"
                >
                  <div class="flex items-center gap-2 min-w-0">
                    <app-icon [name]="item.task.agent" [size]="13" />
                    <span class="text-[12px] text-[#e4e4e7] truncate">{{ item.label }}</span>
                    @if (notifications.isUnread(item.task.id)) {
                      <span class="h-1.5 w-1.5 rounded-full bg-[#d9a043] shrink-0" role="img" aria-label="Unread update" ksTooltip="Unread update"></span>
                    }
                  </div>

                  <div class="flex items-center gap-1.5 shrink-0 text-[10px] font-mono-ide text-[#86888f]">
                    <span class="max-w-[68px] truncate">{{ item.workerName }}</span>
                    <span>{{ elapsedLabel(item.task) }}</span>
                    @if (item.task.status === 'waiting_input') {
                      <span class="h-2 w-2 rounded-full bg-[#d9a043]" role="img" aria-label="Needs you" ksTooltip="Needs you"></span>
                    } @else {
                      <span class="h-2.5 w-2.5 rounded-full border-[1.5px] border-[#60a5fa] border-t-transparent ks-ring-spin" role="img" aria-label="Working" ksTooltip="Working"></span>
                    }
                  </div>
                </button>
              }
            }

            @if (idleRoster().length > 0) {
              <div class="px-1.5 pt-1.5 pb-1 text-[10px] text-[#86888f]">
                Idle {{ idleRoster().length }}
              </div>
              @for (item of idleRoster(); track item.task.id) {
                <button
                  type="button"
                  (click)="selectTask(item.task, fleet.workersById()[item.task.worker_id])"
                  class="w-full flex items-center justify-between gap-2 rounded-md px-2 py-[7px] text-left transition cursor-pointer"
                  [class.bg-[#24262e]]="tabs.activeTabId() === item.task.id"
                  [class.hover:bg-[#1e2026]]="tabs.activeTabId() !== item.task.id"
                >
                  <div class="flex items-center gap-2 min-w-0">
                    <app-icon [name]="item.task.agent" [size]="13" />
                    <span class="text-[12px] text-[#d4d4d8] truncate">{{ item.label }}</span>
                  </div>

                  <div class="flex items-center gap-1.5 shrink-0 text-[10px] font-mono-ide text-[#86888f]">
                    <span class="max-w-[68px] truncate">{{ item.workerName }}</span>
                    <span>{{ elapsedLabel(item.task) }}</span>
                    <span class="h-2 w-2 rounded-full border border-[#5e6068]" role="img" aria-label="Idle" ksTooltip="Idle"></span>
                  </div>
                </button>
              }
            }

            @if (recentRoster().length > 0) {
              <div class="px-1.5 pt-1.5 pb-1 text-[10px] text-[#86888f]">
                Recent
              </div>
              @for (item of recentRoster(); track item.task.id) {
                <button
                  type="button"
                  (click)="selectTask(item.task, fleet.workersById()[item.task.worker_id])"
                  class="w-full flex items-center justify-between gap-2 rounded-md px-2 py-[7px] text-left transition cursor-pointer"
                  [class.bg-[#24262e]]="tabs.activeTabId() === item.task.id"
                  [class.hover:bg-[#1e2026]]="tabs.activeTabId() !== item.task.id"
                >
                  <div class="flex items-center gap-2 min-w-0">
                    <app-icon [name]="item.task.agent" [size]="13" />
                    <span class="text-[12px] text-[#b4b6be] truncate">{{ item.label }}</span>
                  </div>

                  <div class="flex items-center gap-1.5 shrink-0 text-[10px] font-mono-ide text-[#86888f]">
                    <span class="max-w-[68px] truncate">{{ item.workerName }}</span>
                    <span>{{ elapsedLabel(item.task) }}</span>
                    @if (item.task.status === 'failed') {
                      <span class="h-2 w-2 rounded-full bg-[#e05d5d]" role="img" aria-label="Failed" ksTooltip="Failed"></span>
                    } @else {
                      <span class="h-2 w-2 rounded-full border border-[#484a52]" role="img" aria-label="Completed" ksTooltip="Completed"></span>
                    }
                  </div>
                </button>
              }
            }
          }
        </div>
      </div>

      <!-- Herdr-style Bottom Pinned Controls -->
      <div class="border-t border-white/[0.06] px-2.5 py-2.5 space-y-0.5 shrink-0">
        <button
          type="button"
          (click)="openSettings.emit()"
          class="w-full flex items-center justify-between rounded-md px-2 py-1.5 text-[12.5px] text-[#9a9ca4] hover:text-[#ececed] hover:bg-[#1e2026] transition cursor-pointer"
        >
          <span class="inline-flex items-center gap-2">
            <app-icon name="settings" [size]="13" />
            <span>Settings</span>
          </span>
          @if (auth.authRequired()) {
            <span class="text-[10px] text-[#86888f]">{{ auth.user() }}</span>
          }
        </button>
        <button
          type="button"
          (click)="openAddMachine.emit()"
          class="w-full flex items-center gap-2 rounded-md px-2 py-1.5 text-[12.5px] text-[#9a9ca4] hover:text-[#ececed] hover:bg-[#1e2026] transition cursor-pointer"
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
  readonly worktreeSelected = output<{ repo: Repo; worktree: Worktree | null }>();
  readonly openMachineDetails = output<Worker>();
  readonly openAddMachine = output<void>();
  readonly openSettings = output<void>();

  readonly orchestratorId = ORCHESTRATOR_TAB_ID;
  readonly fleet = inject(FleetStore);
  readonly tabs = inject(TabsStore);
  readonly auth = inject(AuthService);
  readonly notifications = inject(NotificationService);
  private readonly api = inject(ApiClientService);

  readonly navMode = signal<'workspaces' | 'machines'>('workspaces');
  readonly selectedWorkspaceId = signal<string>('');
  readonly query = signal('');

  // Inline create form state for Space > Project > Repo > Worktree
  readonly createMode = signal<'workspace' | 'project' | 'repo' | 'worktree' | null>(null);
  readonly formParentId = signal('');
  readonly formName = signal('');
  readonly formPath = signal('');
  readonly formWorkerId = signal('');

  private readonly collapsedWorkers = signal<Record<string, boolean>>({});
  private readonly collapsedProjects = signal<Record<string, boolean>>({});
  private readonly nowTick = signal(Date.now());
  private timer: ReturnType<typeof setInterval> | null = null;

  readonly activeWorkspaceId = computed(() => {
    const sel = this.selectedWorkspaceId();
    const list = this.fleet.workspaces();
    if (sel && list.some((w) => w.id === sel)) return sel;
    return list.find((w) => w.is_default)?.id || list[0]?.id || 'space-default';
  });

  readonly filteredProjects = computed(() => {
    const wsId = this.activeWorkspaceId();
    const q = this.query().trim().toLowerCase();
    const projects = this.fleet.projects().filter((p) => p.workspace_id === wsId);
    if (!q) return projects;
    return projects.filter((p) => {
      if (p.name.toLowerCase().includes(q)) return true;
      const repos = this.fleet.repos().filter((r) => r.project_id === p.id);
      return repos.some(
        (r) =>
          r.name.toLowerCase().includes(q) ||
          r.path.toLowerCase().includes(q) ||
          this.fleet.worktrees().some((wt) => wt.repo_id === r.id && (wt.branch || '').toLowerCase().includes(q))
      );
    });
  });

  readonly defaultUncategorizedSessions = computed(() => {
    const q = this.query().trim().toLowerCase();
    return this.fleet
      .tasks()
      .filter(
        (t) =>
          t.origin !== 'scratch' &&
          (!t.repo_id || t.origin === 'external' || t.project_id === DEFAULT_PROJECT_ID)
      )
      .filter((t) => !q || t.agent.toLowerCase().includes(q) || (t.workdir || '').toLowerCase().includes(q))
      .slice(0, 8);
  });

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

  readonly workingRoster = computed(() =>
    this.filteredRoster().filter((i) => i.stateGroup === 'working')
  );

  readonly idleRoster = computed(() =>
    this.filteredRoster().filter((i) => i.stateGroup === 'idle')
  );

  readonly recentRoster = computed(() =>
    this.filteredRoster().filter((i) => i.stateGroup === 'recent')
  );

  ngOnInit(): void {
    this.timer = setInterval(() => this.nowTick.set(Date.now()), 5000);
  }

  ngOnDestroy(): void {
    if (this.timer) clearInterval(this.timer);
  }

  baseName(p: string): string {
    return basenamePath(p);
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

  toggleProject(projectId: string): void {
    this.collapsedProjects.update((m) => ({ ...m, [projectId]: !m[projectId] }));
  }

  isProjectCollapsed(projectId: string): boolean {
    return !!this.collapsedProjects()[projectId];
  }

  reposForProject(projectId: string): Repo[] {
    const q = this.query().trim().toLowerCase();
    const all = this.fleet.repos().filter((r) => r.project_id === projectId);
    if (!q) return all;
    return all.filter(
      (r) =>
        r.name.toLowerCase().includes(q) ||
        r.path.toLowerCase().includes(q) ||
        this.worktreesForRepo(r.id).length > 0
    );
  }

  worktreesForRepo(repoId: string): Worktree[] {
    return this.fleet.worktrees().filter((wt) => wt.repo_id === repoId);
  }

  toggleCreateForm(mode: 'workspace' | 'project'): void {
    if (this.createMode() === mode) {
      this.createMode.set(null);
      return;
    }
    this.formName.set('');
    this.formPath.set('');
    this.formParentId.set('');
    this.createMode.set(mode);
  }

  openAddRepoForm(projectId: string): void {
    this.formParentId.set(projectId);
    this.formName.set('');
    this.formPath.set('~');
    const resolved = this.fleet.resolveSessionDefaults();
    this.formWorkerId.set(resolved.machine_id || this.fleet.workers()[0]?.id || '');
    this.createMode.set('repo');
  }

  openAddWorktreeForm(repoId: string, repoPath: string): void {
    this.formParentId.set(repoId);
    this.formName.set('');
    this.formPath.set(repoPath);
    this.createMode.set('worktree');
  }

  async submitCreateWorkspace(): Promise<void> {
    const name = this.formName().trim();
    if (!name) return;
    try {
      const ws = await this.api.createWorkspace(name);
      this.fleet.upsertWorkspace(ws);
      this.selectedWorkspaceId.set(ws.id);
      this.createMode.set(null);
    } catch {
      // ignore
    }
  }

  async submitCreateProject(): Promise<void> {
    const name = this.formName().trim();
    if (!name) return;
    try {
      const proj = await this.api.createProject({ workspace_id: this.activeWorkspaceId(), name });
      this.fleet.upsertProject(proj);
      this.createMode.set(null);
    } catch {
      // ignore
    }
  }

  async submitCreateRepo(projectId: string): Promise<void> {
    const path = this.formPath().trim();
    const workerId = this.formWorkerId();
    if (!path || !workerId) return;
    try {
      const repo = await this.api.createRepo({
        project_id: projectId,
        worker_id: workerId,
        path,
        name: this.formName().trim() || undefined,
      });
      this.fleet.upsertRepo(repo);
      await this.fleet.refresh();
      this.createMode.set(null);
    } catch {
      // ignore
    }
  }

  async submitCreateWorktree(repoId: string): Promise<void> {
    const branch = this.formName().trim();
    if (!branch) return;
    try {
      const wt = await this.api.createWorktree({
        repo_id: repoId,
        branch,
        path: this.formPath().trim() || undefined,
      });
      this.fleet.upsertWorktree(wt);
      this.createMode.set(null);
    } catch {
      // ignore
    }
  }

  async deleteProject(proj: Project): Promise<void> {
    if (proj.is_default) return;
    if (!window.confirm(`Delete project "${proj.name}" and its repo links?`)) return;
    try {
      await this.api.deleteProject(proj.id);
      this.fleet.removeProject(proj.id);
    } catch {
      // ignore
    }
  }

  async deleteRepo(repo: Repo): Promise<void> {
    try {
      await this.api.deleteRepo(repo.id);
      this.fleet.removeRepo(repo.id);
    } catch {
      // ignore
    }
  }

  async deleteWorktree(wt: Worktree): Promise<void> {
    try {
      await this.api.deleteWorktree(wt.id);
      this.fleet.removeWorktree(wt.id);
    } catch {
      // ignore
    }
  }

  openWorktreeSession(repo: Repo, worktree: Worktree | null): void {
    this.worktreeSelected.emit({ repo, worktree });
    this.taskSelected.emit();
  }

  /** True when the worker's konsertd build differs from the hub's (dev/unknown never do). */
  launchFailedLabel(w: Worker): string {
    return `Couldn't start konsertd on ${w.name}: ${w.launch_error || 'unknown error'}. Click to retry.`;
  }

  retryStart(w: Worker): void {
    void this.fleet.retryStart(w.id);
  }

  hasVersionMismatch(w: Worker): boolean {
    return isVersionMismatch(this.fleet.controllerVersion(), w.version);
  }

  versionMismatchLabel(w: Worker): string {
    return `Version mismatch: ${w.name} runs konsertd ${w.version}, hub is ${this.fleet.controllerVersion()}`;
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

  async deleteWorker(worker: Worker): Promise<void> {
    const activeSpaces = this.fleet.spaces().filter((s) => s.worker_id === worker.id);
    if (worker.status === 'online' || activeSpaces.length > 0) {
      const ok = window.confirm(`Remove machine "${worker.name}" from Konsert?`);
      if (!ok) return;
    }
    try {
      await this.api.deleteWorker(worker.id);
      this.fleet.removeWorker(worker.id);
      const curSpace = this.tabs.activeSpaceId();
      if (curSpace && activeSpaces.some((s) => s.id === curSpace)) {
        this.tabs.activeSpaceId.set(null);
      }
    } catch {
      // ignore
    }
  }

  selectTask(task: Task, worker?: Worker): void {
    this.tabs.openTaskTab(task, worker?.name);
    this.notifications.markRead(task.id);
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

import { Component, OnDestroy, computed, effect, inject, input, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { GitInspectResult, GitSummary } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { FleetStore, basenamePath } from '../core/state/fleet.store';
import { TabsStore } from '../core/state/tabs.store';
import { TelemetryStore } from '../core/state/telemetry.store';
import { IconComponent } from '../shared/icon.component';

export type InspectorPanel = 'agent' | 'git' | 'checks';

@Component({
  selector: 'app-telemetry-drawer',
  standalone: true,
  imports: [FormsModule, IconComponent],
  template: `
    <div class="flex flex-col h-full w-full bg-[#16171a] text-[#d4d4d8]">
      <!-- Panel Header -->
      <div class="flex items-center justify-between px-3 py-2.5 border-b border-[#26282e] shrink-0">
        @switch (panel()) {
          @case ('agent') {
            <div>
              <div class="text-[12px] font-semibold text-[#ececed]">Agent Activity</div>
              <div class="text-[10px] text-[#787a82]">
                {{ activeTask() ? 'Live telemetry & session history' : 'Fleet session history' }}
              </div>
            </div>
          }
          @case ('git') {
            <div class="flex items-center gap-2">
              <span class="text-[12px] font-semibold text-[#ececed]">Changes</span>
              <span class="rounded bg-[#22242a] px-1.5 py-0.5 text-[10px] font-mono-ide text-[#8e9098]">
                {{ liveFileCount() }} files
              </span>
            </div>
            <div class="flex items-center gap-2 font-mono-ide text-[11px]">
              @if (liveGit(); as lg) {
                <span class="text-[#69b879]">+{{ lg.added }}</span>
                <span class="text-[#e05d5d]">-{{ lg.deleted }}</span>
              }
              <button
                type="button"
                (click)="refreshLiveGit()"
                title="Refresh Git status"
                class="rounded px-1.5 py-0.5 text-[10px] font-sans text-[#8e9098] hover:text-[#ececed] hover:bg-[#24262c] transition cursor-pointer"
              >
                {{ gitLoading() ? '...' : '↻' }}
              </button>
            </div>
          }
          @case ('checks') {
            <div>
              <div class="text-[12px] font-semibold text-[#ececed]">Checks & Runtime</div>
              <div class="text-[10px] text-[#787a82]">Execution posture & exit signals</div>
            </div>
          }
        }

        @if (panel() !== 'git' && activeTask(); as t) {
          <span class="font-mono-ide text-[10px] text-[#686a72]">{{ t.id.slice(0, 8) }}</span>
        }
      </div>

      <!-- PANEL 1: AGENT TELEMETRY + ORCA-STYLE SESSION HISTORY -->
      @if (panel() === 'agent') {
        <div class="ks-panel-enter flex-1 overflow-y-auto p-3 space-y-3.5">
          @if (activeTask(); as t) {
            @if (t.summary) {
              <div class="rounded-lg border border-[#282a30] bg-[#1b1c20] p-2.5 space-y-1">
                <div class="text-[10px] font-semibold uppercase tracking-wider text-[#8e9098]">Summary</div>
                <p class="text-[12px] text-[#e4e4e7] whitespace-pre-wrap leading-relaxed">{{ t.summary }}</p>
              </div>
            }

            @if (t.status === 'running' || t.status === 'waiting_input') {
              <form (ngSubmit)="sendSteer(t.id)" class="flex gap-1.5">
                <input
                  type="text"
                  name="steer"
                  [ngModel]="steerText()"
                  (ngModelChange)="steerText.set($event)"
                  placeholder="Steer agent..."
                  class="flex-1 rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-1.5 text-[12px] text-[#ececed] placeholder-[#5e6068] focus:border-[#4a668a] focus:outline-none"
                />
                <button
                  type="submit"
                  [disabled]="!steerText().trim()"
                  class="rounded-md border border-[#2c2e36] bg-[#22242a] hover:bg-[#2a2c34] px-2.5 py-1.5 text-[11px] font-medium text-[#ececed] cursor-pointer disabled:opacity-40"
                >
                  Send
                </button>
              </form>
            }

            <div class="space-y-1.5">
              <div class="text-[11px] font-medium text-[#787a82]">
                Milestones ({{ events().length }})
              </div>
              @if (events().length === 0) {
                <div class="text-[11px] text-[#5e6068]">Waiting for structured agent milestones...</div>
              }
              @for (ev of events(); track ev.id) {
                <div class="rounded-md border border-[#24262c] bg-[#191a1e] px-2.5 py-1.5">
                  <div class="flex items-center justify-between gap-2 mb-0.5">
                    <span
                      class="text-[10px] font-mono-ide uppercase font-medium"
                      [class.text-[#60a5fa]]="ev.kind === 'tool_call'"
                      [class.text-[#69b879]]="ev.kind === 'file_edit' || ev.kind === 'milestone'"
                      [class.text-[#d9a043]]="ev.kind === 'waiting_input' || ev.kind === 'control'"
                      [class.text-[#e05d5d]]="ev.kind === 'error'"
                      [class.text-[#8e9098]]="ev.kind === 'exit' || ev.kind === 'summary'"
                    >
                      {{ ev.kind }}
                    </span>
                    <span class="text-[10px] font-mono-ide text-[#5e6068]">{{ formatTime(ev.created_at) }}</span>
                  </div>
                  <div class="text-[11px] text-[#d4d4d8] break-words font-mono-ide">{{ ev.message }}</div>
                </div>
              }
            </div>
          }

          <div class="space-y-2" [class.pt-2]="!!activeTask()" [class.border-t]="!!activeTask()" [class.border-[#26282e]]="!!activeTask()">
            <div class="relative">
              <app-icon name="search" [size]="12" class="absolute left-2.5 top-1/2 -translate-y-1/2 text-[#686a72]" />
              <input
                type="text"
                [ngModel]="sessionFilter()"
                (ngModelChange)="sessionFilter.set($event)"
                placeholder="Search sessions..."
                class="w-full rounded-md border border-[#2a2c33] bg-[#111214] pl-7 pr-2.5 py-1.5 text-[12px] text-[#e4e4e7] placeholder-[#5e6068] focus:border-[#4a668a] focus:outline-none"
              />
            </div>

            <div class="flex items-center justify-between text-[11px] text-[#686a72] px-0.5">
              <span>{{ filteredHistory().length }} sessions</span>
              <span>Recent</span>
            </div>

            <div class="space-y-1.5">
              @for (item of filteredHistory(); track item.id) {
                <button
                  type="button"
                  (click)="openSession(item.id)"
                  class="w-full text-left rounded-lg border border-[#24262c] bg-[#191a1e] hover:border-[#343741] hover:bg-[#1e2026] p-2.5 transition cursor-pointer space-y-1"
                >
                  <div class="text-[12px] font-medium text-[#ececed] truncate">
                    {{ item.prompt || basename(item.workdir) || item.agent }}
                  </div>
                  @if (item.summary) {
                    <div class="text-[11px] text-[#8e9098] line-clamp-2 leading-snug">
                      {{ item.summary }}
                    </div>
                  }
                  <div class="flex items-center justify-between pt-0.5 text-[10px] text-[#686a72]">
                    <span class="inline-flex items-center gap-1.5">
                      <app-icon [name]="item.agent" [size]="11" />
                      <span>{{ item.agent }}</span>
                      <span>·</span>
                      <span>{{ item.status }}</span>
                    </span>
                    <span class="font-mono-ide">{{ formatRelative(item.created_at) }}</span>
                  </div>
                </button>
              }
            </div>
          </div>
        </div>
      }

      <!-- PANEL 2: LIVE GIT CHANGES, EXPANDABLE DIFFS & COMMIT BOX (Herdr style) -->
      @if (panel() === 'git') {
        <div class="ks-panel-enter flex-1 flex flex-col min-h-0">
          @if (!hasGitTarget()) {
            <div class="p-3 text-[12px] text-[#686a72]">
              Select a machine Space or workspace session tab to inspect live Git changes, diffs, and commit directly.
            </div>
          } @else {
            <div class="flex-1 overflow-y-auto p-3 space-y-3">
              @if (gitError()) {
                <div class="rounded-md border border-[#e05d5d]/30 bg-[#e05d5d]/10 px-2.5 py-2 text-[11px] text-[#f09595]">
                  {{ gitError() }}
                </div>
              }

              @if (liveGit(); as lg) {
                <!-- Branch & Worktree info -->
                <div class="rounded-lg border border-[#26282e] bg-[#191a1e] p-2.5 space-y-1.5 text-[12px]">
                  <div class="flex items-center justify-between gap-2">
                    <span class="text-[#787a82]">Branch</span>
                    <span class="inline-flex items-center gap-1 font-mono-ide text-[11px] text-[#e4e4e7]">
                      <app-icon name="git-branch" [size]="11" />
                      {{ lg.branch || '(not a git repo)' }}
                    </span>
                  </div>
                  @if (lg.effective_dir) {
                    <div class="pt-1 border-t border-[#24262c] font-mono-ide text-[10px] text-[#787a82] break-all">
                      {{ lg.effective_dir }}
                    </div>
                  }
                  @if (lg.is_worktree) {
                    <div class="pt-1.5 border-t border-[#24262c] flex items-center justify-between">
                      <span class="text-[10px] text-[#8e9098]">Isolated worktree</span>
                      <button
                        type="button"
                        (click)="cleanupWorktree(lg.effective_dir || '')"
                        class="rounded border border-[#2e3038] hover:border-[#e05d5d]/50 px-2 py-0.5 text-[10px] text-[#a1a3ab] hover:text-[#f09595] transition cursor-pointer"
                      >
                        Clean up worktree
                      </button>
                    </div>
                  }
                </div>

                @if (!lg.is_git_repo) {
                  <div class="text-[12px] text-[#686a72] px-1">Directory is not a git repository.</div>
                } @else {
                  <!-- Changed Files with Expandable Unified Diffs -->
                  @if (lg.files.length === 0) {
                    <div class="text-[12px] text-[#686a72] px-1 py-2">No uncommitted changes</div>
                  } @else {
                    <div class="space-y-1.5">
                      @for (f of lg.files; track f.path) {
                        <div class="rounded-lg border border-[#26282e] bg-[#191a1e] overflow-hidden">
                          <button
                            type="button"
                            (click)="toggleFileDiff(f.path)"
                            class="w-full flex items-center justify-between gap-2 px-2.5 py-1.5 text-left hover:bg-[#202228] transition cursor-pointer"
                          >
                            <div class="flex items-center gap-2 min-w-0">
                              <span
                                class="font-mono-ide text-[10px] font-semibold w-4 text-center shrink-0"
                                [class.text-[#d9a043]]="f.status === 'M'"
                                [class.text-[#69b879]]="f.status === 'A' || f.status === '??'"
                                [class.text-[#e05d5d]]="f.status === 'D'"
                              >
                                {{ f.status }}
                              </span>
                              <span class="font-mono-ide text-[11px] text-[#e4e4e7] truncate">{{ f.path }}</span>
                            </div>
                            <span class="font-mono-ide text-[10px] shrink-0 space-x-1">
                              @if (f.added > 0) {
                                <span class="text-[#69b879]">+{{ f.added }}</span>
                              }
                              @if (f.deleted > 0) {
                                <span class="text-[#e05d5d]">-{{ f.deleted }}</span>
                              }
                            </span>
                          </button>

                          @if (isDiffExpanded(f.path) && f.diff) {
                            <div class="border-t border-[#24262c] bg-[#111214] p-2 overflow-x-auto max-h-60 font-mono-ide text-[10px] leading-relaxed">
                              @for (line of diffLines(f.diff); track $index) {
                                <div
                                  class="whitespace-pre"
                                  [class.text-[#69b879]]="line.startsWith('+') && !line.startsWith('+++')"
                                  [class.bg-[#69b879]/10]="line.startsWith('+') && !line.startsWith('+++')"
                                  [class.text-[#e05d5d]]="line.startsWith('-') && !line.startsWith('---')"
                                  [class.bg-[#e05d5d]/10]="line.startsWith('-') && !line.startsWith('---')"
                                  [class.text-[#60a5fa]]="line.startsWith('@@')"
                                  [class.text-[#8e9098]]="!line.startsWith('+') && !line.startsWith('-') && !line.startsWith('@@')"
                                >{{ line }}</div>
                              }
                            </div>
                          }
                        </div>
                      }
                    </div>
                  }

                  <!-- Recent Commits -->
                  @if (lg.commits?.length) {
                    <div class="space-y-1 pt-1">
                      <div class="text-[11px] font-medium text-[#787a82]">Recent Commits</div>
                      <div class="rounded-lg border border-[#26282e] bg-[#191a1e] p-2 space-y-1 font-mono-ide text-[10px] text-[#a1a3ab]">
                        @for (c of lg.commits; track c) {
                          <div class="truncate">{{ c }}</div>
                        }
                      </div>
                    </div>
                  }
                }
              } @else if (gitSummary(); as gs) {
                <!-- Fallback static summary for completed tasks when worker is offline -->
                @if (gs.diff_stat) {
                  <pre class="rounded-lg border border-[#26282e] bg-[#111214] p-2.5 font-mono-ide text-[10px] text-[#b4b6be] overflow-x-auto">{{ gs.diff_stat }}</pre>
                }
              }
            </div>

            <!-- Herdr-style Bottom-Pinned Commit Bar -->
            @if (liveGit()?.is_git_repo) {
              <div class="border-t border-[#26282e] p-2.5 bg-[#151619] space-y-1.5 shrink-0">
                @if (commitNotice()) {
                  <div class="text-[11px] text-[#69b879] font-mono-ide truncate">{{ commitNotice() }}</div>
                }
                <form (ngSubmit)="submitCommit()" class="flex items-center gap-1.5">
                  <input
                    type="text"
                    name="commitMsg"
                    [ngModel]="commitMessage()"
                    (ngModelChange)="commitMessage.set($event)"
                    placeholder="Commit message"
                    class="flex-1 rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-1.5 text-[12px] text-[#ececed] placeholder-[#5e6068] focus:border-[#4a668a] focus:outline-none"
                  />
                  <button
                    type="submit"
                    [disabled]="committing() || !commitMessage().trim() || (liveGit()?.files?.length ?? 0) === 0"
                    class="rounded-md border border-[#2c2e36] bg-[#24262d] hover:bg-[#2f323b] disabled:opacity-40 px-3 py-1.5 text-[12px] font-medium text-[#ececed] transition cursor-pointer shrink-0"
                  >
                    {{ committing() ? '...' : 'Commit' }}
                  </button>
                </form>
              </div>
            }
          }
        </div>
      }

      <!-- PANEL 3: CHECKS & RUNTIME POSTURE -->
      @if (panel() === 'checks') {
        <div class="ks-panel-enter flex-1 overflow-y-auto p-3 space-y-3">
          @if (!activeTask()) {
            <div class="text-[12px] text-[#686a72]">
              Select a workspace session tab to inspect execution status, permission posture, and host details.
            </div>
          } @else {
            @let t = activeTask()!;
            @let w = activeWorker();
            <div class="rounded-lg border border-[#26282e] bg-[#191a1e] divide-y divide-[#23252c] text-[12px]">
              <div class="flex items-center justify-between px-3 py-2">
                <span class="text-[#787a82]">Status</span>
                <span class="font-mono-ide text-[11px] text-[#ececed]">{{ t.status }}</span>
              </div>
              <div class="flex items-center justify-between px-3 py-2">
                <span class="text-[#787a82]">Origin</span>
                <span class="font-mono-ide text-[11px] text-[#d4d4d8]">{{ t.origin || 'orchestrator' }}</span>
              </div>
              <div class="flex items-center justify-between px-3 py-2">
                <span class="text-[#787a82]">Mode</span>
                <span class="font-mono-ide text-[11px] text-[#d4d4d8]">{{ t.mode }}</span>
              </div>
              <div class="flex items-center justify-between px-3 py-2">
                <span class="text-[#787a82]">Permissions</span>
                <span
                  class="font-mono-ide text-[11px]"
                  [class.text-[#69b879]]="t.permission_mode === 'full_access'"
                  [class.text-[#d9a043]]="t.permission_mode === 'sandboxed'"
                >
                  {{ t.permission_mode }}
                </span>
              </div>
              @if (t.exit_code !== null && t.exit_code !== undefined) {
                <div class="flex items-center justify-between px-3 py-2">
                  <span class="text-[#787a82]">Exit Code</span>
                  <span class="font-mono-ide text-[11px]" [class.text-[#69b879]]="t.exit_code === 0" [class.text-[#e05d5d]]="t.exit_code !== 0">
                    {{ t.exit_code }}
                  </span>
                </div>
              }
              @if (w) {
                <div class="flex items-center justify-between px-3 py-2">
                  <span class="text-[#787a82]">Host</span>
                  <span class="font-mono-ide text-[11px] text-[#d4d4d8]">{{ w.name }} ({{ w.os }}/{{ w.arch }})</span>
                </div>
                <div class="flex items-center justify-between px-3 py-2">
                  <span class="text-[#787a82]">Isolation</span>
                  <span class="font-mono-ide text-[11px] uppercase text-[#a1a3ab]">{{ w.isolation }}</span>
                </div>
              }
            </div>

            @if (errorEvents().length > 0) {
              <div class="space-y-1.5">
                <div class="text-[11px] font-medium text-[#e05d5d]">Errors ({{ errorEvents().length }})</div>
                @for (ev of errorEvents(); track ev.id) {
                  <div class="rounded-md border border-[#e05d5d]/30 bg-[#e05d5d]/10 px-2.5 py-1.5 font-mono-ide text-[11px] text-[#f09595]">
                    {{ ev.message }}
                  </div>
                }
              </div>
            }
          }
        </div>
      }
    </div>
  `,
})
export class TelemetryDrawerComponent implements OnDestroy {
  readonly panel = input<InspectorPanel>('agent');

  private readonly tabs = inject(TabsStore);
  private readonly fleet = inject(FleetStore);
  private readonly telemetry = inject(TelemetryStore);
  private readonly api = inject(ApiClientService);

  readonly steerText = signal('');
  readonly sessionFilter = signal('');

  // Live Git Changes state
  readonly liveGit = signal<GitInspectResult | null>(null);
  readonly gitLoading = signal(false);
  readonly gitError = signal('');
  readonly expandedDiffs = signal<Record<string, boolean>>({});
  readonly commitMessage = signal('');
  readonly committing = signal(false);
  readonly commitNotice = signal('');

  private pollTimer: ReturnType<typeof setInterval> | null = null;

  readonly activeTask = computed(() => {
    const tab = this.tabs.activeTab();
    if (!tab || tab.kind !== 'terminal' || !tab.taskId) return undefined;
    return this.fleet.tasksById()[tab.taskId];
  });

  readonly activeSpace = computed(() => {
    const sid = this.tabs.activeSpaceId();
    return sid ? this.fleet.spacesById()[sid] : undefined;
  });

  readonly hasGitTarget = computed(() => !!this.activeTask() || !!this.activeSpace());

  readonly activeWorker = computed(() => {
    const t = this.activeTask();
    return t?.worker_id ? this.fleet.workersById()[t.worker_id] : undefined;
  });

  readonly events = computed(() => {
    const t = this.activeTask();
    if (!t) return [];
    return this.telemetry.eventsByTask()[t.id] ?? [];
  });

  readonly errorEvents = computed(() =>
    this.events().filter((e) => e.kind === 'error')
  );

  readonly gitSummary = computed<GitSummary | null>(() => {
    const raw = this.activeTask()?.git_summary_json;
    if (!raw) return null;
    try {
      return JSON.parse(raw) as GitSummary;
    } catch {
      return null;
    }
  });

  readonly liveFileCount = computed(() => {
    const lg = this.liveGit();
    if (lg) return lg.files.length;
    return this.gitSummary()?.dirty_files?.length ?? 0;
  });

  readonly filteredHistory = computed(() => {
    const q = this.sessionFilter().trim().toLowerCase();
    const list = this.fleet.tasks().filter((t) => t.origin !== 'scratch');
    if (!q) return list.slice(0, 20);
    return list
      .filter(
        (t) =>
          (t.prompt || '').toLowerCase().includes(q) ||
          (t.workdir || '').toLowerCase().includes(q) ||
          (t.branch || '').toLowerCase().includes(q) ||
          (t.agent || '').toLowerCase().includes(q)
      )
      .slice(0, 20);
  });

  constructor() {
    effect(() => {
      const t = this.activeTask();
      if (t) {
        void this.telemetry.hydrateTask(t.id);
      }
    });

    // Auto-inspect live Git changes when switching to Git panel or changing active task/space.
    effect(() => {
      const p = this.panel();
      const t = this.activeTask();
      const sp = this.activeSpace();
      if (this.pollTimer) {
        clearInterval(this.pollTimer);
        this.pollTimer = null;
      }
      if (p === 'git' && (t || sp)) {
        void this.refreshLiveGit();
        this.pollTimer = setInterval(() => void this.refreshLiveGit(true), 6000);
      }
    });
  }

  async refreshLiveGit(silent = false): Promise<void> {
    const t = this.activeTask();
    const sp = this.activeSpace();
    if (!t && !sp) return;
    if (!silent) this.gitLoading.set(true);
    this.gitError.set('');
    try {
      const res = await this.api.inspectGit({
        taskId: t?.id,
        spaceId: !t ? sp?.id : undefined,
      });
      if (res.error) {
        this.gitError.set(res.error);
      } else {
        this.liveGit.set(res);
      }
    } catch (err) {
      if (!silent) {
        this.gitError.set(err instanceof Error ? err.message : 'Unable to inspect git status');
      }
    } finally {
      if (!silent) this.gitLoading.set(false);
    }
  }

  toggleFileDiff(path: string): void {
    this.expandedDiffs.update((m) => ({ ...m, [path]: !m[path] }));
  }

  isDiffExpanded(path: string): boolean {
    return !!this.expandedDiffs()[path];
  }

  diffLines(diff: string): string[] {
    return diff.split('\n');
  }

  async submitCommit(): Promise<void> {
    const msg = this.commitMessage().trim();
    const t = this.activeTask();
    const sp = this.activeSpace();
    if (!msg || (!t && !sp)) return;

    this.committing.set(true);
    this.gitError.set('');
    this.commitNotice.set('');
    try {
      const res = await this.api.commitGit({
        task_id: t?.id,
        space_id: !t ? sp?.id : undefined,
        message: msg,
        stage_all: true,
      });
      if (!res.ok) {
        this.gitError.set(res.error || 'Commit failed');
      } else {
        this.commitMessage.set('');
        this.commitNotice.set(`Committed ${res.commit_sha}`);
        setTimeout(() => this.commitNotice.set(''), 4000);
        await this.refreshLiveGit();
      }
    } catch (err) {
      this.gitError.set(err instanceof Error ? err.message : 'Commit failed');
    } finally {
      this.committing.set(false);
    }
  }

  async cleanupWorktree(workdir: string): Promise<void> {
    const t = this.activeTask();
    if (!t || !workdir) return;
    this.gitError.set('');
    try {
      const res = await this.api.cleanupWorktree({ task_id: t.id, workdir });
      if (!res.ok) {
        this.gitError.set(res.error || 'Worktree cleanup failed');
      } else {
        this.liveGit.set(null);
        this.commitNotice.set('Worktree removed');
      }
    } catch (err) {
      this.gitError.set(err instanceof Error ? err.message : 'Worktree cleanup failed');
    }
  }

  openSession(taskId: string): void {
    const task = this.fleet.tasksById()[taskId];
    if (!task) return;
    const w = this.fleet.workersById()[task.worker_id];
    this.tabs.openTaskTab(task, w?.name);
  }

  async sendSteer(taskId: string): Promise<void> {
    const text = this.steerText().trim();
    if (!text) return;
    this.steerText.set('');
    try {
      await this.api.steerTask(taskId, text, true);
    } catch {
      // ignore
    }
  }

  basename(p: string): string {
    return basenamePath(p);
  }

  formatTime(iso: string): string {
    try {
      return new Date(iso).toLocaleTimeString([], { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' });
    } catch {
      return '';
    }
  }

  formatRelative(iso: string): string {
    const secs = Math.max(0, Math.floor((Date.now() - new Date(iso).getTime()) / 1000));
    if (secs < 60) return 'Just now';
    if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
    if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`;
    return `${Math.floor(secs / 86400)}d ago`;
  }

  ngOnDestroy(): void {
    if (this.pollTimer) clearInterval(this.pollTimer);
  }
}

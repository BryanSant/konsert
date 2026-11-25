import { ChangeDetectionStrategy, Component, computed, inject, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Project, Repo, Worktree } from '../../core/models/syre.models';
import { DELIVERY_MODE_OPTIONS, MERGE_STYLE_OPTIONS } from '../../core/models/review.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { FleetStore, basenamePath } from '../../core/state/fleet.store';
import { IconComponent } from '../../shared/icon.component';
import { ModalDialogComponent } from '../../shared/modal-dialog.component';
import { TooltipDirective } from '../../shared/tooltip.directive';

/**
 * Projects and repos: workspaces, the projects in them, the repos (a path on a machine) in
 * each project and their worktrees. Live work groups sessions by these, so a session in a
 * project's repo shows under that project. Start a chat in any repo or worktree from here.
 */
@Component({
  selector: 'sy-projects-dialog',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [FormsModule, ModalDialogComponent, IconComponent, TooltipDirective],
  template: `
    <app-modal-dialog labelledBy="sy-projects-title" panelClass="w-full max-w-2xl rounded-[var(--sy-radius-xl)] bg-[var(--sy-surface-2)] p-5 max-h-[86vh] overflow-y-auto" (close)="close.emit()">
      <div class="flex items-start gap-3 mb-4">
        <div class="flex-1">
          <h2 id="sy-projects-title" class="m-0 text-[15px] font-semibold">Projects and repos</h2>
          <p class="m-0 mt-1 text-[12px] text-[var(--sy-text-secondary)]">Live work groups sessions by project: anything running in a project's repo shows under it.</p>
        </div>
        <button type="button" class="sy-icon-btn" aria-label="Close" (click)="close.emit()"><app-icon name="close" [size]="15" /></button>
      </div>

      @if (error()) {
        <p role="alert" class="m-0 mb-3 rounded-[var(--sy-radius-md)] bg-[var(--sy-red-soft)] px-3 py-2 text-[12px] text-[#f3a5a5]">{{ error() }}</p>
      }

      <div class="flex items-center gap-2 mb-3">
        <label for="sy-proj-ws" class="text-[12px] text-[var(--sy-text-secondary)]">Workspace</label>
        <select id="sy-proj-ws" class="sy-field sy-field-sm flex-1 min-w-0" [ngModel]="activeWorkspaceId()" (ngModelChange)="workspaceId.set($event)">
          @for (ws of fleet.workspaces(); track ws.id) {
            <option [value]="ws.id">{{ ws.name }}</option>
          }
        </select>
        <button type="button" class="sy-btn sy-btn-sm sy-btn-soft" (click)="startForm('workspace', '')"><app-icon name="plus" [size]="12" />Workspace</button>
        <button type="button" class="sy-btn sy-btn-sm sy-btn-soft" data-testid="projects-new" (click)="startForm('project', '')"><app-icon name="plus" [size]="12" />Project</button>
      </div>

      @if (form() === 'workspace' || form() === 'project') {
        <form class="mb-3 flex gap-2 rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-1)] p-3" (ngSubmit)="submit()">
          <label class="sy-sr-only" for="sy-proj-name">{{ form() === 'workspace' ? 'Workspace name' : 'Project name' }}</label>
          <input id="sy-proj-name" name="name" class="sy-field sy-field-sm flex-1" autofocus [placeholder]="form() === 'workspace' ? 'Workspace name (e.g. Work)' : 'Project name'" [ngModel]="name()" (ngModelChange)="name.set($event)" />
          <button type="button" class="sy-btn sy-btn-sm" (click)="form.set(null)">Cancel</button>
          <button type="submit" class="sy-btn sy-btn-sm sy-btn-primary" [disabled]="!name().trim() || busy()">Add</button>
        </form>
      }

      <ul class="m-0 p-0 list-none flex flex-col gap-2" data-testid="projects-list">
        @for (p of projects(); track p.id) {
          <li class="rounded-[var(--sy-radius-lg)] bg-[var(--sy-surface-1)] p-3" [attr.data-project]="p.name">
            <div class="flex items-center gap-2">
              <app-icon name="project" [size]="13" />
              <h3 class="m-0 flex-1 text-[13px] font-semibold truncate">{{ p.name }}@if (p.is_default) {<span class="ml-1.5 text-[11px] font-normal text-[var(--sy-text-muted)]">default</span>}</h3>
              <button type="button" class="sy-btn sy-btn-sm" (click)="startForm('repo', p.id)" [attr.aria-label]="'Add a repo to ' + p.name"><app-icon name="plus" [size]="12" />Repo</button>
              @if (!p.is_default) {
                <button type="button" class="sy-icon-btn sy-icon-btn-sm" [attr.aria-label]="'Delete project ' + p.name" syTooltip="Delete the project and its repo links" (click)="deleteProject(p)"><app-icon name="trash" [size]="13" /></button>
              }
            </div>
            @if (p.headline) {
              <p class="m-0 mt-1 text-[12px] text-[var(--sy-text-secondary)]">{{ p.headline }} <span class="text-[var(--sy-text-muted)]" aria-hidden="true">✦</span><span class="sy-sr-only">(written by the commander)</span></p>
            }
            <div class="mt-2 flex flex-wrap items-center gap-2 text-[11px] text-[var(--sy-text-muted)]">
              <span>Delivery:</span>
              <select class="sy-field sy-field-sm text-[11px] py-0.5" [attr.aria-label]="'Delivery mode for project ' + p.name" [ngModel]="p.delivery_mode || ''" (ngModelChange)="updateProjectDelivery(p, $event, p.merge_style || '')">
                <option value="">Inherit global</option>
                @for (opt of deliveryModeChoices; track opt.value) {
                  <option [value]="opt.value">{{ opt.label }}</option>
                }
              </select>
              <select class="sy-field sy-field-sm text-[11px] py-0.5" [attr.aria-label]="'Merge style for project ' + p.name" [ngModel]="p.merge_style || ''" (ngModelChange)="updateProjectDelivery(p, p.delivery_mode || '', $event)">
                <option value="">Inherit style</option>
                @for (opt of mergeStyleChoices; track opt.value) {
                  <option [value]="opt.value">{{ opt.label }}</option>
                }
              </select>
            </div>
            @if (form() === 'repo' && parentId() === p.id) {
              <form class="mt-2 grid grid-cols-[minmax(0,1fr)_minmax(0,2fr)] gap-2 rounded-[var(--sy-radius-md)] bg-black/20 p-3" (ngSubmit)="submit()">
                <label class="flex flex-col gap-1 text-[11px] text-[var(--sy-text-muted)]">Machine
                  <select name="worker" class="sy-field sy-field-sm" [ngModel]="workerId()" (ngModelChange)="workerId.set($event)">
                    @for (w of fleet.workers(); track w.id) {
                      <option [value]="w.id">{{ w.name }}</option>
                    }
                  </select>
                </label>
                <label class="flex flex-col gap-1 text-[11px] text-[var(--sy-text-muted)]">Path on the machine
                  <input name="path" class="sy-field sy-field-sm font-mono-ide" placeholder="~/src/repo" [ngModel]="path()" (ngModelChange)="path.set($event)" />
                </label>
                <label class="flex flex-col gap-1 text-[11px] text-[var(--sy-text-muted)] col-span-2">Name (optional)
                  <input name="name" class="sy-field sy-field-sm" placeholder="Defaults to the folder name" [ngModel]="name()" (ngModelChange)="name.set($event)" />
                </label>
                <div class="col-span-2 flex justify-end gap-2">
                  <button type="button" class="sy-btn sy-btn-sm" (click)="form.set(null)">Cancel</button>
                  <button type="submit" class="sy-btn sy-btn-sm sy-btn-primary" [disabled]="!path().trim() || !workerId() || busy()">Add repo</button>
                </div>
              </form>
            }
            <ul class="m-0 mt-2 p-0 list-none flex flex-col gap-0.5">
              @for (r of reposOf(p.id); track r.id) {
                <li>
                  <div class="group flex items-center gap-2 rounded-[var(--sy-radius-md)] px-2 py-1 hover:bg-[var(--sy-raised)]">
                    <app-icon name="repo" [size]="12" />
                    <span class="text-[12.5px] truncate">{{ r.name }}</span>
                    <span class="font-mono-ide text-[11px] text-[var(--sy-text-muted)] truncate">{{ machineName(r.worker_id) }}:{{ r.path }}</span>
                    <span class="flex-1"></span>
                    <button type="button" class="sy-btn sy-btn-sm" (click)="startChat(r, null)" [attr.aria-label]="'Start a chat in ' + r.name">Chat here</button>
                    <button type="button" class="sy-icon-btn sy-icon-btn-sm" [attr.aria-label]="'Add a worktree to ' + r.name" syTooltip="Add a worktree" (click)="startForm('worktree', r.id, r.path)"><app-icon name="git-branch" [size]="12" /></button>
                    <button type="button" class="sy-icon-btn sy-icon-btn-sm" [attr.aria-label]="'Remove repo ' + r.name" syTooltip="Remove the repo from the project (the folder stays)" (click)="deleteRepo(r)"><app-icon name="close" [size]="12" /></button>
                  </div>
                  <div class="ml-5 mb-1 flex flex-wrap items-center gap-2 text-[11px] text-[var(--sy-text-muted)]">
                    <select class="sy-field sy-field-sm text-[11px] py-0.5" [attr.aria-label]="'Delivery mode for repo ' + r.name" [ngModel]="r.delivery_mode || ''" (ngModelChange)="updateRepoDelivery(r, $event, r.merge_style || '')">
                      <option value="">Inherit project</option>
                      @for (opt of deliveryModeChoices; track opt.value) {
                        <option [value]="opt.value">{{ opt.label }}</option>
                      }
                    </select>
                    <select class="sy-field sy-field-sm text-[11px] py-0.5" [attr.aria-label]="'Merge style for repo ' + r.name" [ngModel]="r.merge_style || ''" (ngModelChange)="updateRepoDelivery(r, r.delivery_mode || '', $event)">
                      <option value="">Inherit style</option>
                      @for (opt of mergeStyleChoices; track opt.value) {
                        <option [value]="opt.value">{{ opt.label }}</option>
                      }
                    </select>
                  </div>
                  @if (form() === 'worktree' && parentId() === r.id) {
                    <form class="ml-6 my-1 flex gap-2 rounded-[var(--sy-radius-md)] bg-black/20 p-2" (ngSubmit)="submit()">
                      <label class="sy-sr-only" for="sy-wt-branch">Branch</label>
                      <input id="sy-wt-branch" name="branch" class="sy-field sy-field-sm flex-1 font-mono-ide" autofocus placeholder="Branch (e.g. feat/auth)" [ngModel]="name()" (ngModelChange)="name.set($event)" />
                      <label class="sy-sr-only" for="sy-wt-path">Worktree path</label>
                      <input id="sy-wt-path" name="path" class="sy-field sy-field-sm flex-1 font-mono-ide" placeholder="Worktree path" [ngModel]="path()" (ngModelChange)="path.set($event)" />
                      <button type="button" class="sy-btn sy-btn-sm" (click)="form.set(null)">Cancel</button>
                      <button type="submit" class="sy-btn sy-btn-sm sy-btn-primary" [disabled]="!name().trim() || busy()">Create</button>
                    </form>
                  }
                  @for (wt of worktreesOf(r.id); track wt.id) {
                    <div class="group ml-6 flex items-center gap-2 rounded-[var(--sy-radius-md)] px-2 py-0.5 hover:bg-[var(--sy-raised)] text-[12px] text-[var(--sy-text-secondary)]">
                      <app-icon name="git-branch" [size]="11" />
                      <span class="font-mono-ide truncate">{{ wt.branch || base(wt.path) }}</span>
                      <span class="flex-1"></span>
                      <button type="button" class="sy-btn sy-btn-sm" (click)="startChat(r, wt)" [attr.aria-label]="'Start a chat in ' + (wt.branch || base(wt.path))">Chat here</button>
                      @if (!wt.is_primary) {
                        <button type="button" class="sy-icon-btn sy-icon-btn-sm" [attr.aria-label]="'Remove worktree ' + (wt.branch || base(wt.path))" (click)="deleteWorktree(wt)"><app-icon name="close" [size]="12" /></button>
                      }
                    </div>
                  }
                </li>
              } @empty {
                <li class="px-2 text-[12px] text-[var(--sy-text-muted)]">No repos yet.</li>
              }
            </ul>
          </li>
        } @empty {
          <li class="text-[12.5px] text-[var(--sy-text-muted)]">No projects in this workspace yet.</li>
        }
      </ul>
    </app-modal-dialog>
  `,
})
export class ProjectsDialogComponent {
  readonly close = output<void>();
  /** Start a chat in a repo or worktree (the caller opens the session dialog). */
  readonly chat = output<{ repo: Repo; worktree: Worktree | null }>();
  readonly fleet = inject(FleetStore);
  private readonly api = inject(ApiClientService);

  readonly workspaceId = signal('');
  readonly form = signal<'workspace' | 'project' | 'repo' | 'worktree' | null>(null);
  readonly parentId = signal('');
  readonly name = signal('');
  readonly path = signal('');
  readonly workerId = signal('');
  readonly busy = signal(false);
  readonly error = signal('');
  readonly deliveryModeChoices = DELIVERY_MODE_OPTIONS.filter((o) => o.value !== '');
  readonly mergeStyleChoices = MERGE_STYLE_OPTIONS;

  readonly activeWorkspaceId = computed(() => {
    const sel = this.workspaceId();
    const list = this.fleet.workspaces();
    if (sel && list.some((w) => w.id === sel)) return sel;
    return list.find((w) => w.is_default)?.id || list[0]?.id || '';
  });

  readonly projects = computed(() => {
    const ws = this.activeWorkspaceId();
    return this.fleet.projects().filter((p) => !ws || p.workspace_id === ws);
  });

  reposOf(projectId: string): Repo[] {
    return this.fleet.repos().filter((r) => r.project_id === projectId);
  }

  worktreesOf(repoId: string): Worktree[] {
    return this.fleet.worktrees().filter((w) => w.repo_id === repoId);
  }

  machineName(id: string): string {
    return this.fleet.workersById()[id]?.name ?? '?';
  }

  base(p: string): string {
    return basenamePath(p);
  }

  startForm(kind: 'workspace' | 'project' | 'repo' | 'worktree', parent: string, path = ''): void {
    this.error.set('');
    if (this.form() === kind && this.parentId() === parent) {
      this.form.set(null);
      return;
    }
    this.form.set(kind);
    this.parentId.set(parent);
    this.name.set('');
    this.path.set(kind === 'repo' ? '~' : path);
    if (kind === 'repo') this.workerId.set(this.fleet.resolveSessionDefaults().machine_id || this.fleet.workers()[0]?.id || '');
  }

  async submit(): Promise<void> {
    const kind = this.form();
    if (!kind || this.busy()) return;
    this.busy.set(true);
    this.error.set('');
    try {
      const name = this.name().trim();
      if (kind === 'workspace') {
        const ws = await this.api.createWorkspace(name);
        this.fleet.upsertWorkspace(ws);
        this.workspaceId.set(ws.id);
      } else if (kind === 'project') {
        this.fleet.upsertProject(await this.api.createProject({ workspace_id: this.activeWorkspaceId(), name }));
      } else if (kind === 'repo') {
        this.fleet.upsertRepo(await this.api.createRepo({ project_id: this.parentId(), worker_id: this.workerId(), path: this.path().trim(), name: name || undefined }));
        await this.fleet.refresh();
      } else {
        this.fleet.upsertWorktree(await this.api.createWorktree({ repo_id: this.parentId(), branch: name, path: this.path().trim() || undefined }));
      }
      this.form.set(null);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not save');
    } finally {
      this.busy.set(false);
    }
  }

  async deleteProject(p: Project): Promise<void> {
    if (p.is_default || !window.confirm(`Delete project "${p.name}" and its repo links? The folders stay.`)) return;
    await this.run(async () => {
      await this.api.deleteProject(p.id);
      this.fleet.removeProject(p.id);
    });
  }

  async updateProjectDelivery(p: Project, delivery_mode: string, merge_style: string): Promise<void> {
    await this.run(async () => {
      this.fleet.upsertProject(await this.api.updateProject(p.id, { delivery_mode, merge_style }));
    });
  }

  async updateRepoDelivery(r: Repo, delivery_mode: string, merge_style: string): Promise<void> {
    await this.run(async () => {
      this.fleet.upsertRepo(await this.api.updateRepo(r.id, { delivery_mode, merge_style }));
    });
  }

  async deleteRepo(r: Repo): Promise<void> {
    await this.run(async () => {
      await this.api.deleteRepo(r.id);
      this.fleet.removeRepo(r.id);
    });
  }

  async deleteWorktree(wt: Worktree): Promise<void> {
    await this.run(async () => {
      await this.api.deleteWorktree(wt.id);
      this.fleet.removeWorktree(wt.id);
    });
  }

  startChat(repo: Repo, worktree: Worktree | null): void {
    this.chat.emit({ repo, worktree });
  }

  private async run(fn: () => Promise<void>): Promise<void> {
    this.error.set('');
    try {
      await fn();
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not remove it');
    }
  }
}

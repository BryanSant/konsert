import { ChangeDetectionStrategy, Component, inject, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Project, Repo } from '../../core/models/syre.models';
import { DELIVERY_MODE_OPTIONS, MERGE_STYLE_OPTIONS } from '../../core/models/review.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { FleetStore } from '../../core/state/fleet.store';
import { IconComponent } from '../../shared/icon.component';
import { ModalDialogComponent } from '../../shared/modal-dialog.component';
import { TooltipDirective } from '../../shared/tooltip.directive';

/**
 * Projects and repos: each project owns a repo registry keyed by remote_url and delivery
 * defaults. Start a chat for any registered repo from here.
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
          <p class="m-0 mt-1 text-[12px] text-[var(--sy-text-secondary)]">Each project owns its repository registry and delivery defaults.</p>
        </div>
        <button type="button" class="sy-btn sy-btn-sm sy-btn-soft" data-testid="projects-new" (click)="startForm('project', '')"><app-icon name="plus" [size]="12" />Project</button>
        <button type="button" class="sy-icon-btn" aria-label="Close" (click)="close.emit()"><app-icon name="close" [size]="15" /></button>
      </div>

      @if (error()) {
        <p role="alert" class="m-0 mb-3 rounded-[var(--sy-radius-md)] bg-[var(--sy-red-soft)] px-3 py-2 text-[12px] text-[#f3a5a5]">{{ error() }}</p>
      }

      @if (form() === 'project') {
        <form class="mb-3 flex gap-2 rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-1)] p-3" (ngSubmit)="submit()">
          <label class="sy-sr-only" for="sy-proj-name">Project name</label>
          <input id="sy-proj-name" name="name" class="sy-field sy-field-sm flex-1" autofocus placeholder="Project name" [ngModel]="name()" (ngModelChange)="name.set($event)" />
          <button type="button" class="sy-btn sy-btn-sm" (click)="form.set(null)">Cancel</button>
          <button type="submit" class="sy-btn sy-btn-sm sy-btn-primary" [disabled]="!name().trim() || busy()">Add</button>
        </form>
      }

      <ul class="m-0 p-0 list-none flex flex-col gap-2" data-testid="projects-list">
        @for (p of fleet.projects(); track p.id) {
          <li class="rounded-[var(--sy-radius-lg)] bg-[var(--sy-surface-1)] p-3" [attr.data-project]="p.name">
            <div class="flex items-center gap-2">
              <app-icon name="project" [size]="13" />
              <h3 class="m-0 flex-1 text-[13px] font-semibold truncate">{{ p.name }}@if (p.is_default) {<span class="ml-1.5 text-[11px] font-normal text-[var(--sy-text-muted)]">default</span>}</h3>
              <button type="button" class="sy-btn sy-btn-sm" (click)="startForm('repo', p.id)" [attr.aria-label]="'Add a repo to ' + p.name"><app-icon name="plus" [size]="12" />Repo</button>
              @if (!p.is_default) {
                <button type="button" class="sy-icon-btn sy-icon-btn-sm" [attr.aria-label]="'Delete project ' + p.name" syTooltip="Delete the project and its repo registry" (click)="deleteProject(p)"><app-icon name="trash" [size]="13" /></button>
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
              <form class="mt-2 grid grid-cols-[minmax(0,2fr)_minmax(0,1fr)] gap-2 rounded-[var(--sy-radius-md)] bg-black/20 p-3" (ngSubmit)="submit()">
                <label class="flex flex-col gap-1 text-[11px] text-[var(--sy-text-muted)] col-span-2">Remote URL
                  <input name="remote_url" class="sy-field sy-field-sm font-mono-ide" autofocus placeholder="https://github.com/org/repo.git" [ngModel]="remoteUrl()" (ngModelChange)="remoteUrl.set($event)" />
                </label>
                <label class="flex flex-col gap-1 text-[11px] text-[var(--sy-text-muted)]">Name (optional)
                  <input name="name" class="sy-field sy-field-sm" placeholder="Defaults to repo name" [ngModel]="name()" (ngModelChange)="name.set($event)" />
                </label>
                <label class="flex flex-col gap-1 text-[11px] text-[var(--sy-text-muted)]">Default branch (optional)
                  <input name="default_branch" class="sy-field sy-field-sm font-mono-ide" placeholder="main" [ngModel]="defaultBranch()" (ngModelChange)="defaultBranch.set($event)" />
                </label>
                <div class="col-span-2 flex justify-end gap-2">
                  <button type="button" class="sy-btn sy-btn-sm" (click)="form.set(null)">Cancel</button>
                  <button type="submit" class="sy-btn sy-btn-sm sy-btn-primary" [disabled]="!remoteUrl().trim() || busy()">Add repo</button>
                </div>
              </form>
            }
            <ul class="m-0 mt-2 p-0 list-none flex flex-col gap-0.5">
              @for (r of reposOf(p.id); track r.id) {
                <li>
                  <div class="group flex items-center gap-2 rounded-[var(--sy-radius-md)] px-2 py-1 hover:bg-[var(--sy-raised)]">
                    <app-icon name="repo" [size]="12" />
                    <span class="text-[12.5px] truncate">{{ r.name }}</span>
                    <span class="font-mono-ide text-[11px] text-[var(--sy-text-muted)] truncate">{{ r.remote_url }}</span>
                    @if (r.default_branch) {
                      <span class="font-mono-ide text-[10.5px] text-[var(--sy-text-muted)]">({{ r.default_branch }})</span>
                    }
                    <span class="flex-1"></span>
                    <button type="button" class="sy-btn sy-btn-sm" (click)="startChat(r)" [attr.aria-label]="'Start a chat in ' + r.name">Chat here</button>
                    <button type="button" class="sy-icon-btn sy-icon-btn-sm" [attr.aria-label]="'Remove repo ' + r.name" syTooltip="Remove the repo from the project" (click)="deleteRepo(r)"><app-icon name="close" [size]="12" /></button>
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
                </li>
              } @empty {
                <li class="px-2 text-[12px] text-[var(--sy-text-muted)]">No repos yet.</li>
              }
            </ul>
          </li>
        } @empty {
          <li class="text-[12.5px] text-[var(--sy-text-muted)]">No projects yet.</li>
        }
      </ul>
    </app-modal-dialog>
  `,
})
export class ProjectsDialogComponent {
  readonly close = output<void>();
  /** Start a chat for a registered repo (the caller opens the session dialog). */
  readonly chat = output<{ repo: Repo }>();
  readonly fleet = inject(FleetStore);
  private readonly api = inject(ApiClientService);

  readonly form = signal<'project' | 'repo' | null>(null);
  readonly parentId = signal('');
  readonly name = signal('');
  readonly remoteUrl = signal('');
  readonly defaultBranch = signal('');
  readonly busy = signal(false);
  readonly error = signal('');
  readonly deliveryModeChoices = DELIVERY_MODE_OPTIONS.filter((o) => o.value !== '');
  readonly mergeStyleChoices = MERGE_STYLE_OPTIONS;

  reposOf(projectId: string): Repo[] {
    return this.fleet.repos().filter((r) => r.project_id === projectId);
  }

  startForm(kind: 'project' | 'repo', parent: string): void {
    this.error.set('');
    if (this.form() === kind && this.parentId() === parent) {
      this.form.set(null);
      return;
    }
    this.form.set(kind);
    this.parentId.set(parent);
    this.name.set('');
    this.remoteUrl.set('');
    this.defaultBranch.set('');
  }

  async submit(): Promise<void> {
    const kind = this.form();
    if (!kind || this.busy()) return;
    this.busy.set(true);
    this.error.set('');
    try {
      const name = this.name().trim();
      if (kind === 'project') {
        this.fleet.upsertProject(await this.api.createProject({ name }));
      } else {
        this.fleet.upsertRepo(
          await this.api.createRepo(this.parentId(), {
            remote_url: this.remoteUrl().trim(),
            name: name || undefined,
            default_branch: this.defaultBranch().trim() || undefined,
          }),
        );
      }
      this.form.set(null);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not save');
    } finally {
      this.busy.set(false);
    }
  }

  async deleteProject(p: Project): Promise<void> {
    if (p.is_default || !window.confirm(`Delete project "${p.name}" and its repo registry?`)) return;
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
      this.fleet.upsertRepo(await this.api.updateRepo(r.project_id, r.id, { delivery_mode, merge_style }));
    });
  }

  async deleteRepo(r: Repo): Promise<void> {
    await this.run(async () => {
      await this.api.deleteRepo(r.project_id, r.id);
      this.fleet.removeRepo(r.id);
    });
  }

  startChat(repo: Repo): void {
    this.chat.emit({ repo });
  }

  private async run(fn: () => Promise<void>): Promise<void> {
    this.error.set('');
    try {
      await fn();
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not save changes');
    }
  }
}

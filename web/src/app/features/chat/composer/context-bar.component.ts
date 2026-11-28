import { NgTemplateOutlet } from '@angular/common';
import { ChangeDetectionStrategy, Component, computed, inject, input, output, signal } from '@angular/core';
import { FleetStore, basenamePath } from '../../../core/state/fleet.store';
import { IconComponent } from '../../../shared/icon.component';
import { PopoverComponent, PopoverTriggerDirective } from '../../../shared/popover.component';
import { RemoteDirPickerComponent } from '../../../shared/remote-dir-picker.component';
import { TooltipDirective } from '../../../shared/tooltip.directive';
import { MachineTargetPickerComponent } from '../../../shared/machines/machine-target-picker.component';
import { ALL_MACHINES, MachineTarget } from '../../../shared/machines/machine-target';
import { WorkspaceFolderInputComponent } from '../../../shared/machines/workspace-folder-input.component';
import { displayFolder } from '../../../shared/machines/workspace-folder';

/** One place the chat can run: a repo checkout (worktree) or a Space on the machine. */
export interface CheckoutOption {
  key: string;
  path: string;
  name: string;
  detail: string;
  branch: string;
  repoId?: string;
}

export interface ContextChange {
  workerId?: string;
  cwd?: string;
  branch?: string;
}

/** Quiet line under the composer: machine (or a Commander chat's machines) · checkout · branch, each a picker. */
@Component({
  selector: 'sy-context-bar',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [NgTemplateOutlet, PopoverComponent, PopoverTriggerDirective, IconComponent, TooltipDirective, RemoteDirPickerComponent, MachineTargetPickerComponent, WorkspaceFolderInputComponent],
  template: `
    @if (locked()) {
      <div class="flex flex-wrap items-center gap-3 px-3 pt-2 text-[11.5px] text-[var(--sy-text-muted)]" data-testid="context-locked">
        @if (scopeMode()) {
          <sy-machine-target-picker mode="scope" [value]="scope()" (valueChange)="scopeChange.emit($event)" />
        } @else {
          <sy-machine-target-picker mode="host" [workerId]="workerId()" [locked]="true" />
        }
        @if (homeOnly()) {
          <ng-container *ngTemplateOutlet="workspaceFolderChip" />
        } @else {
          <span class="inline-flex items-center gap-1.5" [syTooltip]="cwd()">
            <app-icon name="folder" [size]="12" />
            <span class="max-w-[240px] truncate">{{ checkoutLabel() }}</span>
          </span>
        }
        @if (displayBranch()) {
          <span class="inline-flex items-center gap-1.5">
            <app-icon name="git-branch" [size]="12" />
            <span class="max-w-[160px] truncate">{{ displayBranch() }}</span>
          </span>
        }
      </div>
    } @else {
    <div class="flex flex-wrap items-center gap-0.5 px-1 pt-1.5 text-[11.5px]">
      <!-- Machine: where the agent runs, or for a Commander chat where its work may go -->
      @if (scopeMode()) {
        <sy-machine-target-picker mode="scope" [value]="scope()" (valueChange)="scopeChange.emit($event)" />
      } @else {
        <sy-machine-target-picker mode="host" [workerId]="workerId()" (workerChange)="changed.emit({ workerId: $event, cwd: '~', branch: '' })" />
      }

      <!-- Checkout -->
      @if (homeOnly()) {
        <ng-container *ngTemplateOutlet="workspaceFolderChip" />
      } @else {
      <sy-popover label="Checkout" role="dialog" placement="top-start" panelClass="w-[340px]" [closeOnSelect]="false" [(open)]="checkoutOpen">
        <button
          type="button"
          syPopoverTrigger
          class="sy-btn sy-btn-sm gap-1.5"
          data-testid="context-checkout"
          [disabled]="locked()"
          [attr.aria-label]="'Working folder: ' + checkoutLabel()"
          [syTooltip]="locked() ? cwd() + ' (fixed for this chat)' : cwd()"
        >
          <app-icon name="folder" [size]="12" />
          <span class="max-w-[220px] truncate">{{ checkoutLabel() }}</span>
        </button>
        <div class="max-h-[220px] overflow-y-auto">
          @if (checkouts().length > 0) {
            <div class="sy-menu-heading">Checkouts on {{ machineName() }}</div>
          }
          @for (c of checkouts(); track c.key) {
            <button type="button" class="sy-menu-item items-center" [attr.aria-current]="c.path === cwd() ? 'true' : null" (click)="pickCheckout(c)">
              <app-icon name="folder" [size]="13" />
              <span class="flex-1 min-w-0">
                <span class="sy-menu-label block truncate">{{ c.name }}</span>
                <span class="sy-menu-desc block truncate">{{ c.detail }}</span>
              </span>
              @if (c.path === cwd()) {
                <app-icon name="check" [size]="13" />
              }
            </button>
          }
        </div>
        <div class="sy-menu-heading">Any folder</div>
        <div class="px-2 pb-2">
          <app-remote-dir-picker [workerId]="workerId()" [value]="draftPath()" (valueChange)="draftPath.set($event)" inputName="chatCwd" />
          <div class="flex justify-end pt-2">
            <button type="button" class="sy-btn sy-btn-sm sy-btn-soft" [disabled]="!draftPath().trim()" (click)="useDraftPath()">Use folder</button>
          </div>
        </div>
      </sy-popover>
      }

      <!-- Branch -->
      @if (displayBranch() || branchOptions().length > 0) {
        <sy-popover label="Branch" placement="top-start" panelClass="w-[260px] max-h-[300px] overflow-y-auto" [(open)]="branchOpen">
          <button
            type="button"
            syPopoverTrigger
            class="sy-btn sy-btn-sm gap-1.5"
            data-testid="context-branch"
            [disabled]="locked() || branchOptions().length === 0"
            [attr.aria-label]="'Branch: ' + (displayBranch() || 'none')"
            [syTooltip]="locked() ? 'Fixed for this chat' : 'Switch to another checkout of this repo'"
          >
            <app-icon name="git-branch" [size]="12" />
            <span class="max-w-[160px] truncate">{{ displayBranch() || 'no branch' }}</span>
          </button>
          @for (c of branchOptions(); track c.key) {
            <button type="button" role="menuitemradio" class="sy-menu-item items-center" [attr.aria-checked]="c.path === cwd()" (click)="pickCheckout(c)">
              <app-icon name="git-branch" [size]="13" />
              <span class="flex-1 min-w-0">
                <span class="sy-menu-label block truncate">{{ c.branch || 'detached' }}</span>
                <span class="sy-menu-desc block truncate">{{ c.path }}</span>
              </span>
            </button>
          }
        </sy-popover>
      }
    </div>
    }

    <!-- Several machines in a Commander chat: one folder in each home (SYR-W3SVZ) -->
    <ng-template #workspaceFolderChip>
        <sy-popover label="Workspace folder" role="dialog" placement="top-start" panelClass="w-[320px] p-2" [closeOnSelect]="false" [(open)]="folderOpen">
          <button
            type="button"
            syPopoverTrigger
            class="sy-btn sy-btn-sm gap-1.5"
            data-testid="context-workspace-folder"
            [attr.aria-label]="'Workspace folder: ' + folderLabel()"
            syTooltip="The folder in each machine's home where this chat's work starts"
          >
            <app-icon name="folder" [size]="12" />
            <span class="max-w-[200px] truncate font-mono-ide">{{ folderLabel() }}</span>
          </button>
          <sy-workspace-folder-input label="Workspace folder for this chat" [value]="workFolder()" (valueChange)="folderOpen.set(false); workFolderChange.emit($event)" />
        </sy-popover>
    </ng-template>
  `,
})
export class ContextBarComponent {
  readonly workerId = input('');
  readonly cwd = input('~');
  readonly branch = input('');
  /** Existing chats keep their machine and folder. */
  readonly locked = input(false);
  /** A Commander chat: the machine chip says where its work may go (SYR-FJA2T). */
  readonly scopeMode = input(false);
  readonly scope = input<MachineTarget>(ALL_MACHINES);
  readonly changed = output<ContextChange>();
  readonly scopeChange = output<MachineTarget>();
  /** Several machines in scope: the folder in each home where work starts ("src"). */
  readonly workFolder = input('');
  readonly workFolderChange = output<string>();
  readonly folderOpen = signal(false);
  readonly folderLabel = computed(() => (this.workFolder() ? displayFolder(this.workFolder()) : 'Workspace folder'));

  private readonly fleet = inject(FleetStore);
  readonly checkoutOpen = signal(false);
  readonly branchOpen = signal(false);
  readonly draftPath = signal('');

  /** Several machines in scope: no one machine's folders to pick from (SYR-W3SVZ). */
  readonly homeOnly = computed(() => this.scopeMode() && this.scope().kind !== 'machine');
  readonly machineName = computed(() => this.fleet.workersById()[this.workerId()]?.name || (this.workerId() ? this.workerId() : 'localhost'));

  // Per-machine checkouts are reported as daemon facts in A1c; until then users pick any folder path directly.
  readonly checkouts = computed<CheckoutOption[]>(() => []);

  private readonly currentCheckout = computed(() => this.checkouts().find((c) => c.path === this.cwd()) ?? null);

  readonly checkoutLabel = computed(() => {
    const c = this.currentCheckout();
    if (c) {
      const detail = c.detail.startsWith('Main checkout') ? 'Main checkout' : c.branch || basenamePath(c.path);
      return `${c.name} · ${detail}`;
    }
    const p = this.cwd() || '~';
    return p === '~' ? 'Home folder' : basenamePath(p);
  });

  /** The chosen branch, else the branch of the checkout the folder belongs to. */
  readonly displayBranch = computed(() => this.branch() || this.currentCheckout()?.branch || '');

  readonly branchOptions = computed(() => {
    const c = this.currentCheckout();
    if (!c?.repoId) return [];
    return this.checkouts().filter((o) => o.repoId === c.repoId && o.branch);
  });

  pickCheckout(c: CheckoutOption): void {
    this.changed.emit({ cwd: c.path, branch: c.branch });
    this.checkoutOpen.set(false);
    this.branchOpen.set(false);
  }

  useDraftPath(): void {
    const p = this.draftPath().trim();
    if (!p) return;
    this.changed.emit({ cwd: p, branch: '' });
    this.checkoutOpen.set(false);
  }
}

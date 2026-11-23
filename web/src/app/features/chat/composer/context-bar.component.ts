import { ChangeDetectionStrategy, Component, computed, inject, input, output, signal } from '@angular/core';
import { FleetStore, basenamePath } from '../../../core/state/fleet.store';
import { IconComponent } from '../../../shared/icon.component';
import { PopoverComponent, PopoverTriggerDirective } from '../../../shared/popover.component';
import { RemoteDirPickerComponent } from '../../../shared/remote-dir-picker.component';
import { TooltipDirective } from '../../../shared/tooltip.directive';

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

/** Quiet line under the composer: machine · checkout · branch, each a picker. */
@Component({
  selector: 'ks-context-bar',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [PopoverComponent, PopoverTriggerDirective, IconComponent, TooltipDirective, RemoteDirPickerComponent],
  template: `
    <div class="flex flex-wrap items-center gap-0.5 px-1 pt-1.5 text-[11.5px]">
      <!-- Machine -->
      <ks-popover label="Machine" placement="top-start" panelClass="w-[260px] max-h-[320px] overflow-y-auto" [(open)]="machineOpen">
        <button
          type="button"
          ksPopoverTrigger
          class="ks-btn ks-btn-sm gap-1.5"
          data-testid="context-machine"
          [disabled]="locked()"
          [attr.aria-label]="'Machine: ' + machineName()"
          [ksTooltip]="locked() ? 'Fixed for this chat' : 'Change machine'"
        >
          <app-icon name="server" [size]="12" />
          <span class="max-w-[140px] truncate">{{ machineName() }}</span>
        </button>
        @for (w of workers(); track w.id) {
          <button
            type="button"
            role="menuitemradio"
            class="ks-menu-item items-center"
            [attr.aria-checked]="w.id === workerId()"
            (click)="changed.emit({ workerId: w.id, cwd: '~', branch: '' })"
          >
            <span
              class="h-1.5 w-1.5 rounded-full shrink-0"
              [class.bg-[var(--ks-green)]]="w.status === 'online'"
              [class.bg-[#5e6068]]="w.status !== 'online'"
              aria-hidden="true"
            ></span>
            <span class="flex-1 min-w-0">
              <span class="ks-menu-label block truncate">{{ w.name }}</span>
              <span class="ks-menu-desc block">{{ w.status === 'online' ? 'Online' : 'Offline' }} · {{ w.os }}</span>
            </span>
            @if (w.id === workerId()) {
              <app-icon name="check" [size]="13" />
            }
          </button>
        } @empty {
          <div class="px-3 py-2 text-[12px] text-[var(--ks-text-muted)]">No machines connected</div>
        }
      </ks-popover>

      <!-- Checkout -->
      <ks-popover label="Checkout" role="dialog" placement="top-start" panelClass="w-[340px]" [closeOnSelect]="false" [(open)]="checkoutOpen">
        <button
          type="button"
          ksPopoverTrigger
          class="ks-btn ks-btn-sm gap-1.5"
          data-testid="context-checkout"
          [disabled]="locked()"
          [attr.aria-label]="'Working folder: ' + checkoutLabel()"
          [ksTooltip]="locked() ? cwd() + ' (fixed for this chat)' : cwd()"
        >
          <app-icon name="folder" [size]="12" />
          <span class="max-w-[220px] truncate">{{ checkoutLabel() }}</span>
        </button>
        <div class="max-h-[220px] overflow-y-auto">
          @if (checkouts().length > 0) {
            <div class="ks-menu-heading">Checkouts on {{ machineName() }}</div>
          }
          @for (c of checkouts(); track c.key) {
            <button type="button" class="ks-menu-item items-center" [attr.aria-current]="c.path === cwd() ? 'true' : null" (click)="pickCheckout(c)">
              <app-icon name="folder" [size]="13" />
              <span class="flex-1 min-w-0">
                <span class="ks-menu-label block truncate">{{ c.name }}</span>
                <span class="ks-menu-desc block truncate">{{ c.detail }}</span>
              </span>
              @if (c.path === cwd()) {
                <app-icon name="check" [size]="13" />
              }
            </button>
          }
        </div>
        <div class="ks-menu-heading">Any folder</div>
        <div class="px-2 pb-2">
          <app-remote-dir-picker [workerId]="workerId()" [value]="draftPath()" (valueChange)="draftPath.set($event)" inputName="chatCwd" />
          <div class="flex justify-end pt-2">
            <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" [disabled]="!draftPath().trim()" (click)="useDraftPath()">Use folder</button>
          </div>
        </div>
      </ks-popover>

      <!-- Branch -->
      @if (branch() || branchOptions().length > 0) {
        <ks-popover label="Branch" placement="top-start" panelClass="w-[260px] max-h-[300px] overflow-y-auto" [(open)]="branchOpen">
          <button
            type="button"
            ksPopoverTrigger
            class="ks-btn ks-btn-sm gap-1.5"
            data-testid="context-branch"
            [disabled]="locked() || branchOptions().length === 0"
            [attr.aria-label]="'Branch: ' + (branch() || 'none')"
            [ksTooltip]="locked() ? 'Fixed for this chat' : 'Switch to another checkout of this repo'"
          >
            <app-icon name="git-branch" [size]="12" />
            <span class="max-w-[160px] truncate">{{ branch() || 'no branch' }}</span>
          </button>
          @for (c of branchOptions(); track c.key) {
            <button type="button" role="menuitemradio" class="ks-menu-item items-center" [attr.aria-checked]="c.path === cwd()" (click)="pickCheckout(c)">
              <app-icon name="git-branch" [size]="13" />
              <span class="flex-1 min-w-0">
                <span class="ks-menu-label block truncate">{{ c.branch || 'detached' }}</span>
                <span class="ks-menu-desc block truncate">{{ c.path }}</span>
              </span>
            </button>
          }
        </ks-popover>
      }
    </div>
  `,
})
export class ContextBarComponent {
  readonly workerId = input('');
  readonly cwd = input('~');
  readonly branch = input('');
  /** Existing chats keep their machine and folder. */
  readonly locked = input(false);
  readonly changed = output<ContextChange>();

  private readonly fleet = inject(FleetStore);
  readonly machineOpen = signal(false);
  readonly checkoutOpen = signal(false);
  readonly branchOpen = signal(false);
  readonly draftPath = signal('');

  readonly workers = computed(() =>
    [...this.fleet.workers()].sort((a, b) => Number(b.status === 'online') - Number(a.status === 'online') || a.name.localeCompare(b.name)),
  );
  readonly machineName = computed(() => this.fleet.workersById()[this.workerId()]?.name || (this.workerId() ? this.workerId() : 'localhost'));

  readonly checkouts = computed<CheckoutOption[]>(() => {
    const wid = this.workerId();
    const repos = this.fleet.repos().filter((r) => r.worker_id === wid);
    const out: CheckoutOption[] = [];
    for (const r of repos) {
      const wts = this.fleet.worktrees().filter((w) => w.repo_id === r.id);
      if (wts.length === 0) {
        out.push({ key: `r:${r.id}`, path: r.path, name: r.name, detail: 'Main checkout', branch: '', repoId: r.id });
      }
      for (const wt of wts) {
        out.push({
          key: `wt:${wt.id}`,
          path: wt.path,
          name: r.name,
          detail: wt.is_primary ? `Main checkout${wt.branch ? ' · ' + wt.branch : ''}` : `${wt.branch || 'worktree'} · ${wt.path}`,
          branch: wt.branch || '',
          repoId: r.id,
        });
      }
    }
    for (const sp of this.fleet.spaces().filter((s) => s.worker_id === wid)) {
      if (!out.some((o) => o.path === sp.path)) {
        out.push({ key: `sp:${sp.id}`, path: sp.path, name: sp.name, detail: sp.path, branch: sp.branch || '' });
      }
    }
    return out;
  });

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

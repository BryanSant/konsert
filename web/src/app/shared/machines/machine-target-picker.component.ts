import {
  ChangeDetectionStrategy,
  Component,
  computed,
  inject,
  input,
  output,
  signal,
} from '@angular/core';
import { FleetStore } from '../../core/state/fleet.store';
import { ModeUiStore } from '../../modes/mode-ui.store';
import { IconComponent } from '../icon.component';
import { PopoverComponent, PopoverTriggerDirective } from '../popover.component';
import { TooltipDirective } from '../tooltip.directive';
import { MachineSummaryComponent } from './machine-summary.component';
import {
  ALL_MACHINES,
  MachineTarget,
  isAllMachines,
  resolveTarget,
  sameTarget,
  targetChipText,
} from './machine-target';

/**
 * The machine chip under a chat's composer (SYR-FJA2T), one component with two meanings:
 * - `scope` (Commander chats): where the Commander may send work. All machines, a saved
 *   group, a label (every machine with it, now and later), one machine, or a hand-picked
 *   set from the Machines dialog. It stays editable; a change applies to the next dispatch.
 * - `host` (direct agent chats): the one machine the agent runs on; locked once the chat exists.
 */
@Component({
  selector: 'sy-machine-target-picker',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [
    PopoverComponent,
    PopoverTriggerDirective,
    IconComponent,
    TooltipDirective,
    MachineSummaryComponent,
  ],
  template: `
    @if (mode() === 'host' && locked()) {
      <span
        class="inline-flex items-center gap-1.5"
        syTooltip="Machine (fixed for this chat)"
        data-testid="context-machine-locked"
      >
        <app-icon name="server" [size]="12" />
        <span class="max-w-[160px] truncate">{{ hostName() }}</span>
      </span>
    } @else {
      <sy-popover
        [label]="mode() === 'scope' ? 'Machines' : 'Machine'"
        placement="top-start"
        panelClass="w-[300px] max-h-[380px] overflow-y-auto"
        [(open)]="open"
      >
        <button
          type="button"
          syPopoverTrigger
          class="sy-btn sy-btn-sm gap-1.5"
          [attr.data-testid]="mode() === 'scope' ? 'context-scope' : 'context-machine'"
          [attr.aria-label]="
            mode() === 'scope'
              ? 'Where the Commander may send work: ' + chipText()
              : 'Machine: ' + hostName()
          "
          [syTooltip]="mode() === 'scope' ? 'Where the Commander may send work' : 'Change machine'"
        >
          <app-icon [name]="mode() === 'scope' ? 'globe' : 'server'" [size]="12" />
          <span class="max-w-[200px] truncate">{{
            mode() === 'scope' ? chipText() : hostName()
          }}</span>
        </button>

        @if (mode() === 'scope') {
          <button
            type="button"
            role="menuitemradio"
            class="sy-menu-item items-center"
            data-testid="scope-all"
            [attr.aria-checked]="isAll()"
            (click)="pick(all)"
          >
            <app-icon name="globe" [size]="13" />
            <span class="flex-1 min-w-0">
              <span class="sy-menu-label block">All machines</span>
              <span class="sy-menu-desc block">Syre picks, now and as machines join</span>
            </span>
            @if (isAll()) {
              <app-icon name="check" [size]="13" />
            }
          </button>
          @if (fleet.machineGroups().length) {
            <div class="sy-menu-heading">Groups</div>
            @for (g of fleet.machineGroups(); track g.id) {
              <button
                type="button"
                role="menuitemradio"
                class="sy-menu-item items-center"
                [attr.data-testid]="'scope-group-' + g.name"
                [attr.aria-checked]="isPicked({ kind: 'group', id: g.id })"
                (click)="pick({ kind: 'group', id: g.id })"
              >
                <span class="flex-1 min-w-0">
                  <span class="sy-menu-label block truncate">{{ g.name }}</span>
                  <span class="sy-menu-desc block"
                    >{{ g.worker_ids.length }}
                    {{ g.worker_ids.length === 1 ? 'machine' : 'machines' }}</span
                  >
                </span>
                @if (isPicked({ kind: 'group', id: g.id })) {
                  <app-icon name="check" [size]="13" />
                }
              </button>
            }
          }
          @if (labelCounts().length) {
            <div class="sy-menu-heading">Labels</div>
            @for (l of labelCounts(); track l.label) {
              <button
                type="button"
                role="menuitemradio"
                class="sy-menu-item items-center"
                [attr.data-testid]="'scope-label-' + l.label"
                [attr.aria-checked]="isPicked({ kind: 'label', label: l.label })"
                (click)="pick({ kind: 'label', label: l.label })"
              >
                <span class="flex-1 min-w-0">
                  <span class="sy-menu-label block truncate">Label: {{ l.label }}</span>
                  <span class="sy-menu-desc block"
                    >Every machine with it, now and later · {{ l.count }} now</span
                  >
                </span>
                @if (isPicked({ kind: 'label', label: l.label })) {
                  <app-icon name="check" [size]="13" />
                }
              </button>
            }
          }
          <div class="sy-menu-heading">Machines</div>
        }
        @for (w of workers(); track w.id) {
          <button
            type="button"
            role="menuitemradio"
            class="sy-menu-item items-center"
            [attr.data-testid]="'pick-machine-' + (w.sid || w.id)"
            [attr.aria-checked]="
              mode() === 'scope' ? isPicked({ kind: 'machine', id: w.id }) : w.id === workerId()
            "
            (click)="pickMachine(w.id)"
          >
            <sy-machine-summary class="flex-1" [worker]="w" />
            @if (
              mode() === 'scope' ? isPicked({ kind: 'machine', id: w.id }) : w.id === workerId()
            ) {
              <app-icon name="check" [size]="13" />
            }
          </button>
        } @empty {
          <div class="px-3 py-2 text-[12px] text-[var(--sy-text-muted)]">No machines connected</div>
        }
        @if (mode() === 'scope') {
          <div class="border-t border-[var(--sy-hairline)] mt-1 pt-1">
            <button
              type="button"
              class="sy-menu-item items-center"
              data-testid="scope-select"
              (click)="selectMachines()"
            >
              <span class="sy-menu-label">{{
                value().kind === 'machines' ? 'Change machines…' : 'Select machines…'
              }}</span>
            </button>
          </div>
        }
      </sy-popover>
    }
  `,
})
export class MachineTargetPickerComponent {
  readonly fleet = inject(FleetStore);
  private readonly ui = inject(ModeUiStore);

  readonly mode = input<'scope' | 'host'>('host');
  /** Scope mode: where work may go. */
  readonly value = input<MachineTarget>(ALL_MACHINES);
  /** Host mode: the machine the agent runs on. */
  readonly workerId = input('');
  /** Host mode: fixed for an existing chat. */
  readonly locked = input(false);
  readonly valueChange = output<MachineTarget>();
  readonly workerChange = output<string>();

  readonly open = signal(false);
  readonly all = ALL_MACHINES;

  /** Online first, then by name. */
  readonly workers = computed(() =>
    [...this.fleet.workers()].sort(
      (a, b) =>
        Number(b.status === 'online') - Number(a.status === 'online') ||
        a.name.localeCompare(b.name),
    ),
  );
  readonly hostName = computed(
    () =>
      this.fleet.workersById()[this.workerId()]?.name ||
      (this.workerId() ? this.workerId() : 'localhost'),
  );
  readonly isAll = computed(() => isAllMachines(this.value()));

  private readonly groupMap = computed(() =>
    Object.fromEntries(this.fleet.machineGroups().map((g) => [g.id, g.worker_ids])),
  );
  readonly resolved = computed(() =>
    resolveTarget(
      this.value(),
      this.fleet.workers().map((w) => w.id),
      this.fleet.labelsById(),
      this.groupMap(),
    ),
  );
  readonly chipText = computed(() => {
    const names = Object.fromEntries(this.fleet.workers().map((w) => [w.id, w.name]));
    const online = new Set(
      this.fleet
        .workers()
        .filter((w) => w.status === 'online')
        .map((w) => w.id),
    );
    return targetChipText(this.value(), names, this.fleet.machineGroups(), this.resolved(), online);
  });

  /** Each label in use with how many machines have it. */
  readonly labelCounts = computed(() =>
    this.fleet.allLabels().map((label) => ({
      label,
      count: this.fleet
        .workers()
        .filter((w) => (w.labels ?? []).some((l) => l.toLowerCase() === label.toLowerCase()))
        .length,
    })),
  );

  isPicked(t: MachineTarget): boolean {
    return sameTarget(this.value(), t);
  }

  pick(t: MachineTarget): void {
    this.open.set(false);
    if (!this.isPicked(t)) this.valueChange.emit(t);
  }

  pickMachine(id: string): void {
    if (this.mode() === 'host') {
      this.open.set(false);
      if (id !== this.workerId()) this.workerChange.emit(id);
      return;
    }
    this.pick({ kind: 'machine', id });
  }

  /** Opens the Machines dialog in select mode on the machines the scope names now. */
  selectMachines(): void {
    this.open.set(false);
    this.ui.openMachines('select', {
      preselect: this.isAll() ? [] : this.resolved(),
      onApply: (ids) =>
        this.valueChange.emit(
          ids.length === 1 ? { kind: 'machine', id: ids[0] } : { kind: 'machines', ids },
        ),
    });
  }
}

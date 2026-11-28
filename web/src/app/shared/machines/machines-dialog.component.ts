import {
  ChangeDetectionStrategy,
  Component,
  OnInit,
  computed,
  inject,
  input,
  output,
  signal,
} from '@angular/core';
import { Worker } from '../../core/models/syre.models';
import { FleetStore } from '../../core/state/fleet.store';
import { AddMachinePanelComponent } from '../../layout/add-machine-panel.component';
import { IconComponent } from '../icon.component';
import { ModalDialogComponent } from '../modal-dialog.component';
import { MachineGroup } from './machine-target';
import { MachineTableComponent } from './machine-table.component';

export type MachinesMode = 'browse' | 'select' | 'add';

/** What the Machines dialog opens on (ModeUiStore.machinesDialog). */
export interface MachinesDialogState {
  mode: MachinesMode;
  /** Select mode: machines checked at the start. */
  preselect?: readonly string[];
  /** Select mode opened from a picker: "Use selection" hands the machines back. */
  onApply?: (ids: string[]) => void;
}

/**
 * One dialog for the fleet (SYR-FJA2T): browse every machine with its specs and labels,
 * select machines (save them as a group, or use them for a chat's machines), or add a
 * machine. The table is the same component in every mode.
 */
@Component({
  selector: 'sy-machines-dialog',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [ModalDialogComponent, MachineTableComponent, AddMachinePanelComponent, IconComponent],
  template: `
    <app-modal-dialog
      labelledBy="machines-title"
      [panelClass]="(mode() === 'add' ? 'max-w-[600px] ' : 'max-w-[980px] ') + 'w-full rounded-[var(--sy-radius-xl)] bg-[var(--sy-surface-2)] p-5 sm:p-6 max-h-[88vh] overflow-y-auto flex flex-col gap-4'"
      (close)="close()"
    >
      <div class="flex items-start justify-between gap-3">
        <div class="min-w-0">
          @if (mode() === 'add') {
            <button
              type="button"
              class="text-[11.5px] text-[var(--sy-text-muted)] hover:text-[var(--sy-text)] inline-flex items-center gap-1 mb-1"
              data-testid="machines-back"
              (click)="toBrowse()"
            >
              <span aria-hidden="true">←</span> All machines
            </button>
          }
          <h3
            id="machines-title"
            class="text-[15px] font-semibold text-[var(--sy-text)]"
            [class.sy-sr-only]="mode() === 'add'"
          >
            {{ title() }}
          </h3>
          @if (mode() === 'select') {
            <p class="m-0 mt-1 text-[12px] text-[var(--sy-text-muted)]">
              {{
                state().onApply
                  ? 'Choose where this chat may send work.'
                  : editing()
                    ? 'Change the machines in ' + editing()!.name + '.'
                    : 'Pick machines to save as a group.'
              }}
            </p>
          }
        </div>
        <div class="flex items-center gap-1.5 shrink-0">
          @if (mode() === 'browse') {
            <button
              type="button"
              class="sy-btn sy-btn-sm"
              data-testid="machines-new-group"
              (click)="newGroup()"
            >
              New group
            </button>
            <button
              type="button"
              class="sy-btn sy-btn-sm sy-btn-soft"
              data-testid="machines-add"
              (click)="toAdd()"
            >
              <app-icon name="plus" [size]="12" />
              Add machine
            </button>
          }
          <button
            type="button"
            (click)="close()"
            aria-label="Esc: close dialog"
            class="text-[11px] text-[var(--sy-text-muted)] hover:text-[var(--sy-text)] pl-2"
          >
            Esc
          </button>
        </div>
      </div>

      @switch (mode()) {
        @case ('add') {
          <sy-add-machine-panel [firstRun]="firstRun()" (inspectMachine)="inspectId($event)" />
        }
        @case ('select') {
          <sy-machine-table [selectable]="true" [(selection)]="selection" />
          <div class="flex flex-wrap items-center justify-between gap-2">
            <div class="flex flex-wrap items-center gap-2">
              @if (naming()) {
                <form
                  class="flex items-center gap-1.5"
                  (submit)="$event.preventDefault(); saveGroup()"
                >
                  <input
                    type="text"
                    class="sy-field sy-field-sm w-[200px]"
                    aria-label="Group name"
                    placeholder="Group name"
                    data-testid="group-name"
                    maxlength="60"
                    [value]="groupName()"
                    (input)="groupName.set($any($event.target).value)"
                    (keydown.escape)="$event.stopPropagation(); naming.set(false)"
                  />
                  <button
                    type="submit"
                    class="sy-btn sy-btn-sm sy-btn-soft"
                    data-testid="group-save"
                    [disabled]="busy() || !groupName().trim() || selection().size === 0"
                  >
                    Save
                  </button>
                  <button type="button" class="sy-btn sy-btn-sm" (click)="naming.set(false)">
                    Cancel
                  </button>
                </form>
              } @else {
                <button
                  type="button"
                  class="sy-btn sy-btn-sm"
                  data-testid="save-as-group"
                  [disabled]="selection().size === 0"
                  (click)="startNaming()"
                >
                  {{ editing() ? 'Save group…' : 'Save as group…' }}
                </button>
              }
            </div>
            <div class="flex items-center gap-1.5">
              <button
                type="button"
                class="sy-btn sy-btn-sm"
                data-testid="select-cancel"
                (click)="cancelSelect()"
              >
                {{ cameFromBrowse() ? 'Back' : 'Cancel' }}
              </button>
              @if (state().onApply) {
                <button
                  type="button"
                  class="sy-btn sy-btn-sm sy-btn-primary"
                  data-testid="use-selection"
                  [disabled]="selection().size === 0"
                  (click)="useSelection()"
                >
                  Use {{ selection().size === 1 ? 'this machine' : selection().size + ' machines' }}
                </button>
              }
            </div>
          </div>
        }
        @default {
          <sy-machine-table (open)="inspect($event)" />
          <section
            class="flex flex-col gap-1.5"
            aria-labelledby="machines-groups-title"
            data-testid="machine-groups"
          >
            <h4
              id="machines-groups-title"
              class="m-0 text-[10.5px] font-semibold uppercase tracking-wider text-[var(--sy-text-muted)]"
            >
              Groups
            </h4>
            @for (g of fleet.machineGroups(); track g.id) {
              <div
                class="flex flex-wrap items-center justify-between gap-2 rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-1)] px-3 py-1.5 text-[12px]"
                [attr.data-group]="g.name"
              >
                <span class="min-w-0">
                  <span class="font-medium text-[var(--sy-text)]">{{ g.name }}</span>
                  <span class="text-[var(--sy-text-muted)]"> · {{ memberNames(g) }}</span>
                </span>
                <span class="flex items-center gap-1">
                  <button
                    type="button"
                    class="sy-btn sy-btn-sm"
                    [attr.data-testid]="'edit-group-' + g.name"
                    (click)="editGroup(g)"
                  >
                    Edit
                  </button>
                  <button
                    type="button"
                    class="sy-btn sy-btn-sm sy-btn-danger"
                    [attr.data-testid]="'delete-group-' + g.name"
                    [disabled]="busy()"
                    (click)="deleteGroup(g)"
                  >
                    Delete
                  </button>
                </span>
              </div>
            } @empty {
              <p class="m-0 text-[12px] text-[var(--sy-text-muted)]">
                No groups yet. New group saves a set of machines to choose in a Commander chat.
              </p>
            }
          </section>
        }
      }
      @if (error()) {
        <p role="alert" class="m-0 text-[12px] text-[#f09595]" data-testid="machines-error">
          {{ error() }}
        </p>
      }
    </app-modal-dialog>
  `,
})
export class MachinesDialogComponent implements OnInit {
  readonly fleet = inject(FleetStore);

  readonly state = input.required<MachinesDialogState>();
  /** The hub's first run: the add pane says "Add your first machine". */
  readonly firstRun = input(false);
  /** Closed; `sawAdd` when the add pane was shown (first run is then dismissed). */
  readonly closed = output<{ sawAdd: boolean }>();
  readonly inspectMachine = output<string>();

  private readonly modeOverride = signal<MachinesMode | null>(null);
  readonly mode = computed(() => this.modeOverride() ?? this.state().mode);
  readonly cameFromBrowse = signal(false);
  readonly selection = signal<ReadonlySet<string>>(new Set());
  readonly editing = signal<MachineGroup | null>(null);
  readonly naming = signal(false);
  readonly groupName = signal('');
  readonly busy = signal(false);
  readonly error = signal('');
  private sawAdd = false;

  readonly title = computed(() => {
    switch (this.mode()) {
      case 'select':
        return 'Select machines';
      default:
        return 'Machines';
    }
  });

  ngOnInit(): void {
    const st = this.state();
    if (st.mode === 'add') this.sawAdd = true;
    if (st.preselect) this.selection.set(new Set(st.preselect));
  }

  memberNames(g: MachineGroup): string {
    const byId = this.fleet.workersById();
    const names = g.worker_ids.map((id) => byId[id]?.name).filter(Boolean);
    return names.length ? names.join(', ') : 'no machines';
  }

  toBrowse(): void {
    this.error.set('');
    this.modeOverride.set('browse');
  }

  toAdd(): void {
    this.sawAdd = true;
    this.modeOverride.set('add');
  }

  newGroup(): void {
    this.editing.set(null);
    this.selection.set(new Set());
    this.cameFromBrowse.set(true);
    this.modeOverride.set('select');
  }

  editGroup(g: MachineGroup): void {
    this.editing.set(g);
    this.selection.set(new Set(g.worker_ids));
    this.cameFromBrowse.set(true);
    this.modeOverride.set('select');
  }

  startNaming(): void {
    this.groupName.set(this.editing()?.name ?? '');
    this.naming.set(true);
  }

  async saveGroup(): Promise<void> {
    const name = this.groupName().trim();
    if (!name || this.selection().size === 0) return;
    this.busy.set(true);
    this.error.set('');
    try {
      const ids = this.orderedSelection();
      await this.fleet.saveGroup({ id: this.editing()?.id, name, worker_ids: ids });
      this.naming.set(false);
      if (this.state().onApply) return; // still choosing for the chat; the group now shows in its picker
      this.editing.set(null);
      this.toBrowse();
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not save the group');
    } finally {
      this.busy.set(false);
    }
  }

  async deleteGroup(g: MachineGroup): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      await this.fleet.deleteGroup(g.id);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not delete the group');
    } finally {
      this.busy.set(false);
    }
  }

  cancelSelect(): void {
    if (this.cameFromBrowse()) {
      this.cameFromBrowse.set(false);
      this.editing.set(null);
      this.naming.set(false);
      this.toBrowse();
      return;
    }
    this.close();
  }

  useSelection(): void {
    const apply = this.state().onApply;
    if (!apply || this.selection().size === 0) return;
    apply(this.orderedSelection());
    this.close();
  }

  inspect(w: Worker): void {
    this.inspectId(w.id);
  }

  inspectId(id: string): void {
    this.inspectMachine.emit(id);
  }

  close(): void {
    this.closed.emit({ sawAdd: this.sawAdd });
  }

  /** The selected machines in fleet order. */
  private orderedSelection(): string[] {
    const sel = this.selection();
    return this.fleet
      .workers()
      .map((w) => w.id)
      .filter((id) => sel.has(id));
  }
}

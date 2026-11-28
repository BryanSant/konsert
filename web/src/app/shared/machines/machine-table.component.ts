import {
  ChangeDetectionStrategy,
  Component,
  computed,
  inject,
  input,
  model,
  output,
  signal,
} from '@angular/core';
import { Worker } from '../../core/models/syre.models';
import { FleetStore } from '../../core/state/fleet.store';
import { LabelChipsComponent } from './label-chips.component';
import {
  MachineSortColumn,
  cpuLiveText,
  cpuText,
  diskText,
  machineMatches,
  machineSortKey,
  osText,
  ramText,
} from './machine-specs';

interface Column {
  id: MachineSortColumn;
  label: string;
  /** Tailwind visibility: narrow screens keep the machine and its one-line spec. */
  cls: string;
}

const COLUMNS: Column[] = [
  { id: 'name', label: 'Machine', cls: '' },
  { id: 'os', label: 'OS', cls: 'hidden lg:table-cell' },
  { id: 'cpu', label: 'CPU', cls: 'hidden md:table-cell' },
  { id: 'ram', label: 'RAM', cls: 'hidden md:table-cell' },
  { id: 'disk', label: 'Disk', cls: 'hidden lg:table-cell' },
  { id: 'load', label: 'Load', cls: 'hidden sm:table-cell' },
  { id: 'tasks', label: 'Tasks', cls: 'hidden sm:table-cell' },
];

/**
 * Every machine with its specs side by side (SYR-FJA2T): one component for the Machines
 * dialog's browse mode (click a row for its details, edit labels inline) and select mode
 * (`selectable`: checkboxes, Select all / Select none over the rows the filter shows).
 */
@Component({
  selector: 'sy-machine-table',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [LabelChipsComponent],
  host: { class: 'flex flex-col gap-2 min-h-0' },
  template: `
    <div class="flex flex-wrap items-center gap-2">
      <input
        type="search"
        class="sy-field sy-field-sm flex-1 min-w-[160px]"
        placeholder="Filter by name, OS or label"
        aria-label="Filter machines"
        data-testid="machine-filter"
        [value]="query()"
        (input)="query.set($any($event.target).value)"
      />
      @if (selectable()) {
        <button
          type="button"
          class="sy-btn sy-btn-sm"
          data-testid="select-all"
          [disabled]="rows().length === 0"
          (click)="selectAll()"
        >
          Select all
        </button>
        <button
          type="button"
          class="sy-btn sy-btn-sm"
          data-testid="select-none"
          [disabled]="selection().size === 0"
          (click)="selectNone()"
        >
          Select none
        </button>
        <span
          class="text-[11.5px] text-[var(--sy-text-muted)]"
          data-testid="selected-count"
          aria-live="polite"
          >{{ selection().size }} selected</span
        >
      }
    </div>

    <div
      class="overflow-auto rounded-[var(--sy-radius-md)] border border-[var(--sy-hairline)] min-h-0"
    >
      <table class="w-full text-left text-[12px] border-collapse" data-testid="machine-table">
        <thead class="sticky top-0 bg-[var(--sy-surface-2)] z-[1]">
          <tr class="text-[10.5px] uppercase tracking-wider text-[var(--sy-text-muted)]">
            @if (selectable()) {
              <th class="w-8 px-2 py-1.5" scope="col"><span class="sy-sr-only">Selected</span></th>
            }
            @for (c of columns; track c.id) {
              <th
                scope="col"
                class="px-2 py-1.5 font-medium"
                [class]="c.cls"
                [attr.aria-sort]="
                  sortCol() === c.id ? (sortAsc() ? 'ascending' : 'descending') : null
                "
              >
                <button
                  type="button"
                  class="inline-flex items-center gap-1 uppercase tracking-wider hover:text-[var(--sy-text)]"
                  [attr.data-testid]="'sort-' + c.id"
                  (click)="sortBy(c.id)"
                >
                  {{ c.label }}
                  @if (sortCol() === c.id) {
                    <span aria-hidden="true">{{ sortAsc() ? '↑' : '↓' }}</span>
                  }
                </button>
              </th>
            }
          </tr>
        </thead>
        <tbody>
          @for (w of rows(); track w.id) {
            <tr
              class="border-t border-[var(--sy-hairline)] align-top hover:bg-white/[0.03]"
              [class.cursor-pointer]="true"
              [class.bg-[var(--sy-blue-soft)]]="selectable() && selection().has(w.id)"
              [attr.data-machine-row]="w.sid || w.id"
              (click)="rowClick(w)"
            >
              @if (selectable()) {
                <td class="px-2 py-2">
                  <input
                    type="checkbox"
                    class="accent-[var(--sy-blue)] cursor-pointer"
                    [attr.aria-label]="'Select ' + w.name"
                    [attr.data-testid]="'select-' + (w.sid || w.id)"
                    [checked]="selection().has(w.id)"
                    (click)="$event.stopPropagation()"
                    (change)="toggle(w.id)"
                  />
                </td>
              }
              <td class="px-2 py-2 min-w-[180px]">
                <div class="flex items-start gap-2 min-w-0">
                  <span
                    class="mt-[5px] h-1.5 w-1.5 rounded-full shrink-0"
                    [class.bg-[var(--sy-green)]]="w.status === 'online'"
                    [class.bg-[#5e6068]]="w.status !== 'online'"
                    role="img"
                    [attr.aria-label]="w.status === 'online' ? 'Online' : 'Offline'"
                  ></span>
                  <div class="min-w-0 flex flex-col gap-1">
                    @if (selectable()) {
                      <span class="font-medium text-[var(--sy-text)] truncate">{{ w.name }}</span>
                    } @else {
                      <button
                        type="button"
                        class="font-medium text-left text-[var(--sy-text)] truncate hover:underline sy-focusable rounded-sm"
                        [attr.data-testid]="'open-machine-' + (w.sid || w.id)"
                        (click)="$event.stopPropagation(); open.emit(w)"
                      >
                        {{ w.name }}
                      </button>
                    }
                    <span class="text-[11px] text-[var(--sy-text-muted)] lg:hidden truncate">{{
                      osText(w)
                    }}</span>
                    <sy-label-chips
                      [labels]="w.labels ?? []"
                      [editable]="editableLabels() && !selectable()"
                      [suggestions]="fleet.allLabels()"
                      [busy]="busy() === w.id"
                      (add)="addLabel(w, $event)"
                      (remove)="removeLabel(w, $event)"
                      (click)="$event.stopPropagation()"
                    />
                  </div>
                </div>
              </td>
              <td class="px-2 py-2 text-[var(--sy-text-secondary)] hidden lg:table-cell">
                {{ osText(w) || '—' }}
              </td>
              <td class="px-2 py-2 text-[var(--sy-text-secondary)] hidden md:table-cell">
                <div class="truncate max-w-[200px]">{{ cpuText(w) || '—' }}</div>
                @if (cpuLiveText(w); as live) {
                  <div class="text-[11px] text-[var(--sy-text-muted)]">{{ live }} in use</div>
                }
              </td>
              <td
                class="px-2 py-2 text-[var(--sy-text-secondary)] font-mono-ide text-[11px] hidden md:table-cell whitespace-nowrap"
              >
                {{ ramText(w) || '—' }}
              </td>
              <td
                class="px-2 py-2 text-[var(--sy-text-secondary)] font-mono-ide text-[11px] hidden lg:table-cell whitespace-nowrap"
              >
                {{ diskText(w) || '—' }}
              </td>
              <td
                class="px-2 py-2 hidden sm:table-cell whitespace-nowrap"
                [style.color]="heatColor(w)"
              >
                {{ loadLabel(w) }}
              </td>
              <td class="px-2 py-2 text-[var(--sy-text-secondary)] hidden sm:table-cell">
                {{ w.active_tasks ?? 0 }}
              </td>
            </tr>
          } @empty {
            <tr>
              <td
                [attr.colspan]="columns.length + (selectable() ? 1 : 0)"
                class="px-3 py-4 text-center text-[var(--sy-text-muted)]"
                data-testid="machine-table-empty"
              >
                {{ fleet.workers().length ? 'No machine matches the filter.' : 'No machines yet.' }}
              </td>
            </tr>
          }
        </tbody>
      </table>
    </div>
    @if (error()) {
      <p role="alert" class="m-0 text-[11.5px] text-[#f09595]">{{ error() }}</p>
    }
  `,
})
export class MachineTableComponent {
  readonly fleet = inject(FleetStore);

  readonly selectable = input(false);
  /** Selected machine IDs (select mode). */
  readonly selection = model<ReadonlySet<string>>(new Set());
  /** Browse mode edits labels inline. */
  readonly editableLabels = input(true);
  readonly open = output<Worker>();

  readonly columns = COLUMNS;
  readonly query = signal('');
  readonly sortCol = signal<MachineSortColumn>('status');
  readonly sortAsc = signal(true);
  readonly busy = signal('');
  readonly error = signal('');

  readonly osText = osText;
  readonly cpuText = cpuText;
  readonly cpuLiveText = cpuLiveText;
  readonly ramText = ramText;
  readonly diskText = diskText;

  /** The rows the filter shows, sorted (online first by default, then by name). */
  readonly rows = computed(() => {
    const q = this.query();
    const col = this.sortCol();
    const dir = this.sortAsc() ? 1 : -1;
    const key = machineSortKey[col];
    return this.fleet
      .workers()
      .filter((w) => machineMatches(w, w.labels ?? [], q))
      .sort((a, b) => {
        const ka = key(a);
        const kb = key(b);
        const c =
          typeof ka === 'number' && typeof kb === 'number'
            ? ka - kb
            : String(ka).localeCompare(String(kb));
        return c * dir || a.name.localeCompare(b.name);
      });
  });

  sortBy(col: MachineSortColumn): void {
    if (this.sortCol() === col) {
      this.sortAsc.set(!this.sortAsc());
    } else {
      this.sortCol.set(col);
      this.sortAsc.set(col === 'name' || col === 'os' || col === 'status');
    }
  }

  rowClick(w: Worker): void {
    if (this.selectable()) this.toggle(w.id);
    else this.open.emit(w);
  }

  toggle(id: string): void {
    const next = new Set(this.selection());
    if (next.has(id)) next.delete(id);
    else next.add(id);
    this.selection.set(next);
  }

  /** Selects every row the filter shows (keeping any hidden ones already selected). */
  selectAll(): void {
    const next = new Set(this.selection());
    for (const w of this.rows()) next.add(w.id);
    this.selection.set(next);
  }

  /** Clears the rows the filter shows. */
  selectNone(): void {
    const shown = new Set(this.rows().map((w) => w.id));
    this.selection.set(new Set([...this.selection()].filter((id) => !shown.has(id))));
  }

  loadLabel(w: Worker): string {
    if (w.status !== 'online') return 'offline';
    const cpu = w.metrics ? Math.round(w.metrics.cpu_percent) : null;
    const heat = this.fleet.heatByWorker()[w.id];
    if (heat) return `${Math.round(heat.score * 100)}%`;
    return cpu == null ? '—' : `${cpu}%`;
  }

  heatColor(w: Worker): string {
    const heat = this.fleet.heatByWorker()[w.id];
    if (w.status !== 'online' || !heat) return 'var(--sy-text-muted)';
    return heat.score > 0.75
      ? 'var(--sy-red)'
      : heat.score >= 0.4
        ? 'var(--sy-amber)'
        : 'var(--sy-text-secondary)';
  }

  async addLabel(w: Worker, label: string): Promise<void> {
    await this.run(w.id, () => this.fleet.addLabel(w.id, label));
  }

  async removeLabel(w: Worker, label: string): Promise<void> {
    await this.run(w.id, () => this.fleet.removeLabel(w.id, label));
  }

  private async run(id: string, fn: () => Promise<void>): Promise<void> {
    this.busy.set(id);
    this.error.set('');
    try {
      await fn();
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not change the label');
    } finally {
      this.busy.set('');
    }
  }
}

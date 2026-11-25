import { ChangeDetectionStrategy, Component, computed, effect, input, output, signal, untracked } from '@angular/core';
import { AgentModel } from '../../../core/models/agent-chat.models';
import { ChipTriggerComponent } from '../../../shared/chip-trigger.component';
import { IconComponent } from '../../../shared/icon.component';
import { PopoverComponent, PopoverTriggerDirective } from '../../../shared/popover.component';
import { TooltipDirective } from '../../../shared/tooltip.directive';
import { effortsFor, filterModels, modelTriggerText } from './composer-logic';

let nextId = 0;

/**
 * "Sonnet 5.5 Med" trigger and a popover with model search, a list with descriptions and
 * an effort segment showing only the levels the selected model supports.
 *
 * Keyboard: type to filter, ArrowUp/Down move, Enter picks; Left/Right on the effort row.
 */
@Component({
  selector: 'sy-model-picker',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [PopoverComponent, PopoverTriggerDirective, ChipTriggerComponent, IconComponent, TooltipDirective],
  template: `
    <sy-popover label="Model and effort" role="dialog" placement="top-end" panelClass="w-[340px] !p-0" [closeOnSelect]="false" [(open)]="open">
      <button
        syChip
        syPopoverTrigger
        data-testid="model-trigger"
        [disabled]="disabled() || models().length === 0"
        [attr.aria-label]="trigger().label + ', model and effort'"
        [syTooltip]="trigger().tooltip"
      >
        <span class="max-w-[170px] truncate text-[var(--sy-text)]">{{ trigger().label }}</span>
      </button>

      <div class="p-2 pb-1">
        <div class="flex items-center gap-2 rounded-[var(--sy-radius-md)] bg-black/25 px-2.5 h-8">
          <app-icon name="search" [size]="13" class="text-[var(--sy-text-muted)]" />
          <input
            type="text"
            role="combobox"
            autofocus
            data-testid="model-search"
            aria-label="Search models"
            aria-autocomplete="list"
            [attr.aria-controls]="listId"
            [attr.aria-expanded]="true"
            [attr.aria-activedescendant]="activeId()"
            placeholder="Search models..."
            class="flex-1 min-w-0 bg-transparent text-[12.5px] text-[var(--sy-text)] placeholder-[#80828a] outline-none"
            [value]="query()"
            (input)="onQuery($event)"
            (keydown)="onSearchKey($event)"
          />
        </div>
      </div>
      <div [id]="listId" role="listbox" aria-label="Models" class="max-h-[260px] overflow-y-auto px-1.5 pb-1.5">
        @for (m of filtered(); track m.id; let i = $index) {
          <div
            role="option"
            [id]="optionId(i)"
            [attr.aria-selected]="m.id === selectedId()"
            [attr.data-model]="m.id"
            class="sy-menu-item items-center"
            [class.sy-active]="i === active()"
            (mouseenter)="active.set(i)"
            (click)="pick(m)"
          >
            <span class="flex-1 min-w-0">
              <span class="sy-menu-label block truncate">{{ m.display_name }}</span>
              @if (m.description) {
                <span class="sy-menu-desc block truncate">{{ m.description }}</span>
              }
            </span>
            <span class="w-3.5 shrink-0">
              @if (m.id === selectedId()) {
                <app-icon name="check" [size]="14" />
              }
            </span>
          </div>
        } @empty {
          <div class="px-3 py-4 text-center text-[12px] text-[var(--sy-text-muted)]">No models match "{{ query() }}"</div>
        }
      </div>
      @if (efforts().length > 0) {
        <div class="flex items-center gap-2 px-3 py-2.5 border-t border-[var(--sy-hairline)]" data-testid="effort-row">
          <span class="text-[11.5px] text-[var(--sy-text-muted)] shrink-0" [id]="effortLabelId">Effort</span>
          <div role="radiogroup" [attr.aria-labelledby]="effortLabelId" class="flex flex-1 flex-wrap gap-0.5 rounded-[var(--sy-radius-md)] bg-black/25 p-0.5" (keydown)="onEffortKey($event)">
            @for (e of efforts(); track e.id) {
              <button
                type="button"
                role="radio"
                class="flex-1 min-w-[40px] rounded-[6px] px-1.5 py-1 text-[11.5px] transition-colors cursor-pointer outline-none focus-visible:shadow-[var(--sy-focus-ring)]"
                [class.bg-[var(--sy-raised-strong)]]="e.id === effort()"
                [class.text-[var(--sy-text)]]="e.id === effort()"
                [class.font-semibold]="e.id === effort()"
                [class.text-[var(--sy-text-secondary)]]="e.id !== effort()"
                [attr.aria-checked]="e.id === effort()"
                [attr.tabindex]="e.id === effort() || (!effort() && $first) ? 0 : -1"
                [attr.aria-label]="e.label"
                [attr.data-effort]="e.id"
                (click)="effortChange.emit(e.id)"
              >
                {{ e.short || e.label }}
              </button>
            }
          </div>
        </div>
      }
    </sy-popover>
  `,
})
export class ModelPickerComponent {
  readonly models = input<AgentModel[]>([]);
  readonly selectedId = input('');
  readonly effort = input('');
  readonly disabled = input(false);
  readonly modelChange = output<string>();
  readonly effortChange = output<string>();

  readonly open = signal(false);
  readonly query = signal('');
  readonly active = signal(0);
  private readonly uid = ++nextId;
  readonly listId = `sy-model-list-${this.uid}`;
  readonly effortLabelId = `sy-effort-label-${this.uid}`;

  readonly current = computed(() => this.models().find((m) => m.id === this.selectedId()) ?? null);
  readonly trigger = computed(() => modelTriggerText(this.current(), this.effort()));
  readonly filtered = computed(() => filterModels(this.models(), this.query()));
  readonly efforts = computed(() => effortsFor(this.current()));
  readonly activeId = computed(() => (this.filtered().length ? this.optionId(this.active()) : null));

  constructor() {
    effect(() => {
      if (this.open()) {
        untracked(() => {
          this.query.set('');
          const idx = this.models().findIndex((m) => m.id === this.selectedId());
          this.active.set(Math.max(0, idx));
        });
      }
    });
  }

  optionId(i: number): string {
    return `sy-model-opt-${this.uid}-${i}`;
  }

  onQuery(ev: Event): void {
    this.query.set((ev.target as HTMLInputElement).value);
    this.active.set(0);
  }

  onSearchKey(ev: KeyboardEvent): void {
    const n = this.filtered().length;
    if (ev.key === 'ArrowDown' && n) {
      ev.preventDefault();
      this.active.update((i) => (i + 1) % n);
      this.scrollActive();
    } else if (ev.key === 'ArrowUp' && n) {
      ev.preventDefault();
      this.active.update((i) => (i - 1 + n) % n);
      this.scrollActive();
    } else if (ev.key === 'Enter') {
      ev.preventDefault();
      const m = this.filtered()[this.active()];
      if (m) this.pick(m);
    }
  }

  private scrollActive(): void {
    queueMicrotask(() => document.getElementById(this.optionId(this.active()))?.scrollIntoView?.({ block: 'nearest' }));
  }

  /** Picking a model keeps the popover open when it has efforts to choose. */
  pick(m: AgentModel): void {
    this.modelChange.emit(m.id);
    if (effortsFor(m).length === 0) this.open.set(false);
  }

  onEffortKey(ev: KeyboardEvent): void {
    const list = this.efforts();
    if (!list.length || (ev.key !== 'ArrowLeft' && ev.key !== 'ArrowRight')) return;
    ev.preventDefault();
    const idx = Math.max(0, list.findIndex((e) => e.id === this.effort()));
    const next = ev.key === 'ArrowRight' ? Math.min(list.length - 1, idx + 1) : Math.max(0, idx - 1);
    this.effortChange.emit(list[next].id);
    const group = ev.currentTarget as HTMLElement;
    queueMicrotask(() => group.querySelector<HTMLElement>(`[data-effort="${list[next].id}"]`)?.focus());
  }
}

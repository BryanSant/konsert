import {
  ChangeDetectionStrategy,
  Component,
  ElementRef,
  computed,
  input,
  output,
  signal,
  viewChild,
} from '@angular/core';

let nextListId = 0;

/**
 * A machine's operator labels as chips (SYR-FJA2T). With `editable`, each chip has a remove
 * button and "Add label" opens a field with type-ahead over the labels already in use.
 * Used by the machine table, the machine summary and machine detail.
 */
@Component({
  selector: 'sy-label-chips',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'inline-flex flex-wrap items-center gap-1 min-w-0' },
  template: `
    @for (l of labels(); track l) {
      <span class="sy-label-chip" data-testid="machine-label-chip">
        <span class="truncate max-w-[140px]" data-testid="machine-label">{{ l }}</span>
        @if (editable()) {
          <button
            type="button"
            class="sy-label-x"
            [attr.aria-label]="'Remove label ' + l"
            [attr.data-testid]="'remove-label-' + l"
            [disabled]="busy()"
            (click)="$event.stopPropagation(); remove.emit(l)"
          >
            ×
          </button>
        }
      </span>
    }
    @if (editable()) {
      @if (adding()) {
        <input
          #field
          type="text"
          class="sy-field sy-field-sm w-[150px]"
          data-testid="label-input"
          aria-label="New label"
          maxlength="40"
          [attr.list]="listId"
          [value]="draft()"
          (input)="draft.set($any($event.target).value)"
          (click)="$event.stopPropagation()"
          (keydown.enter)="$event.preventDefault(); commit()"
          (keydown.escape)="$event.stopPropagation(); cancel()"
          (blur)="commit()"
        />
        <datalist [id]="listId">
          @for (s of offered(); track s) {
            <option [value]="s"></option>
          }
        </datalist>
      } @else {
        <button
          type="button"
          class="sy-label-add"
          data-testid="add-label"
          [disabled]="busy()"
          (click)="$event.stopPropagation(); open()"
        >
          + Label
        </button>
      }
    }
  `,
  styles: [
    `
      .sy-label-chip {
        display: inline-flex;
        align-items: center;
        gap: 2px;
        height: 18px;
        padding: 0 6px;
        border-radius: 999px;
        font-size: 10.5px;
        line-height: 1;
        color: var(--sy-text-secondary);
        background: rgb(255 255 255 / 0.06);
        box-shadow: inset 0 0 0 1px rgb(255 255 255 / 0.08);
      }
      .sy-label-x {
        margin-right: -3px;
        padding: 0 2px;
        font-size: 12px;
        line-height: 1;
        color: var(--sy-text-muted);
        cursor: pointer;
      }
      .sy-label-x:hover {
        color: var(--sy-text);
      }
      .sy-label-add {
        height: 18px;
        padding: 0 6px;
        border-radius: 999px;
        font-size: 10.5px;
        color: var(--sy-text-muted);
        box-shadow: inset 0 0 0 1px rgb(255 255 255 / 0.1);
        cursor: pointer;
      }
      .sy-label-add:hover {
        color: var(--sy-text);
      }
      .sy-label-x:focus-visible,
      .sy-label-add:focus-visible {
        outline: none;
        box-shadow: var(--sy-focus-ring);
      }
    `,
  ],
})
export class LabelChipsComponent {
  readonly labels = input<readonly string[]>([]);
  readonly editable = input(false);
  /** Labels already in use elsewhere, offered as type-ahead. */
  readonly suggestions = input<readonly string[]>([]);
  readonly busy = input(false);
  readonly add = output<string>();
  readonly remove = output<string>();

  readonly adding = signal(false);
  readonly draft = signal('');
  readonly listId = `sy-labels-${nextListId++}`;
  private readonly field = viewChild<ElementRef<HTMLInputElement>>('field');

  readonly offered = computed(() => {
    const have = new Set(this.labels().map((l) => l.toLowerCase()));
    return this.suggestions().filter((s) => !have.has(s.toLowerCase()));
  });

  open(): void {
    this.draft.set('');
    this.adding.set(true);
    queueMicrotask(() => this.field()?.nativeElement.focus());
  }

  commit(): void {
    if (!this.adding()) return;
    const l = this.draft().trim().replace(/\s+/g, ' ');
    this.adding.set(false);
    if (l && !this.labels().some((x) => x.toLowerCase() === l.toLowerCase())) this.add.emit(l);
  }

  cancel(): void {
    this.adding.set(false);
  }
}

import {
  ChangeDetectionStrategy,
  Component,
  ElementRef,
  OnDestroy,
  afterNextRender,
  input,
  output,
  viewChild,
} from '@angular/core';

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

/**
 * Accessible modal shell built on the native <dialog> element.
 *
 * `showModal()` gives dialog semantics, aria-modal behaviour and a focus trap (the
 * rest of the page becomes inert). On open, focus moves to the first `[autofocus]`
 * element (or the first focusable one); on close it returns to whatever was focused
 * before. Escape and backdrop clicks emit `close`; the parent removes the component.
 */
@Component({
  selector: 'app-modal-dialog',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <dialog
      #dialog
      class="ks-dialog"
      aria-modal="true"
      [attr.aria-labelledby]="labelledBy() || null"
      [attr.aria-label]="label() || null"
      [attr.aria-describedby]="describedBy() || null"
      (cancel)="onCancel($event)"
    >
      <div
        class="ks-backdrop fixed inset-0 flex justify-center px-4"
        [class.items-center]="align() === 'center'"
        [class.items-start]="align() === 'top'"
        [class.pt-[12vh]]="align() === 'top'"
        (click)="close.emit()"
      >
        <div [class]="'ks-modal ' + panelClass()" (click)="$event.stopPropagation()">
          <ng-content />
        </div>
      </div>
    </dialog>
  `,
})
export class ModalDialogComponent implements OnDestroy {
  /** id of the element that names the dialog (usually its heading). */
  readonly labelledBy = input<string>('');
  /** Accessible name when there is no visible heading. */
  readonly label = input<string>('');
  readonly describedBy = input<string>('');
  /** Classes for the visible panel (size, colours, padding). */
  readonly panelClass = input<string>('');
  readonly align = input<'center' | 'top'>('center');

  readonly close = output<void>();

  private readonly dialogRef = viewChild.required<ElementRef<HTMLDialogElement>>('dialog');
  private readonly previouslyFocused: HTMLElement | null =
    typeof document !== 'undefined' && document.activeElement instanceof HTMLElement
      ? document.activeElement
      : null;

  constructor() {
    afterNextRender(() => {
      const dialog = this.dialogRef().nativeElement;
      if (typeof dialog.showModal === 'function') {
        if (!dialog.open) dialog.showModal();
      } else {
        dialog.setAttribute('open', '');
      }
      this.focusInitial(dialog);
    });
  }

  onCancel(ev: Event): void {
    // Keep the element mounted until the parent removes us (so focus restore runs once).
    ev.preventDefault();
    this.close.emit();
  }

  private focusInitial(dialog: HTMLDialogElement): void {
    const target =
      dialog.querySelector<HTMLElement>('[autofocus]') ?? dialog.querySelector<HTMLElement>(FOCUSABLE);
    target?.focus();
  }

  ngOnDestroy(): void {
    const dialog = this.dialogRef().nativeElement;
    if (dialog.open && typeof dialog.close === 'function') dialog.close();
    const prev = this.previouslyFocused;
    if (prev && prev.isConnected) {
      prev.focus();
    }
  }
}

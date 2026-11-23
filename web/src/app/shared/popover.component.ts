import { DOCUMENT } from '@angular/common';
import {
  ChangeDetectionStrategy,
  Component,
  Directive,
  ElementRef,
  OnDestroy,
  afterRenderEffect,
  contentChild,
  effect,
  inject,
  input,
  model,
  output,
  untracked,
  viewChild,
} from '@angular/core';

export type PopoverPlacement = 'top-start' | 'top-end' | 'bottom-start' | 'bottom-end';
export type PopoverRole = 'menu' | 'listbox' | 'dialog';

let nextId = 0;

/** Marks the element that toggles a `<ks-popover>`. */
@Directive({ selector: '[ksPopoverTrigger]', standalone: true })
export class PopoverTriggerDirective {
  readonly el = inject<ElementRef<HTMLElement>>(ElementRef);
}

const ITEM_SELECTOR = '[role="menuitem"],[role="menuitemradio"],[role="menuitemcheckbox"]';

/**
 * An anchored, elevated popover (menu, listbox or small dialog) without hard borders.
 *
 * ```html
 * <ks-popover label="Approval mode" [(open)]="open">
 *   <button ksPopoverTrigger class="ks-chip">…</button>
 *   <button role="menuitemradio" class="ks-menu-item">…</button>
 * </ks-popover>
 * ```
 *
 * Keyboard: ArrowDown/ArrowUp on the trigger open it; inside a menu the arrows, Home and
 * End move between items, Enter/Space activate (native buttons), Escape closes and returns
 * focus to the trigger, Tab closes. A click outside closes it. Activating a menu item
 * closes the menu (unless `closeOnSelect` is false).
 */
@Component({
  selector: 'ks-popover',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: {
    class: 'relative inline-flex',
    '(keydown)': 'onHostKeydown($event)',
  },
  template: `
    <ng-content select="[ksPopoverTrigger]" />
    @if (open()) {
      <div
        #panel
        [id]="panelId"
        [attr.role]="role()"
        [attr.aria-label]="label()"
        [attr.aria-orientation]="role() === 'menu' ? 'vertical' : null"
        tabindex="-1"
        class="ks-menu absolute z-50 outline-none"
        [class]="panelClass()"
        [class.bottom-full]="placement().startsWith('top')"
        [class.mb-2]="placement().startsWith('top')"
        [class.top-full]="placement().startsWith('bottom')"
        [class.mt-2]="placement().startsWith('bottom')"
        [class.left-0]="placement().endsWith('start')"
        [class.right-0]="placement().endsWith('end')"
        (click)="onPanelClick($event)"
        (keydown)="onPanelKeydown($event)"
      >
        <ng-content />
      </div>
    }
  `,
})
export class PopoverComponent implements OnDestroy {
  readonly open = model(false);
  readonly label = input.required<string>();
  readonly role = input<PopoverRole>('menu');
  readonly placement = input<PopoverPlacement>('top-start');
  readonly panelClass = input('');
  readonly closeOnSelect = input(true);
  readonly closed = output<void>();

  readonly panelId = `ks-popover-${++nextId}`;
  private readonly trigger = contentChild(PopoverTriggerDirective);
  private readonly panel = viewChild<ElementRef<HTMLElement>>('panel');
  private readonly host = inject<ElementRef<HTMLElement>>(ElementRef);
  private readonly doc = inject(DOCUMENT);
  private wasOpen = false;

  private readonly onDocPointer = (ev: Event) => {
    if (!this.open()) return;
    const t = ev.target as Node | null;
    if (t && this.host.nativeElement.contains(t)) return;
    this.close(false);
  };

  private readonly onTriggerClick = () => this.open.update((v) => !v);

  constructor() {
    // Wire the trigger's ARIA and click handler once it is projected.
    effect((onCleanup) => {
      const trig = this.trigger()?.el.nativeElement;
      if (!trig) return;
      trig.setAttribute('aria-haspopup', this.role() === 'dialog' ? 'dialog' : this.role());
      trig.setAttribute('aria-controls', this.panelId);
      trig.addEventListener('click', this.onTriggerClick);
      onCleanup(() => trig.removeEventListener('click', this.onTriggerClick));
    });
    effect(() => {
      const isOpen = this.open();
      this.trigger()?.el.nativeElement.setAttribute('aria-expanded', String(isOpen));
      if (isOpen) {
        this.doc.addEventListener('pointerdown', this.onDocPointer, true);
      } else {
        this.doc.removeEventListener('pointerdown', this.onDocPointer, true);
        if (untracked(() => this.wasOpen)) this.closed.emit();
      }
      this.wasOpen = isOpen;
    });
    // Move focus into the panel when it opens.
    afterRenderEffect(() => {
      const panel = this.panel()?.nativeElement;
      if (!panel) return;
      untracked(() => this.focusInitial(panel));
    });
  }

  private focusInitial(panel: HTMLElement): void {
    if (panel.contains(this.doc.activeElement)) return;
    const auto = panel.querySelector<HTMLElement>('[autofocus],input,textarea');
    if (auto) {
      auto.focus();
      return;
    }
    const items = this.items(panel);
    const checked = items.find((i) => i.getAttribute('aria-checked') === 'true');
    (checked ?? items[0] ?? panel).focus();
  }

  private items(panel = this.panel()?.nativeElement): HTMLElement[] {
    if (!panel) return [];
    return Array.from(panel.querySelectorAll<HTMLElement>(ITEM_SELECTOR)).filter(
      (el) => el.getAttribute('aria-disabled') !== 'true' && !(el as HTMLButtonElement).disabled,
    );
  }

  /** Closes the popover; returns focus to the trigger unless the user clicked elsewhere. */
  close(restoreFocus = true): void {
    if (!this.open()) return;
    this.open.set(false);
    if (restoreFocus) this.trigger()?.el.nativeElement.focus();
  }

  onHostKeydown(ev: KeyboardEvent): void {
    const trig = this.trigger()?.el.nativeElement;
    if (!this.open() && trig && ev.target === trig && (ev.key === 'ArrowDown' || ev.key === 'ArrowUp')) {
      ev.preventDefault();
      this.open.set(true);
    }
  }

  onPanelKeydown(ev: KeyboardEvent): void {
    if (ev.key === 'Escape') {
      ev.preventDefault();
      ev.stopPropagation();
      this.close(true);
      return;
    }
    if (ev.key === 'Tab') {
      this.close(false);
      return;
    }
    if (this.role() !== 'menu') return;
    const items = this.items();
    if (items.length === 0) return;
    const idx = items.indexOf(this.doc.activeElement as HTMLElement);
    let next = -1;
    switch (ev.key) {
      case 'ArrowDown':
        next = idx < 0 ? 0 : (idx + 1) % items.length;
        break;
      case 'ArrowUp':
        next = idx < 0 ? items.length - 1 : (idx - 1 + items.length) % items.length;
        break;
      case 'Home':
        next = 0;
        break;
      case 'End':
        next = items.length - 1;
        break;
      default:
        return;
    }
    ev.preventDefault();
    items[next].focus();
  }

  onPanelClick(ev: MouseEvent): void {
    if (!this.closeOnSelect()) return;
    const item = (ev.target as HTMLElement | null)?.closest<HTMLElement>(ITEM_SELECTOR);
    if (item && item.getAttribute('aria-disabled') !== 'true') {
      // Let the item's own (click) handler run first.
      queueMicrotask(() => this.close(true));
    }
  }

  ngOnDestroy(): void {
    this.doc.removeEventListener('pointerdown', this.onDocPointer, true);
  }
}

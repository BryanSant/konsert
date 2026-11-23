import { DOCUMENT } from '@angular/common';
import { Directive, ElementRef, OnDestroy, effect, inject, input } from '@angular/core';

/** Delay before the first tooltip appears on hover. */
export const TOOLTIP_DELAY_MS = 450;
/** After a tooltip hides, others open instantly for this long (moving between controls). */
export const TOOLTIP_WARM_MS = 600;

let nextId = 0;
/** Shared across instances: when the last tooltip closed (for the "instant" handoff). */
let lastHiddenAt = 0;
let anyOpen: TooltipDirective | null = null;

/** Test hook: forget the warm state between specs. */
export function resetTooltipWarmState(): void {
  lastHiddenAt = 0;
  anyOpen = null;
}

/**
 * `ksTooltip="Attach file"`: a light pill above the control with a quick fly-up.
 *
 * - First hover waits {@link TOOLTIP_DELAY_MS}; moving to another control while warm shows
 *   it instantly.
 * - Keyboard focus shows it too (no delay); Escape, blur and pointer leave hide it.
 * - The pill is linked with aria-describedby (skipped when it repeats the aria-label).
 * - It flips below the control when there is no room above; the animation is disabled
 *   under prefers-reduced-motion.
 */
@Directive({
  selector: '[ksTooltip]',
  standalone: true,
  host: {
    '(mouseenter)': 'onEnter()',
    '(mouseleave)': 'hide()',
    '(focusin)': 'onFocus()',
    '(focusout)': 'hide()',
    '(pointerdown)': 'hide()',
    '(keydown.escape)': 'hide()',
  },
})
export class TooltipDirective implements OnDestroy {
  readonly ksTooltip = input<string | null | undefined>('');
  /** Preferred side; flips when it would leave the viewport. */
  readonly ksTooltipPlacement = input<'top' | 'bottom'>('top');

  private readonly host = inject<ElementRef<HTMLElement>>(ElementRef);
  private readonly doc = inject(DOCUMENT);
  readonly id = `ks-tooltip-${++nextId}`;
  private tip: HTMLElement | null = null;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private shown = false;

  constructor() {
    effect(() => {
      const text = (this.ksTooltip() ?? '').trim();
      this.sync(text);
    });
  }

  private sync(text: string): void {
    const el = this.host.nativeElement;
    if (!text) {
      this.hide();
      this.tip?.remove();
      this.tip = null;
      this.unlinkDescribedBy();
      return;
    }
    if (!this.tip) {
      this.tip = this.doc.createElement('div');
      this.tip.id = this.id;
      this.tip.className = 'ks-tooltip';
      this.tip.setAttribute('role', 'tooltip');
      this.tip.hidden = true;
      this.doc.body.appendChild(this.tip);
    }
    this.tip.textContent = text;
    const label = (el.getAttribute('aria-label') ?? '').trim();
    if (label && label.toLowerCase() === text.toLowerCase()) {
      this.unlinkDescribedBy();
    } else {
      this.linkDescribedBy();
    }
    if (this.shown) this.position();
  }

  private linkDescribedBy(): void {
    const el = this.host.nativeElement;
    const ids = (el.getAttribute('aria-describedby') ?? '').split(/\s+/).filter(Boolean);
    if (!ids.includes(this.id)) el.setAttribute('aria-describedby', [...ids, this.id].join(' '));
  }

  private unlinkDescribedBy(): void {
    const el = this.host.nativeElement;
    const ids = (el.getAttribute('aria-describedby') ?? '').split(/\s+/).filter((i) => i && i !== this.id);
    if (ids.length) el.setAttribute('aria-describedby', ids.join(' '));
    else el.removeAttribute('aria-describedby');
  }

  onEnter(): void {
    if (!this.tip) return;
    const warm = anyOpen !== null || Date.now() - lastHiddenAt < TOOLTIP_WARM_MS;
    this.clearTimer();
    if (warm) {
      this.show(false);
    } else {
      this.timer = setTimeout(() => this.show(true), TOOLTIP_DELAY_MS);
    }
  }

  onFocus(): void {
    // Only keyboard focus (not a mouse click) shows the tooltip.
    const el = this.host.nativeElement;
    let keyboard = true;
    try {
      keyboard = el.matches(':focus-visible');
    } catch {
      keyboard = true; // engines without :focus-visible support
    }
    if (!keyboard || !this.tip) return;
    this.clearTimer();
    this.show(anyOpen === null);
  }

  private show(animate: boolean): void {
    if (!this.tip) return;
    if (anyOpen && anyOpen !== this) anyOpen.hide(true);
    anyOpen = this;
    this.shown = true;
    this.tip.hidden = false;
    this.tip.classList.toggle('ks-tooltip-animate', animate && !this.reducedMotion());
    this.position();
  }

  hide(handoff = false): void {
    this.clearTimer();
    if (!this.shown || !this.tip) return;
    this.shown = false;
    this.tip.hidden = true;
    this.tip.classList.remove('ks-tooltip-animate');
    if (anyOpen === this) anyOpen = null;
    if (!handoff) lastHiddenAt = Date.now();
  }

  /** True while the pill is visible (for tests). */
  get visible(): boolean {
    return this.shown;
  }

  private position(): void {
    if (!this.tip) return;
    const r = this.host.nativeElement.getBoundingClientRect();
    const gap = 8;
    const tipH = this.tip.offsetHeight || 24;
    const vh = this.doc.defaultView?.innerHeight ?? 800;
    const vw = this.doc.defaultView?.innerWidth ?? 1200;
    let below = this.ksTooltipPlacement() === 'bottom';
    if (!below && r.top - gap - tipH < 4) below = true;
    else if (below && r.bottom + gap + tipH > vh - 4) below = false;
    const tipW = this.tip.offsetWidth || 0;
    const half = tipW / 2;
    const cx = Math.min(Math.max(r.left + r.width / 2, half + 6), vw - half - 6);
    this.tip.style.left = `${cx}px`;
    this.tip.style.top = below ? `${r.bottom + gap}px` : `${r.top - gap - tipH}px`;
    this.tip.classList.toggle('ks-tooltip-below', below);
  }

  private reducedMotion(): boolean {
    return !!this.doc.defaultView?.matchMedia?.('(prefers-reduced-motion: reduce)').matches;
  }

  private clearTimer(): void {
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = null;
    }
  }

  ngOnDestroy(): void {
    this.hide();
    this.tip?.remove();
    this.tip = null;
  }
}

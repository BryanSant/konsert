import { Component, signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ChipTriggerComponent } from './chip-trigger.component';
import { PopoverComponent, PopoverTriggerDirective } from './popover.component';
import { TOOLTIP_DELAY_MS, TooltipDirective, resetTooltipWarmState } from './tooltip.directive';

@Component({
  standalone: true,
  imports: [TooltipDirective],
  template: `
    <button id="a" type="button" aria-label="Attach file" syTooltip="Attach file">+</button>
    <button id="b" type="button" aria-label="Approval mode" [syTooltip]="tip()">S</button>
  `,
})
class TooltipHost {
  readonly tip = signal('Full access');
}

@Component({
  standalone: true,
  imports: [PopoverComponent, PopoverTriggerDirective, ChipTriggerComponent],
  template: `
    <button id="outside" type="button">outside</button>
    <sy-popover label="Approval mode" [(open)]="open">
      <button syChip syPopoverTrigger id="trigger">Mode</button>
      @for (m of modes; track m) {
        <button
          type="button"
          role="menuitemradio"
          class="sy-menu-item"
          [id]="'item-' + m"
          [attr.aria-checked]="m === current()"
          [attr.aria-disabled]="m === 'locked' ? 'true' : null"
          (click)="current.set(m)"
        >
          {{ m }}
        </button>
      }
    </sy-popover>
  `,
})
class PopoverHost {
  readonly open = signal(false);
  readonly current = signal('accept');
  readonly modes = ['ask', 'accept', 'locked', 'full'];
}

function key(el: Element, k: string): KeyboardEvent {
  const ev = new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true });
  el.dispatchEvent(ev);
  return ev;
}

describe('TooltipDirective', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    resetTooltipWarmState();
  });
  afterEach(() => {
    vi.useRealTimers();
    TestBed.resetTestingModule();
    document.querySelectorAll('.sy-tooltip').forEach((n) => n.remove());
  });

  function setup() {
    const fixture = TestBed.createComponent(TooltipHost);
    document.body.appendChild(fixture.nativeElement);
    fixture.detectChanges();
    TestBed.tick();
    const a = fixture.nativeElement.querySelector('#a') as HTMLButtonElement;
    const b = fixture.nativeElement.querySelector('#b') as HTMLButtonElement;
    return { fixture, a, b };
  }

  function tipFor(el: HTMLElement): HTMLElement | null {
    const tips = Array.from(document.querySelectorAll<HTMLElement>('.sy-tooltip'));
    const described = el.getAttribute('aria-describedby');
    return described ? document.getElementById(described) : (tips.find((t) => !t.hidden) ?? null);
  }

  it('creates a role=tooltip pill and links aria-describedby unless it repeats the aria-label', () => {
    const { a, b } = setup();
    expect(a.getAttribute('aria-describedby')).toBeNull();
    const id = b.getAttribute('aria-describedby');
    expect(id).toBeTruthy();
    const tip = document.getElementById(id!)!;
    expect(tip.getAttribute('role')).toBe('tooltip');
    expect(tip.textContent).toBe('Full access');
    expect(tip.hidden).toBe(true);
    expect(a.title).toBe('');
  });

  it('waits on the first hover, then is instant when moving between controls', () => {
    const { a, b } = setup();
    a.dispatchEvent(new MouseEvent('mouseenter'));
    const tips = () => Array.from(document.querySelectorAll<HTMLElement>('.sy-tooltip'));
    expect(tips().every((t) => t.hidden)).toBe(true);
    vi.advanceTimersByTime(TOOLTIP_DELAY_MS + 1);
    const tipA = tips().find((t) => t.textContent === 'Attach file')!;
    expect(tipA.hidden).toBe(false);
    expect(tipA.classList.contains('sy-tooltip-animate')).toBe(true);

    a.dispatchEvent(new MouseEvent('mouseleave'));
    expect(tipA.hidden).toBe(true);
    b.dispatchEvent(new MouseEvent('mouseenter'));
    const tipB = tipFor(b)!;
    expect(tipB.hidden).toBe(false);
  });

  it('hides on Escape and updates its text from the binding', () => {
    const { fixture, b } = setup();
    b.dispatchEvent(new MouseEvent('mouseenter'));
    vi.advanceTimersByTime(TOOLTIP_DELAY_MS + 1);
    expect(tipFor(b)!.hidden).toBe(false);
    key(b, 'Escape');
    expect(tipFor(b)!.hidden).toBe(true);
    fixture.componentInstance.tip.set('Ask for approval');
    fixture.detectChanges();
    TestBed.tick();
    expect(tipFor(b)!.textContent).toBe('Ask for approval');
  });

  it('shows on keyboard focus without the hover delay', () => {
    const { b } = setup();
    b.focus();
    b.dispatchEvent(new FocusEvent('focusin', { bubbles: true }));
    expect(tipFor(b)!.hidden).toBe(false);
    b.dispatchEvent(new FocusEvent('focusout', { bubbles: true }));
    expect(tipFor(b)!.hidden).toBe(true);
  });

  it('skips the animation under prefers-reduced-motion', () => {
    const mm = vi.fn(() => ({ matches: true }) as MediaQueryList);
    vi.stubGlobal('matchMedia', mm);
    const { b } = setup();
    b.dispatchEvent(new MouseEvent('mouseenter'));
    vi.advanceTimersByTime(TOOLTIP_DELAY_MS + 1);
    const tip = tipFor(b)!;
    expect(tip.hidden).toBe(false);
    expect(tip.classList.contains('sy-tooltip-animate')).toBe(false);
    vi.unstubAllGlobals();
  });

  it('removes its pill when destroyed', () => {
    const { fixture } = setup();
    expect(document.querySelectorAll('.sy-tooltip').length).toBe(2);
    fixture.destroy();
    expect(document.querySelectorAll('.sy-tooltip').length).toBe(0);
  });
});

describe('PopoverComponent', () => {
  afterEach(() => TestBed.resetTestingModule());

  async function setup() {
    const fixture = TestBed.createComponent(PopoverHost);
    document.body.appendChild(fixture.nativeElement);
    fixture.detectChanges();
    await fixture.whenStable();
    const el = fixture.nativeElement as HTMLElement;
    const trigger = el.querySelector('#trigger') as HTMLButtonElement;
    const render = async () => {
      fixture.detectChanges();
      await fixture.whenStable();
    };
    return { fixture, el, trigger, render };
  }

  it('wires menu ARIA on the trigger and opens on click', async () => {
    const { el, trigger, render } = await setup();
    expect(trigger.getAttribute('aria-haspopup')).toBe('menu');
    expect(trigger.getAttribute('aria-expanded')).toBe('false');
    trigger.click();
    await render();
    const panel = el.querySelector('[role="menu"]') as HTMLElement;
    expect(panel).toBeTruthy();
    expect(panel.getAttribute('aria-label')).toBe('Approval mode');
    expect(trigger.getAttribute('aria-controls')).toBe(panel.id);
    expect(trigger.getAttribute('aria-expanded')).toBe('true');
    // Focus starts on the checked item.
    expect(document.activeElement?.id).toBe('item-accept');
  });

  it('moves with arrows/Home/End, skipping disabled items, and wraps', async () => {
    const { el, trigger, render } = await setup();
    trigger.focus();
    key(trigger, 'ArrowDown');
    await render();
    const panel = el.querySelector('[role="menu"]') as HTMLElement;
    expect(document.activeElement?.id).toBe('item-accept');
    key(panel, 'ArrowDown');
    expect(document.activeElement?.id).toBe('item-full'); // 'locked' is aria-disabled
    key(panel, 'ArrowDown');
    expect(document.activeElement?.id).toBe('item-ask');
    key(panel, 'ArrowUp');
    expect(document.activeElement?.id).toBe('item-full');
    key(panel, 'Home');
    expect(document.activeElement?.id).toBe('item-ask');
    key(panel, 'End');
    expect(document.activeElement?.id).toBe('item-full');
  });

  it('selects with Enter (native click), closes and returns focus to the trigger', async () => {
    const { fixture, el, trigger, render } = await setup();
    trigger.click();
    await render();
    const full = el.querySelector('#item-full') as HTMLButtonElement;
    full.focus();
    full.click(); // what Enter does on a native button
    await Promise.resolve();
    await render();
    expect(fixture.componentInstance.current()).toBe('full');
    expect(el.querySelector('[role="menu"]')).toBeNull();
    expect(document.activeElement).toBe(trigger);
  });

  it('closes on Escape with focus return, and on an outside pointerdown', async () => {
    const { el, trigger, render } = await setup();
    trigger.click();
    await render();
    key(el.querySelector('[role="menu"]')!, 'Escape');
    await render();
    expect(el.querySelector('[role="menu"]')).toBeNull();
    expect(document.activeElement).toBe(trigger);

    trigger.click();
    await render();
    el.querySelector('#outside')!.dispatchEvent(new Event('pointerdown', { bubbles: true }));
    await render();
    expect(el.querySelector('[role="menu"]')).toBeNull();
    expect(trigger.getAttribute('aria-expanded')).toBe('false');
  });
});

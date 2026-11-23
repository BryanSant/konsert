import { Component, signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClientService } from '../core/services/api-client.service';
import { provideRouter } from '@angular/router';
import { FleetModeComponent } from '../modes/fleet/fleet-mode.component';
import { ModePaletteComponent } from '../modes/mode-palette.component';
import { ModalDialogComponent } from '../shared/modal-dialog.component';

@Component({
  standalone: true,
  imports: [ModalDialogComponent],
  template: `
    <button id="opener" type="button">open</button>
    @if (open()) {
      <app-modal-dialog labelledBy="t" (close)="open.set(false)">
        <h3 id="t">Title</h3>
        <button id="first" type="button">first</button>
        <input id="auto" autofocus />
      </app-modal-dialog>
    }
  `,
})
class HostComponent {
  readonly open = signal(false);
}

describe('accessibility', () => {
  beforeEach(() => {
    localStorage.clear();
    vi.stubGlobal('requestAnimationFrame', (fn: FrameRequestCallback) => {
      fn(0);
      return 0;
    });
    TestBed.configureTestingModule({
      providers: [
        provideRouter([]),
        { provide: ApiClientService, useValue: { setUnauthorizedHandler: vi.fn(), getHeatHistory: vi.fn(async () => { throw new Error('none'); }) } },
      ],
    });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    TestBed.resetTestingModule();
  });

  describe('ModalDialogComponent', () => {
    it('is a labelled modal dialog, focuses [autofocus], closes on Escape and restores focus', async () => {
      const fixture = TestBed.createComponent(HostComponent);
      document.body.appendChild(fixture.nativeElement);
      fixture.detectChanges();
      const opener = fixture.nativeElement.querySelector('#opener') as HTMLButtonElement;
      opener.focus();

      fixture.componentInstance.open.set(true);
      fixture.detectChanges();
      await fixture.whenStable();

      const dialog = fixture.nativeElement.querySelector('dialog') as HTMLDialogElement;
      expect(dialog.getAttribute('aria-modal')).toBe('true');
      expect(dialog.getAttribute('aria-labelledby')).toBe('t');
      expect(dialog.hasAttribute('open')).toBe(true);
      expect(document.activeElement?.id).toBe('auto');

      dialog.dispatchEvent(new Event('cancel', { cancelable: true }));
      fixture.detectChanges();
      await fixture.whenStable();
      expect(fixture.nativeElement.querySelector('dialog')).toBeNull();
      expect(document.activeElement).toBe(opener);
      fixture.nativeElement.remove();
    });
  });

  describe('ModePaletteComponent', () => {
    it('exposes combobox/listbox semantics and tracks the active option', async () => {
      const fixture = TestBed.createComponent(ModePaletteComponent);
      document.body.appendChild(fixture.nativeElement);
      fixture.detectChanges();
      await fixture.whenStable();
      const root = fixture.nativeElement as HTMLElement;
      const input = root.querySelector('input[role="combobox"]') as HTMLInputElement;
      const listbox = root.querySelector('[role="listbox"]') as HTMLElement;
      expect(input.getAttribute('aria-controls')).toBe(listbox.id);
      const options = root.querySelectorAll('[role="option"]');
      expect(options.length).toBeGreaterThan(0);
      expect(input.getAttribute('aria-activedescendant')).toBe(options[0].id);
      expect(options[0].getAttribute('aria-selected')).toBe('true');

      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown' }));
      fixture.detectChanges();
      expect(input.getAttribute('aria-activedescendant')).toBe(options[1].id);

      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'End' }));
      fixture.detectChanges();
      expect(input.getAttribute('aria-activedescendant')).toBe(options[options.length - 1].id);
      fixture.nativeElement.remove();
    });
  });

  describe('Fleet heat range', () => {
    it('is a labelled radiogroup with one checked option', async () => {
      const fixture = TestBed.createComponent(FleetModeComponent);
      fixture.detectChanges();
      await fixture.whenStable();
      const root = fixture.nativeElement as HTMLElement;
      const group = root.querySelector('[role="radiogroup"]') as HTMLElement;
      expect(group.getAttribute('aria-labelledby')).toBeTruthy();
      const radios = Array.from(group.querySelectorAll('[role="radio"]'));
      expect(radios.map((r) => r.textContent?.trim())).toEqual(['12h', '24h', '3d', '1w']);
      expect(radios.filter((r) => r.getAttribute('aria-checked') === 'true').map((r) => r.textContent?.trim())).toEqual(['12h']);
      (radios[2] as HTMLButtonElement).click();
      fixture.detectChanges();
      expect(radios[2].getAttribute('aria-checked')).toBe('true');
    });
  });
});

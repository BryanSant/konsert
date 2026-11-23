import { Component, signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Task } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { FleetStore } from '../core/state/fleet.store';
import { ORCHESTRATOR_TAB_ID, TabsStore } from '../core/state/tabs.store';
import { ModalDialogComponent } from '../shared/modal-dialog.component';
import { CommandPaletteComponent } from './command-palette.component';
import { TabStripComponent } from './tab-strip.component';

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
    // TabsStore restores tabs from localStorage; other specs in this worker may have left some.
    localStorage.clear();
    vi.stubGlobal('requestAnimationFrame', (fn: FrameRequestCallback) => {
      fn(0);
      return 0;
    });
    TestBed.configureTestingModule({
      providers: [{ provide: ApiClientService, useValue: { setUnauthorizedHandler: vi.fn() } }],
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

  describe('CommandPaletteComponent', () => {
    it('exposes combobox/listbox semantics and tracks the active option', async () => {
      const fixture = TestBed.createComponent(CommandPaletteComponent);
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
    });
  });

  describe('TabStripComponent', () => {
    it('is a tablist with roving tabindex and arrow-key navigation', async () => {
      const fleet = TestBed.inject(FleetStore);
      const tabs = TestBed.inject(TabsStore);
      const task = {
        id: 't1',
        worker_id: 'w1',
        agent: 'shell',
        status: 'running',
        created_at: new Date().toISOString(),
      } as Task;
      fleet.upsertTask(task);
      tabs.openTaskTab(task, 'w1', false);

      const fixture = TestBed.createComponent(TabStripComponent);
      fixture.detectChanges();
      await fixture.whenStable();
      const root = fixture.nativeElement as HTMLElement;
      const tabEls = Array.from(root.querySelectorAll<HTMLElement>('[role="tab"]'));
      expect(root.querySelector('[role="tablist"]')).not.toBeNull();
      expect(tabEls.length).toBe(2);
      expect(tabEls[0].getAttribute('aria-selected')).toBe('true');
      expect(tabEls[0].tabIndex).toBe(0);
      expect(tabEls[1].tabIndex).toBe(-1);

      tabEls[0].dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }));
      fixture.detectChanges();
      expect(tabs.activeTabId()).toBe('t1');
      expect(tabEls[1].getAttribute('aria-selected')).toBe('true');

      tabEls[1].dispatchEvent(new KeyboardEvent('keydown', { key: 'Delete', bubbles: true }));
      fixture.detectChanges();
      expect(tabs.openTabs().some((t) => t.id === 't1')).toBe(false);
      expect(tabs.activeTabId()).toBe(ORCHESTRATOR_TAB_ID);
    });
  });
});

import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClientService } from '../../core/services/api-client.service';
import { SyreSocketService } from '../../core/services/syre-socket.service';
import { Task } from '../../core/models/syre.models';
import { FleetStore } from '../../core/state/fleet.store';
import { XtermPaneComponent, clampFontSize } from './xterm-pane.component';

class NoopResizeObserver {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

describe('XtermPaneComponent', () => {
  let subscribePty: ReturnType<typeof vi.fn>;
  let unsubs: Record<string, ReturnType<typeof vi.fn>>;
  let setTaskControl: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.stubGlobal('ResizeObserver', NoopResizeObserver);
    // jsdom lacks matchMedia, which xterm uses for devicePixelRatio tracking.
    vi.stubGlobal(
      'matchMedia',
      vi.fn(() => ({
        matches: false,
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
      })),
    );
    unsubs = {};
    subscribePty = vi.fn((taskId: string) => (unsubs[taskId] = vi.fn()));
    setTaskControl = vi.fn(async () => ({ ok: true }));
    TestBed.configureTestingModule({
      providers: [
        { provide: ApiClientService, useValue: { cancelTask: vi.fn(), setTaskControl } },
        {
          provide: SyreSocketService,
          useValue: { subscribePty, sendPtyResize: vi.fn(), sendPtyStdin: vi.fn() },
        },
      ],
    });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    TestBed.resetTestingModule();
  });

  async function create(taskId: string) {
    const fixture = TestBed.createComponent(XtermPaneComponent);
    fixture.componentRef.setInput('taskId', taskId);
    fixture.detectChanges();
    await fixture.whenStable();
    TestBed.tick();
    return fixture;
  }

  function termOf(cmp: XtermPaneComponent): { options: { fontSize?: number } } | null {
    return (cmp as unknown as { term: { options: { fontSize?: number } } | null }).term;
  }

  it('creates new terminals with the configured font size and follows changes', async () => {
    const fleet = TestBed.inject(FleetStore);
    fleet.settings.update((s) => ({ ...s, terminal_font_size: 18 }));
    const fixture = await create('t1');
    expect(termOf(fixture.componentInstance)?.options.fontSize).toBe(18);

    fleet.settings.update((s) => ({ ...s, terminal_font_size: 11 }));
    TestBed.tick();
    expect(termOf(fixture.componentInstance)?.options.fontSize).toBe(11);
    fixture.destroy();
  });

  it('switches the PTY subscription when the taskId input changes', async () => {
    const fixture = await create('t1');
    expect(subscribePty).toHaveBeenCalledTimes(1);
    expect(subscribePty.mock.calls[0][0]).toBe('t1');

    fixture.componentRef.setInput('taskId', 't2');
    TestBed.tick();
    expect(unsubs['t1']).toHaveBeenCalledTimes(1);
    expect(subscribePty).toHaveBeenCalledTimes(2);
    expect(subscribePty.mock.calls[1][0]).toBe('t2');

    fixture.destroy();
    expect(unsubs['t2']).toHaveBeenCalledTimes(1);
  });

  it('reports Take Control toggles to the hub for the audit log', async () => {
    const fixture = await create('t1');
    const cmp = fixture.componentInstance;
    expect(cmp.interactiveMode()).toBe(false);

    cmp.toggleInteractive();
    expect(setTaskControl).toHaveBeenLastCalledWith('t1', true);
    cmp.toggleInteractive();
    expect(setTaskControl).toHaveBeenLastCalledWith('t1', false);
    expect(setTaskControl).toHaveBeenCalledTimes(2);
    fixture.destroy();
  });

  it('clampFontSize falls back to the default for out-of-range values', () => {
    expect(clampFontSize(16)).toBe(16);
    expect(clampFontSize(4)).toBe(13);
    expect(clampFontSize(undefined)).toBe(13);
  });
});

import { signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Worker, WorkerWakeLock } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { AuthService } from '../core/services/auth.service';
import { KonsertSocketService } from '../core/services/konsert-socket.service';
import { FleetStore } from '../core/state/fleet.store';
import { StatusFooterComponent } from './status-footer.component';

function worker(id: string, wake?: WorkerWakeLock, status = 'online'): Worker {
  return {
    id,
    name: id,
    host_group: id,
    os: 'darwin',
    arch: 'arm64',
    isolation: 'host',
    permission_mode: 'full_access',
    agents: [],
    tags: {},
    max_concurrent: 2,
    status,
    last_seen_at: new Date().toISOString(),
    wake_lock: wake,
  };
}

const supportedOff: WorkerWakeLock = { supported: true, active: false, method: 'caffeinate' };
const awake: WorkerWakeLock = { supported: true, desired: true, active: true, method: 'caffeinate' };

describe('StatusFooterComponent Keep Awake', () => {
  let setFleetWakeLock: ReturnType<typeof vi.fn>;
  let setWorkerWakeLock: ReturnType<typeof vi.fn>;
  let release: ReturnType<typeof vi.fn>;
  let request: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    setFleetWakeLock = vi.fn(async () => ({ ok: true, sent_to: [], status: { fleet: null, workers: [] } }));
    setWorkerWakeLock = vi.fn(async () => ({ ok: true, sent: true }));
    release = vi.fn(async () => undefined);
    request = vi.fn(async () => ({ release }));
    Object.defineProperty(navigator, 'wakeLock', { value: { request }, configurable: true });
    TestBed.configureTestingModule({
      providers: [
        {
          provide: ApiClientService,
          useValue: { setFleetWakeLock, setWorkerWakeLock, cancelTask: vi.fn() },
        },
        { provide: AuthService, useValue: { provider: signal(''), user: signal('tester') } },
        { provide: KonsertSocketService, useValue: { state: signal('connected') } },
      ],
    });
  });

  afterEach(() => {
    TestBed.resetTestingModule();
    delete (navigator as unknown as { wakeLock?: unknown }).wakeLock;
  });

  function setup(workers: Worker[]) {
    const fleet = TestBed.inject(FleetStore);
    for (const w of workers) fleet.upsertWorker(w);
    const fixture = TestBed.createComponent(StatusFooterComponent);
    fixture.detectChanges();
    const el = fixture.nativeElement as HTMLElement;
    const q = <T extends Element>(sel: string) => el.querySelector(sel) as T;
    return { fleet, fixture, el, q, cmp: fixture.componentInstance };
  }

  it('turns on the browser and all hosts, and shows host progress', async () => {
    const { fleet, fixture, q, cmp } = setup([
      worker('alpha', supportedOff),
      worker('beta', supportedOff),
      worker('gamma', undefined, 'offline'),
    ]);
    const toggle = q<HTMLButtonElement>('[data-testid="keep-awake-toggle"]');
    expect(q('[data-testid="keep-awake-label"]').textContent?.trim()).toBe('Off');
    expect(toggle.getAttribute('aria-pressed')).toBe('false');

    toggle.click();
    await vi.waitFor(() => expect(setFleetWakeLock).toHaveBeenCalledWith(true, expect.any(String)));
    expect(request).toHaveBeenCalledWith('screen');
    expect(cmp.keepAwake()).toBe(true);

    // The hub broadcasts the new worker state: one host awake, the other still starting.
    fleet.upsertWorker(worker('alpha', awake));
    fleet.upsertWorker(worker('beta', { ...supportedOff, desired: true }));
    fixture.detectChanges();
    expect(q('[data-testid="keep-awake-label"]').textContent?.trim()).toBe('Awake · 1/2 hosts');
    expect(cmp.wakeIndicator()).toBe('partial');
    expect(toggle.getAttribute('aria-pressed')).toBe('true');
    expect(toggle.getAttribute('aria-label')).toContain('1 of 2 hosts awake');

    fleet.upsertWorker(worker('beta', awake));
    fixture.detectChanges();
    expect(q('[data-testid="keep-awake-label"]').textContent?.trim()).toBe('Awake · 2/2 hosts');
    expect(cmp.wakeIndicator()).toBe('on');

    // Toggling again turns everything off.
    toggle.click();
    await vi.waitFor(() => expect(setFleetWakeLock).toHaveBeenLastCalledWith(false, expect.any(String)));
    expect(release).toHaveBeenCalled();
    expect(cmp.keepAwake()).toBe(false);
  });

  it('reflects hosts kept awake by the hub even when this browser is off', () => {
    const { q, cmp } = setup([worker('alpha', awake), worker('beta', supportedOff)]);
    expect(cmp.keepAwake()).toBe(false);
    expect(q('[data-testid="keep-awake-label"]').textContent?.trim()).toBe('Hosts 1/2');
    expect(cmp.anyAwake()).toBe(true);
  });

  it('lists hosts in the popover with per-host switches and errors', async () => {
    const { fixture, q, el } = setup([
      worker('alpha', supportedOff),
      worker('beta', {
        supported: true,
        desired: true,
        active: false,
        method: 'systemd-inhibit',
        error: 'systemd-inhibit exited: Failed to connect to system scope bus',
      }),
      worker('legacy'),
    ]);
    const details = q<HTMLButtonElement>('[data-testid="keep-awake-details"]');
    expect(details.getAttribute('aria-expanded')).toBe('false');
    details.click();
    fixture.detectChanges();
    expect(details.getAttribute('aria-expanded')).toBe('true');
    expect(q('[data-testid="keep-awake-popover"]')).toBeTruthy();

    const row = (id: string) => q<HTMLElement>(`[data-testid="host-wake-${id}"]`);
    const sw = (id: string) => row(id).querySelector('input[role="switch"]') as HTMLInputElement;
    expect(sw('alpha').checked).toBe(false);
    expect(sw('alpha').getAttribute('aria-label')).toBe('Keep alpha awake');
    expect(row('beta').textContent).toContain('Failed to connect to system scope bus');
    expect(sw('beta').checked).toBe(true);
    expect(sw('legacy').disabled).toBe(true);
    expect(row('legacy').textContent).toContain('Not supported');

    sw('alpha').click();
    await vi.waitFor(() => expect(setWorkerWakeLock).toHaveBeenCalledWith('alpha', true, expect.any(String)));
    sw('beta').click();
    await vi.waitFor(() => expect(setWorkerWakeLock).toHaveBeenCalledWith('beta', false, expect.any(String)));
    expect(setFleetWakeLock).not.toHaveBeenCalled();

    // Escape closes the popover.
    el.querySelector('footer')!.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    fixture.detectChanges();
    expect(q('[data-testid="keep-awake-popover"]')).toBeNull();
  });

  it('shows an error when the hub request fails but keeps the browser lock', async () => {
    setFleetWakeLock.mockRejectedValueOnce(new Error('hub unreachable'));
    const { fixture, q, cmp } = setup([worker('alpha', supportedOff)]);
    await cmp.toggleWakeLock();
    expect(cmp.keepAwake()).toBe(true);
    expect(cmp.wakeError()).toContain('hub unreachable');
    cmp.togglePopover('wake');
    fixture.detectChanges();
    expect(q('[role="alert"]').textContent).toContain('hub unreachable');
  });
});

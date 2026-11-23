import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SetupStatus } from '../models/konsert.models';
import { ApiClientService } from '../services/api-client.service';
import { SetupStore } from './setup.store';

const firstRun: SetupStatus = {
  needs_setup: true,
  local: { hostname: 'studio', konsertd_found: true, worker_id: 'w-1', connected: false, added: false },
};

describe('SetupStore (first-run Add Machine)', () => {
  let getSetup: ReturnType<typeof vi.fn>;
  let dismissSetup: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    getSetup = vi.fn(async () => firstRun);
    dismissSetup = vi.fn(async () => ({ ok: true }));
    TestBed.configureTestingModule({
      providers: [{ provide: ApiClientService, useValue: { getSetup, dismissSetup } }],
    });
  });

  afterEach(() => TestBed.resetTestingModule());

  it('reports first run from the hub, so the shell opens Add Machine', async () => {
    const store = TestBed.inject(SetupStore);
    const s = await store.load();
    expect(s?.needs_setup).toBe(true);
    expect(store.needsSetup()).toBe(true);
    expect(store.local()?.hostname).toBe('studio');
  });

  it('dismisses once on close and does not ask the hub again', async () => {
    const store = TestBed.inject(SetupStore);
    await store.load();
    await store.dismiss();
    await store.dismiss();
    expect(dismissSetup).toHaveBeenCalledTimes(1);
    expect(store.needsSetup()).toBe(false);
  });

  it('does not dismiss when setup is already complete (e.g. a machine was added)', async () => {
    getSetup.mockResolvedValueOnce({ ...firstRun, needs_setup: false });
    const store = TestBed.inject(SetupStore);
    await store.load();
    await store.dismiss();
    expect(dismissSetup).not.toHaveBeenCalled();

    const store2 = TestBed.inject(SetupStore);
    await store2.load();
    store2.markCompleted();
    await store2.dismiss();
    expect(dismissSetup).not.toHaveBeenCalled();
  });

  it('keeps working when the hub cannot be asked', async () => {
    getSetup.mockRejectedValueOnce(new Error('offline'));
    const store = TestBed.inject(SetupStore);
    expect(await store.load()).toBeNull();
    expect(store.needsSetup()).toBe(false);
    expect(store.error()).toBe('offline');
  });
});

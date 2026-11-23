import { DestroyRef } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { SwUpdate, VersionEvent } from '@angular/service-worker';
import { Subject } from 'rxjs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AppUpdateService } from './app-update.service';

describe('AppUpdateService', () => {
  let versionUpdates: Subject<VersionEvent>;
  let unrecoverable: Subject<unknown>;
  let sw: {
    isEnabled: boolean;
    versionUpdates: Subject<VersionEvent>;
    unrecoverable: Subject<unknown>;
    checkForUpdate: ReturnType<typeof vi.fn>;
    activateUpdate: ReturnType<typeof vi.fn>;
  };
  const destroyRef = { onDestroy: () => undefined } as unknown as DestroyRef;

  beforeEach(() => {
    versionUpdates = new Subject();
    unrecoverable = new Subject();
    sw = {
      isEnabled: true,
      versionUpdates,
      unrecoverable,
      checkForUpdate: vi.fn(async () => false),
      activateUpdate: vi.fn(async () => true),
    };
    TestBed.configureTestingModule({ providers: [{ provide: SwUpdate, useValue: sw }] });
  });

  afterEach(() => TestBed.resetTestingModule());

  it('prompts once a new version is ready', () => {
    const svc = TestBed.inject(AppUpdateService);
    svc.start(destroyRef);
    expect(sw.checkForUpdate).toHaveBeenCalled();
    expect(svc.updateReady()).toBe(false);

    versionUpdates.next({ type: 'VERSION_DETECTED', version: { hash: 'b' } } as VersionEvent);
    expect(svc.updateReady()).toBe(false);

    versionUpdates.next({
      type: 'VERSION_READY',
      currentVersion: { hash: 'a' },
      latestVersion: { hash: 'b' },
    } as VersionEvent);
    expect(svc.updateReady()).toBe(true);
  });

  it('flags an unrecoverable state', () => {
    const svc = TestBed.inject(AppUpdateService);
    svc.start(destroyRef);
    unrecoverable.next({ type: 'UNRECOVERABLE_STATE', reason: 'gone' });
    expect(svc.unrecoverable()).toBe(true);
  });

  it('is inert when the service worker is disabled', () => {
    sw.isEnabled = false;
    const svc = TestBed.inject(AppUpdateService);
    svc.start(destroyRef);
    expect(sw.checkForUpdate).not.toHaveBeenCalled();
  });
});

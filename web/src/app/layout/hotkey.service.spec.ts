import { TestBed } from '@angular/core/testing';
import { DestroyRef } from '@angular/core';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { HotkeyService, resolveHotkey } from './hotkey.service';

function key(init: KeyboardEventInit): KeyboardEvent {
  return new KeyboardEvent('keydown', { cancelable: true, ...init });
}

describe('resolveHotkey', () => {
  it('maps palette and tab shortcuts', () => {
    expect(resolveHotkey(key({ key: 'k', metaKey: true }))).toEqual({ kind: 'palette' });
    expect(resolveHotkey(key({ key: 'K', ctrlKey: true }))).toEqual({ kind: 'palette' });
    expect(resolveHotkey(key({ key: 'P', metaKey: true, shiftKey: true }))).toEqual({
      kind: 'palette',
    });
    expect(resolveHotkey(key({ key: 'j', ctrlKey: true }))).toBeNull();
    expect(resolveHotkey(key({ key: '3', metaKey: true }))).toEqual({
      kind: 'select-tab',
      index: 2,
    });
    expect(resolveHotkey(key({ key: 'w', metaKey: true }))).toEqual({ kind: 'close-tab' });
    expect(resolveHotkey(key({ key: '\\', metaKey: true }))).toBeNull();
    expect(resolveHotkey(key({ key: 'Escape' }))).toEqual({ kind: 'escape' });
  });

  it('leaves Ctrl+W, Ctrl+\\ and Ctrl+digits to the terminal', () => {
    expect(resolveHotkey(key({ key: 'w', ctrlKey: true }))).toBeNull();
    expect(resolveHotkey(key({ key: '\\', ctrlKey: true }))).toBeNull();
    expect(resolveHotkey(key({ key: '1', ctrlKey: true }))).toBeNull();
    expect(resolveHotkey(key({ key: 'a' }))).toBeNull();
  });
});

describe('HotkeyService', () => {
  afterEach(() => TestBed.resetTestingModule());

  it('dispatches to registered handlers, prevents default when handled, and unregisters', () => {
    TestBed.configureTestingModule({});
    const svc = TestBed.inject(HotkeyService);
    const callbacks: Array<() => void> = [];
    const destroyRef = {
      onDestroy: (fn: () => void) => callbacks.push(fn),
    } as unknown as DestroyRef;
    const handler = vi.fn(() => true);
    svc.register(handler, destroyRef);

    const ev = key({ key: 'k', metaKey: true });
    window.dispatchEvent(ev);
    expect(handler).toHaveBeenCalledWith({ kind: 'palette' }, ev);
    expect(ev.defaultPrevented).toBe(true);

    const plain = key({ key: 'x' });
    window.dispatchEvent(plain);
    expect(handler).toHaveBeenCalledTimes(1);

    callbacks.forEach((fn) => fn());
    window.dispatchEvent(key({ key: 'k', metaKey: true }));
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it('does not prevent default when the handler declines', () => {
    TestBed.configureTestingModule({});
    const svc = TestBed.inject(HotkeyService);
    const destroyRef = { onDestroy: () => undefined } as unknown as DestroyRef;
    svc.register(() => false, destroyRef);
    const ev = key({ key: 'Escape' });
    window.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(false);
  });
});

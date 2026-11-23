import { DOCUMENT } from '@angular/common';
import { DestroyRef, Injectable, inject } from '@angular/core';

/** Global shortcuts understood by the app shell. */
export type HotkeyAction =
  | { kind: 'palette' }
  | { kind: 'scratch' }
  | { kind: 'select-tab'; index: number }
  | { kind: 'close-tab' }
  | { kind: 'toggle-split' }
  | { kind: 'escape' };

/**
 * Maps a keydown to a shell shortcut, or null.
 *
 * ⌘/Ctrl+K and ⌘/Ctrl+Shift+P open the palette, ⌘/Ctrl+J toggles scratch. Tab
 * switching (⌘1-9), ⌘W and ⌘\ require the Meta key alone (never Ctrl) so terminal
 * Ctrl+W / Ctrl+\ keep reaching the PTY.
 */
export function resolveHotkey(ev: KeyboardEvent): HotkeyAction | null {
  const key = ev.key.toLowerCase();
  const meta = ev.metaKey || ev.ctrlKey;
  if (meta && (key === 'k' || (ev.shiftKey && key === 'p'))) return { kind: 'palette' };
  if (meta && key === 'j') return { kind: 'scratch' };
  if (ev.metaKey && !ev.ctrlKey && !ev.altKey) {
    if (ev.key >= '1' && ev.key <= '9') return { kind: 'select-tab', index: Number(ev.key) - 1 };
    if (key === 'w') return { kind: 'close-tab' };
    if (ev.key === '\\') return { kind: 'toggle-split' };
  }
  if (ev.key === 'Escape') return { kind: 'escape' };
  return null;
}

/**
 * Handler for a resolved shortcut. Return true when the shortcut was handled, in
 * which case the browser default is prevented.
 */
export type HotkeyHandler = (action: HotkeyAction, ev: KeyboardEvent) => boolean;

/** Owns the single window keydown listener for global shortcuts. */
@Injectable({ providedIn: 'root' })
export class HotkeyService {
  private readonly document = inject(DOCUMENT);
  private readonly handlers = new Set<HotkeyHandler>();
  private listening = false;

  private readonly onKeyDown = (ev: KeyboardEvent): void => {
    const action = resolveHotkey(ev);
    if (!action) return;
    for (const handler of this.handlers) {
      if (handler(action, ev)) {
        ev.preventDefault();
        return;
      }
    }
  };

  /**
   * Registers a handler for the lifetime of the caller's injection context (or until
   * the returned function is called).
   */
  register(handler: HotkeyHandler, destroyRef: DestroyRef = inject(DestroyRef)): () => void {
    this.handlers.add(handler);
    this.ensureListening();
    const unregister = () => {
      this.handlers.delete(handler);
      if (this.handlers.size === 0) this.stopListening();
    };
    destroyRef.onDestroy(unregister);
    return unregister;
  }

  private ensureListening(): void {
    if (this.listening) return;
    this.document.defaultView?.addEventListener('keydown', this.onKeyDown);
    this.listening = true;
  }

  private stopListening(): void {
    if (!this.listening) return;
    this.document.defaultView?.removeEventListener('keydown', this.onKeyDown);
    this.listening = false;
  }
}

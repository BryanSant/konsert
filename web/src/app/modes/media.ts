import { Signal } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { Observable } from 'rxjs';
import { PHONE_QUERY } from '../app.routes';

function matchesNow(q: string): boolean {
  return typeof window !== 'undefined' && window.matchMedia?.(q).matches === true;
}

/** A media query as an observable of its match state. */
export function mediaQuery(q: string): Observable<boolean> {
  return new Observable<boolean>((sub) => {
    if (typeof window === 'undefined' || !window.matchMedia) {
      sub.next(false);
      return;
    }
    const mql = window.matchMedia(q);
    const on = () => sub.next(mql.matches);
    on();
    mql.addEventListener?.('change', on);
    return () => mql.removeEventListener?.('change', on);
  });
}

/** Whether the viewport is phone-sized (call in an injection context). */
export function phoneSignal(): Signal<boolean> {
  return toSignal(mediaQuery(PHONE_QUERY), { initialValue: matchesNow(PHONE_QUERY) });
}

/** Whether the user asked for reduced motion (call in an injection context). */
export function reducedMotionSignal(): Signal<boolean> {
  const q = '(prefers-reduced-motion: reduce)';
  return toSignal(mediaQuery(q), { initialValue: matchesNow(q) });
}

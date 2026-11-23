import { Injectable } from '@angular/core';
import { AuditEvent } from '../models/konsert.models';

type AuditListener = (ev: AuditEvent) => void;

/**
 * Fan-out point for live audit entries pushed by the hub as fleet.update frames of
 * kind "audit". Nothing is retained: an open Audit view subscribes and merges new
 * entries into the page it already fetched, so a closed view costs nothing.
 */
@Injectable({ providedIn: 'root' })
export class AuditStore {
  private readonly listeners = new Set<AuditListener>();

  /** Registers fn for live entries; returns the unsubscribe function. */
  subscribe(fn: AuditListener): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  publish(ev: AuditEvent): void {
    for (const fn of this.listeners) {
      try {
        fn(ev);
      } catch {
        // A faulty view must not break socket dispatch.
      }
    }
  }
}

import { Injectable, computed, inject, signal } from '@angular/core';
import { SetupStatus } from '../models/syre.models';
import { ApiClientService } from '../services/api-client.service';

/**
 * First-run state, persisted by the hub (GET /api/setup): a hub with no machines whose
 * Add Machine dialog was never completed or dismissed opens it on load. Closing the
 * dialog dismisses setup for good; adding any machine completes it on the hub.
 */
@Injectable({ providedIn: 'root' })
export class SetupStore {
  private readonly api = inject(ApiClientService);

  readonly status = signal<SetupStatus | null>(null);
  readonly loading = signal(false);
  readonly error = signal('');
  readonly needsSetup = computed(() => !!this.status()?.needs_setup);
  readonly local = computed(() => this.status()?.local ?? null);

  async load(): Promise<SetupStatus | null> {
    this.loading.set(true);
    try {
      const s = await this.api.getSetup();
      this.status.set(s);
      this.error.set('');
      return s;
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not check this machine');
      return null;
    } finally {
      this.loading.set(false);
    }
  }

  /** The hub recorded setup as complete (a machine was added). */
  markCompleted(): void {
    this.status.update((s) => (s ? { ...s, needs_setup: false } : s));
  }

  /** Closing the first-run dialog: remember it so it does not reopen on every load. */
  async dismiss(): Promise<void> {
    if (!this.needsSetup()) return;
    this.markCompleted();
    try {
      await this.api.dismissSetup();
    } catch {
      // Best effort: the next load asks again.
    }
  }
}

import { DestroyRef, Injectable, inject, signal } from '@angular/core';
import { SwUpdate, VersionReadyEvent } from '@angular/service-worker';

/** How often to ask the service worker to look for a new build. */
const CHECK_INTERVAL_MS = 30 * 60 * 1000;

/**
 * Watches the service worker for a newer UI build (e.g. after the hub binary was
 * upgraded) so the app can prompt for a reload instead of running a stale bundle
 * against a newer API.
 */
@Injectable({ providedIn: 'root' })
export class AppUpdateService {
  // Optional: absent in unit tests and when the service worker is not provided.
  private readonly swUpdate = inject(SwUpdate, { optional: true });

  /** True once a new version has been downloaded and is ready to activate. */
  readonly updateReady = signal(false);
  /** True when the cached app is broken and must be reloaded. */
  readonly unrecoverable = signal(false);

  private started = false;

  start(destroyRef: DestroyRef): void {
    const sw = this.swUpdate;
    if (this.started || !sw?.isEnabled) return;
    this.started = true;
    const sub = sw.versionUpdates.subscribe((ev) => {
      if (ev.type === 'VERSION_READY') {
        this.onVersionReady(ev);
      }
    });
    const unrecSub = sw.unrecoverable.subscribe(() => this.unrecoverable.set(true));
    const timer = setInterval(() => void this.check(), CHECK_INTERVAL_MS);
    const onVisible = () => {
      if (document.visibilityState === 'visible') void this.check();
    };
    document.addEventListener('visibilitychange', onVisible);
    destroyRef.onDestroy(() => {
      sub.unsubscribe();
      unrecSub.unsubscribe();
      clearInterval(timer);
      document.removeEventListener('visibilitychange', onVisible);
    });
    void this.check();
  }

  private onVersionReady(_ev: VersionReadyEvent): void {
    this.updateReady.set(true);
  }

  private async check(): Promise<void> {
    try {
      await this.swUpdate?.checkForUpdate();
    } catch {
      // Offline or SW not ready: try again next interval.
    }
  }

  /** Activates the downloaded version and reloads into it. */
  async reload(): Promise<void> {
    try {
      if (this.updateReady()) await this.swUpdate?.activateUpdate();
    } finally {
      document.location.reload();
    }
  }

  dismiss(): void {
    this.updateReady.set(false);
  }
}

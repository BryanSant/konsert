import { Injectable, computed, inject, signal } from '@angular/core';
import { SwPush } from '@angular/service-worker';
import { PushPrefs } from '../models/syre.models';
import { ApiClientService } from './api-client.service';

/**
 * Web Push on this device (SYR-Z15J6): subscribe through Angular's service worker after
 * the user asks for it, keep the hub's copy of the subscription and prefs in step, and
 * send a test notification. A notification click opens the session (the hub's payload
 * carries the service worker's navigateLastFocusedOrOpen action).
 */
@Injectable({ providedIn: 'root' })
export class PushService {
  private readonly api = inject(ApiClientService);
  private readonly sw = inject(SwPush);

  readonly loaded = signal(false);
  readonly subscribed = signal(false);
  readonly prefs = signal<PushPrefs>({});
  readonly busy = signal(false);
  readonly error = signal('');
  readonly testResult = signal('');
  private publicKey = '';

  /** Push works here: a service worker and the Push API (and on iOS, the installed app). */
  readonly supported = computed(
    () => this.sw.isEnabled && typeof window !== 'undefined' && 'PushManager' in window,
  );
  /** iOS only allows push for an app on the Home Screen. */
  readonly needsInstall = computed(() => isIOS() && !isStandalone());
  readonly permission = signal<NotificationPermission | 'unsupported'>(
    typeof Notification === 'undefined' ? 'unsupported' : Notification.permission,
  );

  async load(): Promise<void> {
    try {
      const cfg = await this.api.getPushConfig();
      this.publicKey = cfg.public_key;
      this.prefs.set(cfg.prefs ?? {});
      const current = await this.currentEndpoint();
      this.subscribed.set(!!current && cfg.endpoints.includes(current));
      this.error.set('');
    } catch (e) {
      this.error.set(errText(e));
    } finally {
      this.loaded.set(true);
    }
  }

  /** Turns notifications on for this device: the browser asks for permission here. */
  async enable(): Promise<void> {
    if (!this.supported()) return;
    this.busy.set(true);
    this.error.set('');
    try {
      if (!this.publicKey) await this.load();
      const sub = await this.sw.requestSubscription({ serverPublicKey: this.publicKey });
      this.permission.set(Notification.permission);
      await this.api.addPushSubscription(sub.toJSON(), this.prefs());
      this.subscribed.set(true);
    } catch (e) {
      this.permission.set(
        typeof Notification === 'undefined' ? 'unsupported' : Notification.permission,
      );
      this.error.set(
        this.permission() === 'denied'
          ? 'Notifications are blocked for this site in the browser settings.'
          : errText(e),
      );
    } finally {
      this.busy.set(false);
    }
  }

  async disable(): Promise<void> {
    this.busy.set(true);
    try {
      const endpoint = await this.currentEndpoint();
      if (endpoint) await this.api.removePushSubscription(endpoint);
      await this.sw.unsubscribe().catch(() => undefined);
      this.subscribed.set(false);
    } catch (e) {
      this.error.set(errText(e));
    } finally {
      this.busy.set(false);
    }
  }

  async savePrefs(prefs: PushPrefs): Promise<void> {
    this.prefs.set(prefs);
    try {
      await this.api.setPushPrefs(prefs);
      this.error.set('');
    } catch (e) {
      this.error.set(errText(e));
    }
  }

  async test(): Promise<void> {
    this.testResult.set('');
    try {
      const r = await this.api.sendTestPush();
      this.testResult.set(
        r.sent > 0 ? 'Sent: it should appear in a moment.' : 'The push service did not accept it.',
      );
    } catch (e) {
      this.testResult.set(errText(e));
    }
  }

  private async currentEndpoint(): Promise<string> {
    if (!this.supported()) return '';
    return new Promise((resolve) => {
      const s = this.sw.subscription.subscribe((sub) => {
        resolve(sub?.endpoint ?? '');
        queueMicrotask(() => s.unsubscribe());
      });
      setTimeout(() => resolve(''), 2000);
    });
  }
}

function errText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

export function isIOS(
  ua: string = typeof navigator === 'undefined' ? '' : navigator.userAgent,
): boolean {
  return /iPhone|iPad|iPod/.test(ua);
}

function isStandalone(): boolean {
  if (typeof window === 'undefined') return false;
  return (
    window.matchMedia?.('(display-mode: standalone)').matches ||
    (navigator as Navigator & { standalone?: boolean }).standalone === true
  );
}

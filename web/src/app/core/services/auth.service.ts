import { Injectable, inject, signal } from '@angular/core';
import { Device } from '../models/device.models';
import { ApiClientService } from './api-client.service';

export const SESSION_EXPIRED_MESSAGE = 'This device is signed out (revoked or unused for too long). Sign in again with a new link.';

@Injectable({ providedIn: 'root' })
export class AuthService {
  private readonly api = inject(ApiClientService);

  readonly loading = signal(true);
  readonly authenticated = signal(false);
  readonly authRequired = signal(false);
  readonly user = signal('');
  /** This browser's device credential (null for the recovery key, Tailscale or --no-auth). */
  readonly device = signal<Device | null>(null);
  readonly provider = signal('openai');
  readonly model = signal('');
  readonly error = signal('');

  constructor() {
    // Any 401 from the hub (outside of login itself) means the cookie expired or the
    // secret rotated: drop back to the login screen. Leaving `authenticated` tears
    // down the app shell, which disconnects the WebSocket.
    this.api.setUnauthorizedHandler(() => this.handleSessionExpired());
  }

  async checkAuth(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const me = await this.api.getMe();
      this.authenticated.set(me.authenticated);
      this.authRequired.set(me.auth_required);
      this.user.set(me.user);
      this.device.set(me.device ?? null);
      this.provider.set(me.provider || 'openai');
      this.model.set(me.model || '');
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Unable to reach Konsert controller');
      this.authenticated.set(false);
    } finally {
      this.loading.set(false);
    }
  }

  /**
   * Re-validates the session without toggling the loading screen (used after repeated
   * WebSocket handshake failures, which carry no status code). Returns false when the
   * hub says the session is no longer valid. Network errors are treated as "unknown"
   * and leave the session alone.
   */
  async verifySession(): Promise<boolean> {
    if (!this.authenticated()) return false;
    try {
      const me = await this.api.getMe();
      if (!me.authenticated) {
        this.handleSessionExpired();
        return false;
      }
      return true;
    } catch {
      return true;
    }
  }

  handleSessionExpired(): void {
    if (!this.authenticated()) return;
    this.authenticated.set(false);
    this.user.set('');
    this.device.set(null);
    this.error.set(SESSION_EXPIRED_MESSAGE);
  }

  /** The "Use recovery key" fallback: signs this browser in as a new device. */
  async login(token: string, name = ''): Promise<boolean> {
    this.error.set('');
    try {
      const res = await this.api.login(token, name);
      this.authenticated.set(res.authenticated);
      this.user.set(res.user);
      this.device.set(res.device ?? null);
      return res.authenticated;
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Authentication failed');
      return false;
    }
  }

  /** Redeems a pairing code: this browser becomes a signed-in device. */
  async pair(code: string, name: string): Promise<boolean> {
    this.error.set('');
    try {
      const res = await this.api.pair(code, name);
      this.authenticated.set(res.authenticated);
      this.user.set(res.user);
      this.device.set(res.device ?? null);
      return res.authenticated;
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Sign-in failed');
      return false;
    }
  }

  async logout(): Promise<void> {
    try {
      await this.api.logout();
    } finally {
      this.authenticated.set(false);
      this.user.set('');
    }
  }
}

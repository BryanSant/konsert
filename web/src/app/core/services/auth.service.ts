import { Injectable, inject, signal } from '@angular/core';
import { ApiClientService } from './api-client.service';

@Injectable({ providedIn: 'root' })
export class AuthService {
  private readonly api = inject(ApiClientService);

  readonly loading = signal(true);
  readonly authenticated = signal(false);
  readonly authRequired = signal(false);
  readonly user = signal('');
  readonly provider = signal('openai');
  readonly model = signal('');
  readonly error = signal('');

  async checkAuth(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const me = await this.api.getMe();
      this.authenticated.set(me.authenticated);
      this.authRequired.set(me.auth_required);
      this.user.set(me.user);
      this.provider.set(me.provider || 'openai');
      this.model.set(me.model || '');
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Unable to reach Konsert controller');
      this.authenticated.set(false);
    } finally {
      this.loading.set(false);
    }
  }

  async login(token: string): Promise<boolean> {
    this.error.set('');
    try {
      const res = await this.api.login(token);
      this.authenticated.set(res.authenticated);
      this.user.set(res.user);
      return res.authenticated;
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Authentication failed');
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

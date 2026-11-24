import { Injectable, computed, inject, signal } from '@angular/core';
import { Device } from '../models/device.models';
import { ApiClientService } from '../services/api-client.service';

/** Newest use first, like the hub's list. */
function byLastUsed(a: Device, b: Device): number {
  return b.last_used_at.localeCompare(a.last_used_at) || b.id.localeCompare(a.id);
}

/**
 * Settings → Devices: the signed-in devices, kept live by fleet.update frames of kind
 * "device" (paired, renamed, created) and "device_revoked".
 */
@Injectable({ providedIn: 'root' })
export class DevicesStore {
  private readonly api = inject(ApiClientService);

  private readonly byId = signal<Record<string, Device>>({});
  readonly currentId = signal('');
  readonly idleDays = signal(90);
  readonly authRequired = signal(true);
  readonly loaded = signal(false);
  readonly error = signal('');
  /** The most recent device that appeared live (Add a device watches this). */
  readonly lastAdded = signal<Device | null>(null);

  readonly devices = computed(() =>
    Object.values(this.byId())
      .map((d) => ({ ...d, current: d.id === this.currentId() }))
      .sort(byLastUsed),
  );

  async load(): Promise<void> {
    try {
      const res = await this.api.listDevices();
      const map: Record<string, Device> = {};
      for (const d of res.devices) map[d.id] = d;
      this.byId.set(map);
      this.currentId.set(res.current_device_id || '');
      this.idleDays.set(res.idle_days || 90);
      this.authRequired.set(res.auth_required !== false);
      this.error.set('');
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Failed to load devices');
    } finally {
      this.loaded.set(true);
    }
  }

  apply(d: Device): void {
    const isNew = !this.byId()[d.id];
    this.byId.update((m) => ({ ...m, [d.id]: { ...m[d.id], ...d } }));
    if (isNew) this.lastAdded.set(d);
  }

  remove(id: string): void {
    this.byId.update((m) => {
      if (!m[id]) return m;
      const next = { ...m };
      delete next[id];
      return next;
    });
  }

  async rename(id: string, name: string): Promise<void> {
    const d = await this.api.renameDevice(id, name);
    this.apply(d);
  }

  async revoke(id: string): Promise<void> {
    await this.api.revokeDevice(id);
    this.remove(id);
  }
}

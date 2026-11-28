import { TestBed } from '@angular/core/testing';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Device, DevicesResponse } from '../../core/models/device.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { DevicesStore } from '../../core/state/devices.store';
import {
  codeFromPath,
  defaultDeviceName,
  formatCountdown,
  isLauncherLink,
  normalizeCode,
} from './device-name';
import { ago, claudeMcpCommand, expiresIn, mcpJsonSnippet, whereFrom } from './devices-logic';

const dev = (over: Partial<Device>): Device => ({
  id: 'id-' + (over.sid ?? 'x'),
  sid: 'dv-aaaaa',
  name: 'Laptop',
  kind: 'browser',
  created_at: '2026-11-01T00:00:00Z',
  last_used_at: '2026-11-20T00:00:00Z',
  expires_at: '2027-02-18T00:00:00Z',
  ...over,
});

describe('pairing codes and device names', () => {
  it('normalizes typed codes the way the hub does', () => {
    expect(normalizeCode('7kq2-m9tx')).toBe('7KQ2-M9TX');
    expect(normalizeCode(' 7KQ2 M9TX ')).toBe('7KQ2-M9TX');
    expect(normalizeCode('oIl1oooo')).toBe('0111-0000');
    for (const bad of ['', '7KQ2-M9T', '7KQ2-M9TXX', '7KQ2-M9TU', 'hello'])
      expect(normalizeCode(bad)).toBe('');
  });

  it('reads codes from sign-in link paths', () => {
    expect(codeFromPath('/pair/7KQ2-M9TX', 'pair')).toBe('7KQ2-M9TX');
    expect(codeFromPath('/pair/7kq2m9tx/', 'pair')).toBe('7KQ2-M9TX');
    expect(codeFromPath('/approve/7KQ2-M9TX', 'pair')).toBe('');
    expect(codeFromPath('/pair/nope', 'pair')).toBe('');
    expect(codeFromPath('/command', 'pair')).toBe('');
  });

  it('signs in without asking only for a launcher link on a loopback hub', () => {
    expect(isLauncherLink('127.0.0.1', '#launch')).toBe(true);
    expect(isLauncherLink('localhost', '#launch')).toBe(true);
    expect(isLauncherLink('[::1]', '#launch')).toBe(true);
    expect(isLauncherLink('127.0.0.1', '')).toBe(false);
    expect(isLauncherLink('192.168.1.5', '#launch')).toBe(false);
    expect(isLauncherLink('hub.example.ts.net', '#launch')).toBe(false);
  });

  it('names this device from its user agent', () => {
    expect(
      defaultDeviceName(
        'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) Version/18.0 Mobile/15E148 Safari/604.1',
      ),
    ).toBe('Safari on iPhone');
    expect(
      defaultDeviceName(
        'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/131.0 Safari/537.36',
      ),
    ).toBe('Chrome on Mac');
    expect(defaultDeviceName('')).toBe('Browser');
  });

  it('formats countdowns and ages', () => {
    expect(formatCountdown(600_000)).toBe('10:00');
    expect(formatCountdown(59_001)).toBe('1:00');
    expect(formatCountdown(-5)).toBe('0:00');
    const now = Date.parse('2026-11-23T12:00:00Z');
    expect(ago('2026-11-23T11:59:30Z', now)).toBe('just now');
    expect(ago('2026-11-23T11:55:00Z', now)).toBe('5 min ago');
    expect(ago('2026-11-22T11:00:00Z', now)).toBe('yesterday');
    expect(expiresIn('2027-02-21T12:00:00Z', now)).toBe('in 90 days');
    expect(whereFrom({ last_user_agent: 'Safari on iPhone', last_ip: '10.0.0.2' })).toBe(
      'Safari on iPhone · 10.0.0.2',
    );
  });

  it('builds MCP client config from a fresh credential', () => {
    expect(claudeMcpCommand('http://desk:8420/mcp', 'sdv_x')).toBe(
      'claude mcp add --transport http syre http://desk:8420/mcp --header "Authorization: Bearer sdv_x"',
    );
    const cfg = JSON.parse(mcpJsonSnippet('http://desk:8420/mcp', 'sdv_x'));
    expect(cfg.mcpServers.syre).toEqual({
      type: 'http',
      url: 'http://desk:8420/mcp',
      headers: { Authorization: 'Bearer sdv_x' },
    });
  });
});

describe('DevicesStore', () => {
  afterEach(() => TestBed.resetTestingModule());

  function setup(res: DevicesResponse) {
    const api = {
      listDevices: vi.fn(async () => res),
      revokeDevice: vi.fn(async () => ({ ok: true })),
      renameDevice: vi.fn(),
    };
    TestBed.configureTestingModule({ providers: [{ provide: ApiClientService, useValue: api }] });
    return { store: TestBed.inject(DevicesStore), api };
  }

  it('lists devices newest first and marks this one', async () => {
    const { store } = setup({
      devices: [
        dev({ sid: 'dv-old', id: 'a', last_used_at: '2026-11-01T00:00:00Z' }),
        dev({ sid: 'dv-new', id: 'b', last_used_at: '2026-11-22T00:00:00Z' }),
      ],
      current_device_id: 'a',
      idle_days: 30,
      auth_required: true,
    });
    await store.load();
    expect(store.devices().map((d) => d.sid)).toEqual(['dv-new', 'dv-old']);
    expect(store.devices().find((d) => d.id === 'a')?.current).toBe(true);
    expect(store.idleDays()).toBe(30);
  });

  it('shows a newly paired device live and drops a revoked one', async () => {
    const { store, api } = setup({
      devices: [dev({ id: 'a' })],
      current_device_id: 'a',
      idle_days: 90,
      auth_required: true,
    });
    await store.load();
    const phone = dev({
      id: 'p',
      sid: 'dv-phone',
      name: 'Pixel',
      kind: 'phone',
      created_by: 'pairing code pc-12345',
    });
    store.apply(phone);
    expect(store.lastAdded()?.id).toBe('p');
    expect(store.devices()).toHaveLength(2);
    store.apply({ ...phone, name: 'Pocket' });
    expect(store.devices().find((d) => d.id === 'p')?.name).toBe('Pocket');
    store.remove('p');
    expect(store.devices()).toHaveLength(1);
    await store.revoke('a');
    expect(api.revokeDevice).toHaveBeenCalledWith('a');
    expect(store.devices()).toHaveLength(0);
  });
});

/** Pure helpers for Settings → Devices. */

import { Device, DeviceKind } from '../../core/models/device.models';

export const KIND_ICON: Record<DeviceKind, string> = {
  browser: 'monitor',
  phone: 'phone',
  cli: 'shell',
  mcp: 'key',
};
export const KIND_LABEL: Record<DeviceKind, string> = {
  browser: 'Browser',
  phone: 'Phone',
  cli: 'CLI',
  mcp: 'MCP client',
};

/** "just now", "5 min ago", "3 h ago", "2 days ago". */
export function ago(iso: string, now = Date.now()): string {
  const ms = now - Date.parse(iso);
  if (!Number.isFinite(ms) || ms < 60_000) return 'just now';
  const min = Math.floor(ms / 60_000);
  if (min < 60) return `${min} min ago`;
  const h = Math.floor(min / 60);
  if (h < 24) return `${h} h ago`;
  const d = Math.floor(h / 24);
  return d === 1 ? 'yesterday' : `${d} days ago`;
}

/** "in 89 days" until a device's sliding expiry. */
export function expiresIn(iso: string, now = Date.now()): string {
  const days = Math.floor((Date.parse(iso) - now) / 86_400_000);
  if (!Number.isFinite(days)) return '';
  if (days < 1) return 'today';
  return days === 1 ? 'in 1 day' : `in ${days} days`;
}

/** "Safari on iPhone · 192.168.1.20" (where a device was last used from). */
export function whereFrom(d: Pick<Device, 'last_user_agent' | 'last_ip'>): string {
  return [d.last_user_agent, d.last_ip].filter(Boolean).join(' · ');
}

/** The Claude Code command that adds Konsert as an HTTP MCP server. */
export function claudeMcpCommand(url: string, token: string): string {
  return `claude mcp add --transport http konsert ${url} --header "Authorization: Bearer ${token}"`;
}

/** A generic mcpServers JSON snippet (Claude Desktop, Cursor and other clients). */
export function mcpJsonSnippet(url: string, token: string): string {
  return JSON.stringify(
    {
      mcpServers: { konsert: { type: 'http', url, headers: { Authorization: `Bearer ${token}` } } },
    },
    null,
    2,
  );
}

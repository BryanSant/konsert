/** Device sign-in (KON-W0P8B, KON-V5JZB, KON-A29CT). Mirrors internal/controller/devices.go. */

export type DeviceKind = 'browser' | 'phone' | 'cli' | 'mcp';

/** A signed-in browser, phone, CLI or MCP client with its own revocable credential. */
export interface Device {
  id: string;
  sid: string;
  name: string;
  kind: DeviceKind;
  created_at: string;
  /** How it was issued: "pairing code pc-…", "recovery key", or the device that made it. */
  created_by?: string;
  last_used_at: string;
  last_ip?: string;
  /** "Chrome on macOS", "konsert/1.2" (never the raw User-Agent). */
  last_user_agent?: string;
  /** Sliding expiry: last use plus the idle period. */
  expires_at: string;
  /** Set only in lists: the credential this browser is using. */
  current?: boolean;
  expired?: boolean;
}

export interface DevicesResponse {
  devices: Device[];
  current_device_id: string;
  idle_days: number;
  auth_required: boolean;
}

/** A QR code as an SVG path over a size×size viewBox. */
export interface QrSvg {
  size: number;
  path: string;
}

/** A fresh single-use pairing code (POST /api/pairing-codes). */
export interface PairingCode {
  code: string;
  sid: string;
  url: string;
  expires_at: string;
  qr: QrSvg;
}

export interface PairResponse {
  authenticated: boolean;
  user: string;
  device?: Device;
}

/** POST /api/devices/mcp: the token is shown once. */
export interface McpCredential {
  device: Device;
  token: string;
  mcp_url: string;
}

/** GET /api/auth/device-code/{code}: what `konsert login` is asking to sign in. */
export interface LoginRequest {
  sid: string;
  name: string;
  kind: DeviceKind;
  created_at: string;
  created_ip?: string;
  expires_at: string;
  status: 'pending' | 'approved' | 'denied' | 'expired' | 'redeemed';
}

export type ForgeKind = 'github' | 'gitlab' | 'forgejo' | '';

export type ForgeCredentialSource = 'git-credential' | 'gh' | 'glab' | 'device' | 'pasted' | 'none';

export interface ForgeHost {
  host: string;
  kind: ForgeKind;
  credential_source: ForgeCredentialSource;
  device_flow_available: boolean;
}

export interface ForgeDetectResult {
  host: string;
  kind: ForgeKind;
  api_base: string;
}

export interface ForgeTokenSaveResult {
  ok: boolean;
  host: string;
  kind: ForgeKind;
  credential_source: ForgeCredentialSource;
}

export interface ForgeDeviceStart {
  id: string;
  host: string;
  kind: ForgeKind;
  user_code: string;
  verification_uri: string;
  verification_uri_complete?: string;
  interval: number;
  expires_at: string;
}

export interface ForgeDevicePoll {
  status: 'pending' | 'slow_down' | 'authorized' | 'expired' | 'denied';
  host?: string;
  kind?: ForgeKind;
  credential_source?: ForgeCredentialSource;
  interval?: number;
  error?: string;
}

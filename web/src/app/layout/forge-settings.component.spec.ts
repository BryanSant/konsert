import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ForgeHost } from '../core/models/forge.models';
import { ApiClientService } from '../core/services/api-client.service';
import {
  ForgeSettingsComponent,
  buildDetectPayload,
  credentialSourceLabel,
  forgeKindLabel,
  hasStoredToken,
} from './forge-settings.component';

describe('forge helpers', () => {
  it('maps forge kinds to human labels', () => {
    expect(forgeKindLabel('github')).toBe('GitHub');
    expect(forgeKindLabel('gitlab')).toBe('GitLab');
    expect(forgeKindLabel('forgejo')).toBe('Forgejo');
    expect(forgeKindLabel('')).toBe('Unknown');
  });

  it('maps credential sources to plain words', () => {
    expect(credentialSourceLabel('git-credential', 'github')).toBe('from your git credentials');
    expect(credentialSourceLabel('gh', 'github')).toBe('from gh');
    expect(credentialSourceLabel('glab', 'gitlab')).toBe('from glab');
    expect(credentialSourceLabel('pasted', 'forgejo')).toBe('token you pasted');
    expect(credentialSourceLabel('device', 'github')).toBe('signed in with GitHub');
    expect(credentialSourceLabel('device', 'gitlab')).toBe('signed in with GitLab');
    expect(credentialSourceLabel('none', 'github')).toBe('not connected');
  });

  it('identifies hub-stored tokens', () => {
    expect(hasStoredToken('pasted')).toBe(true);
    expect(hasStoredToken('device')).toBe(true);
    expect(hasStoredToken('git-credential')).toBe(false);
    expect(hasStoredToken('gh')).toBe(false);
    expect(hasStoredToken('glab')).toBe(false);
    expect(hasStoredToken('none')).toBe(false);
  });

  it('builds detect payloads for hostnames and remote URLs', () => {
    expect(buildDetectPayload('github.com')).toEqual({ host: 'github.com' });
    expect(buildDetectPayload('git.corp.example:8443')).toEqual({ host: 'git.corp.example:8443' });
    expect(buildDetectPayload('https://codeberg.org/alice/demo.git')).toEqual({
      remote_url: 'https://codeberg.org/alice/demo.git',
    });
    expect(buildDetectPayload('git@gitlab.com:acme/api.git')).toEqual({
      remote_url: 'git@gitlab.com:acme/api.git',
    });
  });
});

describe('ForgeSettingsComponent', () => {
  let api: Record<string, ReturnType<typeof vi.fn>>;
  let hosts: ForgeHost[];

  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    hosts = [
      { host: 'github.com', kind: 'github', credential_source: 'gh', device_flow_available: true },
      { host: 'gitlab.example.com', kind: 'gitlab', credential_source: 'none', device_flow_available: false },
      { host: 'codeberg.org', kind: 'forgejo', credential_source: 'pasted', device_flow_available: false },
    ];
    api = {
      listForges: vi.fn(async () => ({ forges: hosts.map((h) => ({ ...h })) })),
      detectForge: vi.fn(async (req: { remote_url?: string; host?: string }) => {
        const added: ForgeHost = {
          host: 'git.local.test:3000',
          kind: 'forgejo',
          credential_source: 'none',
          device_flow_available: false,
        };
        hosts = [...hosts, added];
        return { host: added.host, kind: added.kind, api_base: `http://${added.host}/api/v1`, ...req };
      }),
      putForgeToken: vi.fn(async (host: string) => {
        hosts = hosts.map((h) => (h.host === host ? { ...h, credential_source: 'pasted' } : h));
        return { ok: true, host, kind: 'gitlab', credential_source: 'pasted' };
      }),
      deleteForgeToken: vi.fn(async (host: string) => {
        hosts = hosts.map((h) => (h.host === host ? { ...h, credential_source: 'none' } : h));
        return { ok: true, host };
      }),
      startForgeDevice: vi.fn(async (host: string) => ({
        id: 'flow-1',
        host,
        kind: 'github',
        user_code: 'ABCD-1234',
        verification_uri: 'https://github.com/login/device',
        interval: 2,
        expires_at: new Date(Date.now() + 600_000).toISOString(),
      })),
      pollForgeDevice: vi.fn(),
    };
    TestBed.configureTestingModule({ providers: [{ provide: ApiClientService, useValue: api }] });
  });

  afterEach(() => {
    vi.useRealTimers();
    TestBed.resetTestingModule();
  });

  async function setup() {
    const fixture = TestBed.createComponent(ForgeSettingsComponent);
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();
    return { fixture, cmp: fixture.componentInstance, root: fixture.nativeElement as HTMLElement };
  }

  it('lists hosts with kind, plain-word credential source, and device availability', async () => {
    const { root } = await setup();
    expect(root.textContent).toContain('open and merge PRs/MRs');
    expect(root.querySelectorAll('[title]').length).toBe(0);

    const gh = root.querySelector('[data-forge-host="github.com"]')!;
    expect(gh.querySelector('[data-testid="forge-kind"]')?.textContent?.trim()).toBe('GitHub');
    expect(gh.querySelector('[data-testid="forge-source"]')?.textContent?.trim()).toBe('from gh');
    expect(gh.querySelector('[data-testid="forge-connect-btn"]')).not.toBeNull();
    expect(gh.querySelector('[data-testid="forge-remove-btn"]')).toBeNull();

    const gl = root.querySelector('[data-forge-host="gitlab.example.com"]')!;
    expect(gl.querySelector('[data-testid="forge-source"]')?.textContent?.trim()).toBe('not connected');
    expect(gl.querySelector('[data-testid="forge-connect-btn"]')).toBeNull();
    expect(gl.querySelector('[data-testid="forge-device-unavailable"]')?.textContent).toContain('OAuth app configured');

    const cb = root.querySelector('[data-forge-host="codeberg.org"]')!;
    expect(cb.querySelector('[data-testid="forge-source"]')?.textContent?.trim()).toBe('token you pasted');
    expect(cb.querySelector('[data-testid="forge-remove-btn"]')).not.toBeNull();
  });

  it('pastes a token with masked input, clears it after saving, and never renders it', async () => {
    const { fixture, cmp, root } = await setup();
    const secret = 'glpat_super_secret_never_render_999';
    cmp.openPaste('gitlab.example.com');
    fixture.detectChanges();

    const input = root.querySelector('[data-testid="forge-token-input"]') as HTMLInputElement;
    expect(input.type).toBe('password');
    cmp.tokenDraft.set(secret);
    await cmp.saveToken(hosts[1]);
    fixture.detectChanges();

    expect(api['putForgeToken']).toHaveBeenCalledWith('gitlab.example.com', secret, 'gitlab');
    expect(cmp.tokenDraft()).toBe('');
    expect(cmp.pasteHost()).toBeNull();
    expect(root.textContent).not.toContain(secret);
    const gl = root.querySelector('[data-forge-host="gitlab.example.com"]')!;
    expect(gl.querySelector('[data-testid="forge-source"]')?.textContent?.trim()).toBe('token you pasted');
  });

  it('requires confirmation before removing a stored token', async () => {
    const { fixture, cmp, root } = await setup();
    cmp.askRemove('codeberg.org');
    fixture.detectChanges();
    expect(root.querySelector('[data-testid="forge-remove-confirm-panel"]')).not.toBeNull();
    expect(api['deleteForgeToken']).not.toHaveBeenCalled();

    await cmp.confirmRemove('codeberg.org');
    fixture.detectChanges();

    expect(api['deleteForgeToken']).toHaveBeenCalledWith('codeberg.org');
    const cb = root.querySelector('[data-forge-host="codeberg.org"]')!;
    expect(cb.querySelector('[data-testid="forge-source"]')?.textContent?.trim()).toBe('not connected');
  });

  it('runs OAuth device sign-in: shows code, polls pending then authorized', async () => {
    let calls = 0;
    api['pollForgeDevice'].mockImplementation(async () => {
      calls++;
      if (calls === 1) return { status: 'pending', interval: 2 };
      hosts = hosts.map((h) => (h.host === 'github.com' ? { ...h, credential_source: 'device' } : h));
      return { status: 'authorized', host: 'github.com', kind: 'github', credential_source: 'device' };
    });

    const { fixture, cmp, root } = await setup();
    await cmp.startDevice(hosts[0]);
    fixture.detectChanges();

    expect(root.querySelector('[data-testid="forge-device-code"]')?.textContent?.trim()).toBe('ABCD-1234');
    const link = root.querySelector('[data-testid="forge-device-link"]') as HTMLAnchorElement;
    expect(link.href).toBe('https://github.com/login/device');

    await vi.advanceTimersByTimeAsync(2100);
    await fixture.whenStable();
    fixture.detectChanges();
    expect(api['pollForgeDevice']).toHaveBeenCalledTimes(1);
    expect(cmp.deviceFlow()?.status).toBe('pending');

    await vi.advanceTimersByTimeAsync(2100);
    await fixture.whenStable();
    fixture.detectChanges();
    expect(api['pollForgeDevice']).toHaveBeenCalledTimes(2);
    expect(cmp.deviceFlow()?.status).toBe('authorized');
    expect(root.querySelector('[data-forge-host="github.com"] [data-testid="forge-source"]')?.textContent?.trim()).toBe(
      'signed in with GitHub',
    );
  });

  it('handles expired device sign-in codes', async () => {
    api['pollForgeDevice'].mockResolvedValueOnce({ status: 'expired', error: 'expired_token' });
    const { fixture, cmp, root } = await setup();
    await cmp.startDevice(hosts[0]);
    fixture.detectChanges();

    await vi.advanceTimersByTimeAsync(2100);
    await fixture.whenStable();
    fixture.detectChanges();

    expect(cmp.deviceFlow()?.status).toBe('expired');
    expect(root.querySelector('[data-testid="forge-device-status"]')?.textContent).toContain('expired');
  });

  it('detects and adds a forge host', async () => {
    const { fixture, cmp, root } = await setup();
    cmp.addInput.set('http://git.local.test:3000');
    await cmp.addForge();
    fixture.detectChanges();

    expect(api['detectForge']).toHaveBeenCalledWith({ remote_url: 'http://git.local.test:3000' });
    expect(cmp.addInput()).toBe('');
    expect(root.querySelector('[data-forge-host="git.local.test:3000"]')).not.toBeNull();
    expect(root.querySelector('[data-testid="forge-notice"]')?.textContent).toContain('Added git.local.test:3000 (Forgejo)');
  });
});

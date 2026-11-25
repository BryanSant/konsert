import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { JoinCode } from '../core/models/syre.models';
import { ManualJoinComponent, expiresIn, joinCommand } from './manual-join.component';
import { MachineCredentialsSecurityComponent } from './machine-credentials-security.component';
import { ago } from './machine-credential.component';

const code: JoinCode = {
  id: 'jc-1',
  code: 'ABCD-EFGH',
  expires_at: new Date(Date.now() + 15 * 60_000).toISOString(),
  hub_url: 'https://desk.example.ts.net',
  command: 'syred join https://desk.example.ts.net ABCD-EFGH',
};

describe('manual join helpers', () => {
  it('builds the join command with options', () => {
    expect(joinCommand(code, { sandbox: false, enableMock: false })).toBe(code.command);
    expect(joinCommand(code, { sandbox: true, enableMock: true })).toBe(`${code.command} --sandbox --enable-mock-driver`);
  });

  it('counts down to expiry', () => {
    const t = Date.parse('2026-11-23T10:00:00Z');
    expect(expiresIn('2026-11-23T10:15:00Z', t)).toBe('15 min');
    expect(expiresIn('2026-11-23T10:00:45Z', t)).toBe('45 s');
    expect(expiresIn('2026-11-23T09:59:00Z', t)).toBe('');
  });

  it('formats relative times', () => {
    const t = Date.parse('2026-11-23T10:00:00Z');
    expect(ago(undefined, t)).toBe('never');
    expect(ago('2026-11-23T09:57:00Z', t)).toBe('3m ago');
    expect(ago('2026-11-21T10:00:00Z', t)).toBe('2d ago');
  });
});

describe('ManualJoinComponent', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let status = 'pending';

  beforeEach(() => {
    status = 'pending';
    fetchMock = vi.fn(async (url: string) => {
      if (url === '/api/machines/join-codes') return new Response(JSON.stringify(code), { status: 201 });
      if (url === '/api/machines/join-codes/jc-1') {
        return new Response(
          JSON.stringify({ id: 'jc-1', status, expires_at: code.expires_at, worker_id: 'w1', worker_sid: 'm-abcde', worker_name: 'laptop' }),
          { status: 200 },
        );
      }
      if (url === '/api/fleet') return new Response(JSON.stringify({ workers: [], tasks: [], active_tasks_count: 0 }), { status: 200 });
      throw new Error(`unexpected ${url}`);
    });
    vi.stubGlobal('fetch', fetchMock);
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    TestBed.configureTestingModule({});
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
    TestBed.resetTestingModule();
  });

  it('shows a join command and follows the machine once it joins', async () => {
    const fixture = TestBed.createComponent(ManualJoinComponent);
    fixture.detectChanges();
    await vi.waitFor(() => expect(fixture.componentInstance.code()).not.toBeNull());
    fixture.detectChanges();
    const el = fixture.nativeElement as HTMLElement;
    expect(el.querySelector('[data-testid="join-command"]')?.textContent?.trim()).toBe(code.command);
    expect(el.querySelector('[data-testid="join-expiry"]')?.textContent).toContain('expires in');

    status = 'used';
    await vi.advanceTimersByTimeAsync(2000);
    await vi.waitFor(() => expect(fixture.componentInstance.joined()?.name).toBe('laptop'));
    fixture.detectChanges();
    expect(el.querySelector('[data-testid="joined-machine"]')?.textContent).toContain('m-abcde');
    expect(el.querySelector('[data-testid="joined-state"]')?.textContent).toContain('connecting');
  });
});

describe('MachineCredentialsSecurityComponent', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    TestBed.resetTestingModule();
  });

  it('rotates after confirmation and lists machines needing a join code', async () => {
    const fetchMock = vi.fn(async (_url: string) =>
      new Response(
        JSON.stringify({ results: [{ worker_id: 'w1', worker_sid: 'm-abcde', name: 'laptop', action: 'revoked', needs_join: true }], needs_join: 1, failed: 0 }),
        { status: 200 },
      ),
    );
    vi.stubGlobal('fetch', fetchMock);
    TestBed.configureTestingModule({});
    const fixture = TestBed.createComponent(MachineCredentialsSecurityComponent);
    fixture.detectChanges();
    const el = fixture.nativeElement as HTMLElement;
    (el.querySelector('[data-testid="rotate-machine-credentials"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(fetchMock).not.toHaveBeenCalled();
    (el.querySelector('[data-testid="confirm-rotate-machine-credentials"]') as HTMLButtonElement).click();
    await vi.waitFor(() => expect(fixture.componentInstance.result()).not.toBeNull());
    fixture.detectChanges();
    expect(fetchMock.mock.calls[0][0]).toBe('/api/machines/credentials/rotate');
    expect(el.querySelector('[data-testid="rotation-result"]')?.textContent).toContain('laptop');
    expect(el.querySelector('[data-testid="rotation-result"]')?.textContent).toContain('needs a new join code');
  });
});

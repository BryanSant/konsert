import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClientService, ApiError, KONSERT_REQUEST_HEADER } from './api-client.service';
import { AuthService, SESSION_EXPIRED_MESSAGE } from './auth.service';

type FetchArgs = [string, RequestInit];

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('ApiClientService', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let api: ApiClientService;

  beforeEach(() => {
    fetchMock = vi.fn(async () => jsonResponse(200, { ok: true }));
    vi.stubGlobal('fetch', fetchMock);
    TestBed.configureTestingModule({});
    api = TestBed.inject(ApiClientService);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    TestBed.resetTestingModule();
  });

  function lastCall(): { url: string; init: RequestInit; headers: Headers } {
    const [url, init] = fetchMock.mock.calls[fetchMock.mock.calls.length - 1] as FetchArgs;
    return { url, init, headers: new Headers(init.headers) };
  }

  it('sends same-origin credentials, JSON content type and the CSRF marker header', async () => {
    await api.getFleet();
    const { url, init, headers } = lastCall();
    expect(url).toBe('/api/fleet');
    expect(init.credentials).toBe('same-origin');
    expect(headers.get('Content-Type')).toBe('application/json');
    expect(headers.get(KONSERT_REQUEST_HEADER)).toBe('1');
  });

  it('gives bodiless POSTs a JSON body and keeps the JSON content type', async () => {
    await api.cancelTask('t 1');
    const { url, init, headers } = lastCall();
    expect(url).toBe('/api/tasks/t%201/cancel');
    expect(init.method).toBe('POST');
    expect(init.body).toBe('{}');
    expect(headers.get('Content-Type')).toBe('application/json');
  });

  it('sends DELETE with JSON content type and the marker header', async () => {
    await api.deleteSpace('sp1');
    const { init, headers } = lastCall();
    expect(init.method).toBe('DELETE');
    expect(headers.get('Content-Type')).toBe('application/json');
    expect(headers.get(KONSERT_REQUEST_HEADER)).toBe('1');
  });

  it('uploads attachments as multipart without forcing a JSON content type', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse(200, { ok: true, worker_id: 'w', remote_path: '/x', filename: 'a', size: 1 }),
    );
    await api.uploadAttachment(new File(['hi'], 'a.txt'), { workerId: 'w1' });
    const { init, headers } = lastCall();
    expect(init.body).toBeInstanceOf(FormData);
    expect(headers.has('Content-Type')).toBe(false);
    expect(headers.get(KONSERT_REQUEST_HEADER)).toBe('1');
  });

  it('maps error bodies to ApiError with the status code', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(409, { error: 'already exists' }));
    const err = await api.createWorkspace('x').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).message).toBe('already exists');
    expect((err as ApiError).status).toBe(409);
  });

  it('turns 429 into a retry hint honouring Retry-After', async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ error: 'too many failed login attempts; try again later' }), {
        status: 429,
        headers: { 'Retry-After': '42' },
      }),
    );
    await expect(api.login('x')).rejects.toThrow('Too many attempts, try again in 42s.');
  });

  it('explains CSRF-guard and size rejections', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(403, { error: 'cross-site request rejected' }));
    await expect(api.cancelTask('t')).rejects.toThrow(/cross-site request rejected.*Reload/);
    fetchMock.mockResolvedValueOnce(
      jsonResponse(415, { error: 'Content-Type must be application/json' }),
    );
    await expect(api.cancelTask('t')).rejects.toThrow(/request format/);
    fetchMock.mockResolvedValueOnce(jsonResponse(413, { error: 'file exceeds 12 MiB limit' }));
    await expect(api.uploadAttachment(new File(['x'], 'a'))).rejects.toThrow(
      'Too large: file exceeds 12 MiB limit.',
    );
  });

  it('returns the 409 host-key result from a non-streaming SSH bootstrap', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse(409, {
        ok: false,
        target: 'box',
        stage: 'host_key_confirmation',
        host_key_confirmation_required: true,
        host_keys: [{ type: 'ssh-ed25519', fingerprint: 'SHA256:abc' }],
      }),
    );
    const res = await api.bootstrapSSH({ target: 'box', controller_url: 'ws://h/ws/worker' });
    expect(res.host_key_confirmation_required).toBe(true);
    expect(res.host_keys?.[0].fingerprint).toBe('SHA256:abc');
  });

  it('falls back to "HTTP <status>" for non-JSON errors', async () => {
    fetchMock.mockResolvedValueOnce(new Response('<html>bad gateway</html>', { status: 502 }));
    await expect(api.getFleet()).rejects.toThrow('HTTP 502');
  });

  it('invokes the unauthorized handler on 401, except for login', async () => {
    const handler = vi.fn();
    api.setUnauthorizedHandler(handler);

    fetchMock.mockResolvedValueOnce(jsonResponse(401, { error: 'invalid token' }));
    await expect(api.login('nope')).rejects.toThrow('invalid token');
    expect(handler).not.toHaveBeenCalled();

    fetchMock.mockResolvedValueOnce(jsonResponse(401, { error: 'authentication required' }));
    await expect(api.getFleet()).rejects.toThrow('authentication required');
    expect(handler).toHaveBeenCalledTimes(1);
  });
});

describe('AuthService session expiry', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    TestBed.configureTestingModule({});
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    TestBed.resetTestingModule();
  });

  it('returns to the login screen when any request comes back 401', async () => {
    const auth = TestBed.inject(AuthService);
    const api = TestBed.inject(ApiClientService);
    fetchMock.mockResolvedValueOnce(
      jsonResponse(200, { authenticated: true, auth_required: true, user: 'operator' }),
    );
    await auth.checkAuth();
    expect(auth.authenticated()).toBe(true);

    fetchMock.mockResolvedValueOnce(jsonResponse(401, { error: 'authentication required' }));
    await expect(api.getFleet()).rejects.toBeInstanceOf(ApiError);
    expect(auth.authenticated()).toBe(false);
    expect(auth.error()).toBe(SESSION_EXPIRED_MESSAGE);
  });

  it('verifySession logs out when /api/auth/me says unauthenticated, but not on network errors', async () => {
    const auth = TestBed.inject(AuthService);
    auth.authenticated.set(true);

    fetchMock.mockRejectedValueOnce(new TypeError('Failed to fetch'));
    expect(await auth.verifySession()).toBe(true);
    expect(auth.authenticated()).toBe(true);

    fetchMock.mockResolvedValueOnce(
      jsonResponse(200, { authenticated: false, auth_required: true, user: '' }),
    );
    expect(await auth.verifySession()).toBe(false);
    expect(auth.authenticated()).toBe(false);
  });
});

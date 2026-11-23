import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakeWebSocket } from '../../../testing/fake-websocket';
import { ApiClientService } from './api-client.service';
import { AuthService } from './auth.service';
import {
  HANDSHAKE_FAILURES_BEFORE_AUTH_CHECK,
  KonsertSocketService,
} from './konsert-socket.service';

describe('KonsertSocketService', () => {
  let restoreWs: () => void;
  let api: Record<string, ReturnType<typeof vi.fn>>;
  let socket: KonsertSocketService;
  let auth: AuthService;

  beforeEach(() => {
    vi.useFakeTimers();
    restoreWs = FakeWebSocket.install();
    api = {
      getMe: vi.fn(async () => ({ authenticated: true, auth_required: true, user: 'operator' })),
      getFleet: vi.fn(async () => ({ workers: [], tasks: [] })),
      listChatMessages: vi.fn(async () => ({ messages: [] })),
      getTaskEvents: vi.fn(async () => ({ events: [] })),
      setUnauthorizedHandler: vi.fn(),
    };
    TestBed.configureTestingModule({
      providers: [{ provide: ApiClientService, useValue: api }],
    });
    auth = TestBed.inject(AuthService);
    auth.authenticated.set(true);
    socket = TestBed.inject(KonsertSocketService);
  });

  afterEach(() => {
    socket.disconnect();
    restoreWs();
    vi.useRealTimers();
    TestBed.resetTestingModule();
  });

  describe('session expiry', () => {
    async function failHandshakes(n: number): Promise<void> {
      for (let i = 0; i < n; i++) {
        FakeWebSocket.last.serverClose();
        await vi.advanceTimersByTimeAsync(16000);
      }
    }

    it('re-checks /api/auth/me after repeated failed handshakes and stops when the session is gone', async () => {
      socket.connect();
      await failHandshakes(HANDSHAKE_FAILURES_BEFORE_AUTH_CHECK - 1);
      expect(api['getMe']).not.toHaveBeenCalled();

      api['getMe'].mockResolvedValueOnce({ authenticated: false, auth_required: true, user: '' });
      FakeWebSocket.last.serverClose();
      await vi.waitFor(() => expect(api['getMe']).toHaveBeenCalledTimes(1));
      await vi.waitFor(() => expect(auth.authenticated()).toBe(false));

      const count = FakeWebSocket.instances.length;
      await vi.advanceTimersByTimeAsync(60000);
      expect(FakeWebSocket.instances.length).toBe(count);
      expect(socket.state()).toBe('disconnected');
    });

    it('keeps retrying when the session is still valid', async () => {
      socket.connect();
      await failHandshakes(HANDSHAKE_FAILURES_BEFORE_AUTH_CHECK);
      expect(api['getMe']).toHaveBeenCalledTimes(1);
      expect(auth.authenticated()).toBe(true);
      const ws = FakeWebSocket.last;
      ws.open();
      expect(socket.state()).toBe('connected');
    });

    it('does not count sockets that opened as handshake failures', async () => {
      socket.connect();
      for (let i = 0; i < HANDSHAKE_FAILURES_BEFORE_AUTH_CHECK + 1; i++) {
        FakeWebSocket.last.open();
        FakeWebSocket.last.serverClose();
        await vi.advanceTimersByTimeAsync(16000);
      }
      expect(api['getMe']).not.toHaveBeenCalled();
    });
  });
});

import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakeWebSocket } from '../../../testing/fake-websocket';
import { ApiClientService } from './api-client.service';
import {
  FRAME_HEADER_SIZE,
  FRAME_PTY_DATA,
  FRAME_PTY_SNAPSHOT,
  FRAME_PTY_STDIN,
} from '../models/syre.models';
import { AuditStore } from '../state/audit.store';
import { FleetStore } from '../state/fleet.store';
import { TicketStore } from '../state/ticket.store';
import { CommanderStore } from '../state/commander.store';
import { AuthService } from './auth.service';
import {
  HANDSHAKE_FAILURES_BEFORE_AUTH_CHECK,
  SyreSocketService,
} from './syre-socket.service';

describe('SyreSocketService', () => {
  let restoreWs: () => void;
  let api: Record<string, ReturnType<typeof vi.fn>>;
  let socket: SyreSocketService;
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
    socket = TestBed.inject(SyreSocketService);
  });

  afterEach(() => {
    socket.disconnect();
    restoreWs();
    vi.useRealTimers();
    TestBed.resetTestingModule();
  });

  describe('PTY frames', () => {
    function frame(type: number, taskId: string, payload: string): ArrayBuffer {
      const enc = new TextEncoder();
      const id = enc.encode(taskId);
      const data = enc.encode(payload);
      const buf = new Uint8Array(FRAME_HEADER_SIZE + data.byteLength);
      buf[0] = type;
      buf.set(id, 1);
      buf.set(data, FRAME_HEADER_SIZE);
      return buf.buffer;
    }

    it('parses binary frames, strips NUL padding from the task id, and routes by task', () => {
      socket.connect();
      const ws = FakeWebSocket.last;
      ws.open();
      const a = vi.fn();
      const b = vi.fn();
      socket.subscribePty('task-a', a);
      socket.subscribePty('task-b', b);

      ws.receive(frame(FRAME_PTY_DATA, 'task-a', 'hello'));
      ws.receive(frame(FRAME_PTY_SNAPSHOT, 'task-b', 'screen'));
      ws.receive(frame(0x7f, 'task-a', 'ignored type'));
      ws.receive(new Uint8Array([FRAME_PTY_DATA, 1, 2]).buffer); // truncated header
      ws.receive(frame(FRAME_PTY_DATA, 'nobody', 'dropped'));

      expect(a).toHaveBeenCalledTimes(1);
      expect(a.mock.calls[0][0].taskId).toBe('task-a');
      expect(new TextDecoder().decode(a.mock.calls[0][0].data)).toBe('hello');
      expect(b.mock.calls[0][0].frameType).toBe(FRAME_PTY_SNAPSHOT);
    });

    it('subscribes once per task, unsubscribes with the last listener, and resubscribes after reconnect', async () => {
      socket.connect();
      const ws = FakeWebSocket.last;
      ws.open();
      const off1 = socket.subscribePty('t1', vi.fn());
      const off2 = socket.subscribePty('t1', vi.fn());
      const pty = (w: FakeWebSocket) =>
        w
          .sentJSON()
          .filter((e) => e.type.startsWith('pty.'))
          .map((e) => [e.type, e.payload]);
      expect(pty(ws)).toEqual([['pty.subscribe', { task_id: 't1' }]]);

      ws.serverClose();
      await vi.advanceTimersByTimeAsync(1000);
      const ws2 = FakeWebSocket.last;
      ws2.open();
      expect(pty(ws2)).toEqual([['pty.subscribe', { task_id: 't1' }]]);

      off1();
      expect(pty(ws2)).toHaveLength(1);
      off2();
      expect(pty(ws2)).toEqual([
        ['pty.subscribe', { task_id: 't1' }],
        ['pty.unsubscribe', { task_id: 't1' }],
      ]);
    });

    it('encodes stdin as a binary frame', () => {
      socket.connect();
      const ws = FakeWebSocket.last;
      ws.open();
      socket.sendPtyStdin('t1', 'ls\r');
      const sent = new Uint8Array(ws.sent[0] as ArrayBuffer);
      expect(sent[0]).toBe(FRAME_PTY_STDIN);
      expect(new TextDecoder().decode(sent.subarray(1, 3))).toBe('t1');
      expect(new TextDecoder().decode(sent.subarray(FRAME_HEADER_SIZE))).toBe('ls\r');
    });
  });

  describe('reconnect backoff', () => {
    it('backs off exponentially up to 15s and resets after a successful open', async () => {
      socket.connect();
      const delays: number[] = [];
      for (let i = 0; i < 6; i++) {
        const before = FakeWebSocket.instances.length;
        FakeWebSocket.last.serverClose();
        let waited = 0;
        while (FakeWebSocket.instances.length === before) {
          await vi.advanceTimersByTimeAsync(500);
          waited += 500;
        }
        delays.push(waited);
      }
      expect(delays).toEqual([1000, 2000, 4000, 8000, 15000, 15000]);

      FakeWebSocket.last.open();
      const before = FakeWebSocket.instances.length;
      FakeWebSocket.last.serverClose();
      await vi.advanceTimersByTimeAsync(1000);
      expect(FakeWebSocket.instances.length).toBe(before + 1);
    });

    it('stops reconnecting after disconnect()', async () => {
      socket.connect();
      FakeWebSocket.last.open();
      socket.disconnect();
      const count = FakeWebSocket.instances.length;
      await vi.advanceTimersByTimeAsync(60000);
      expect(FakeWebSocket.instances.length).toBe(count);
    });
  });

  describe('fleet updates', () => {
    it('applies fleet.update frames to the stores and ignores malformed JSON', () => {
      socket.connect();
      const ws = FakeWebSocket.last;
      ws.open();
      const fleet = TestBed.inject(FleetStore);
      ws.receive('{not json');
      ws.receiveJSON('fleet.update', {
        kind: 'worker',
        worker: { id: 'w1', name: 'box', host_group: 'box', status: 'online' },
      });
      ws.receiveJSON('fleet.update', { kind: 'worker_deleted', deleted_id: 'nope' });
      expect(fleet.workersById()['w1']?.name).toBe('box');
      ws.receiveJSON('fleet.update', { kind: 'worker_deleted', deleted_id: 'w1' });
      expect(fleet.workersById()['w1']).toBeUndefined();
    });

    it('forwards fleet.update audit entries to AuditStore subscribers', () => {
      socket.connect();
      const ws = FakeWebSocket.last;
      ws.open();
      const seen: number[] = [];
      const unsubscribe = TestBed.inject(AuditStore).subscribe((ev) => seen.push(ev.id));
      ws.receiveJSON('fleet.update', {
        kind: 'audit',
        audit: { id: 7, created_at: '2026-11-22T00:00:00Z', user: 'mcp', action: 'task.cancel', task_id: 't1' },
      });
      ws.receiveJSON('fleet.update', { kind: 'audit' });
      unsubscribe();
      ws.receiveJSON('fleet.update', { kind: 'audit', audit: { id: 8, created_at: '', user: 'x', action: 'y' } });
      expect(seen).toEqual([7]);
    });

    it('applies ticket, edge, and ticket_deleted frames', () => {
      socket.connect();
      const ws = FakeWebSocket.last;
      ws.open();
      const tickets = TestBed.inject(TicketStore);
      ws.receiveJSON('fleet.update', {
        kind: 'ticket',
        ticket: {
          id: 'a',
          sid: 't-a',
          project_id: 'p',
          kind: 'feature',
          title: 'Schema',
          acceptance: [],
          labels: [],
          priority: 'P2',
          status: 'ready',
          created_at: '',
          updated_at: '',
        },
      });
      expect(tickets.bySid('t-a')?.title).toBe('Schema');
      ws.receiveJSON('fleet.update', {
        kind: 'ticket_edge',
        ticket_edge: { ticket_id: 'b', blocked_by_id: 'a', created_at: '' },
      });
      expect(tickets.edges()).toEqual([{ ticket_id: 'b', blocked_by_id: 'a', created_at: '' }]);
      ws.receiveJSON('fleet.update', {
        kind: 'ticket_edge_deleted',
        ticket_edge: { ticket_id: 'b', blocked_by_id: 'a' },
      });
      expect(tickets.edges()).toEqual([]);
      tickets.upsertEdge({ ticket_id: 'b', blocked_by_id: 'a', created_at: '' });
      ws.receiveJSON('fleet.update', {
        kind: 'ticket_edge_deleted',
        ticket_edge: { ticket_id: 'b', blocked_by_id: 'a' },
      });
      expect(tickets.edges()).toEqual([]);
      ws.receiveJSON('fleet.update', { kind: 'ticket_deleted', deleted_id: 'a' });
      expect(tickets.bySid('t-a')).toBeUndefined();
    });
  });

  describe('chat subscriptions', () => {
    function chatFrames(ws: FakeWebSocket) {
      return ws.sentJSON().filter((e) => e.type.startsWith('chat.'));
    }

    it('subscribes to the active session, switches on select, and resubscribes after reconnect', async () => {
      const orch = TestBed.inject(CommanderStore);
      socket.connect();
      const ws = FakeWebSocket.last;
      ws.open();

      orch.activeSessionId.set('s1');
      TestBed.tick();
      expect(chatFrames(ws)).toEqual([
        { type: 'chat.subscribe', ts: expect.any(String), payload: { session_id: 's1' } },
      ]);

      orch.activeSessionId.set('s2');
      TestBed.tick();
      expect(
        chatFrames(ws)
          .slice(1)
          .map((e) => [e.type, e.payload]),
      ).toEqual([
        ['chat.unsubscribe', { session_id: 's1' }],
        ['chat.subscribe', { session_id: 's2' }],
      ]);

      ws.serverClose();
      await vi.advanceTimersByTimeAsync(1000);
      const ws2 = FakeWebSocket.last;
      ws2.open();
      expect(chatFrames(ws2).map((e) => [e.type, e.payload])).toEqual([
        ['chat.subscribe', { session_id: 's2' }],
      ]);
    });

    it('defers the subscribe until the socket opens', () => {
      const orch = TestBed.inject(CommanderStore);
      orch.activeSessionId.set('s1');
      TestBed.tick();
      socket.connect();
      const ws = FakeWebSocket.last;
      ws.open();
      expect(chatFrames(ws).map((e) => [e.type, e.payload])).toEqual([
        ['chat.subscribe', { session_id: 's1' }],
      ]);
    });
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

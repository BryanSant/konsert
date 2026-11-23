import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakeWebSocket } from '../../../testing/fake-websocket';
import { ChatMessage } from '../models/konsert.models';
import { ApiClientService } from '../services/api-client.service';
import { KonsertSocketService } from '../services/konsert-socket.service';
import {
  CHAT_CONNECTION_LOST_ERROR,
  CHAT_NOT_CONNECTED_ERROR,
  OrchestratorStore,
} from './orchestrator.store';

function msg(id: number, session: string, content: string): ChatMessage {
  return { id, session_id: session, role: 'assistant', content, created_at: '' };
}

describe('OrchestratorStore sending state machine', () => {
  let restoreWs: () => void;
  let listChatMessages: ReturnType<typeof vi.fn>;
  let store: OrchestratorStore;
  let socket: KonsertSocketService;

  beforeEach(() => {
    vi.useFakeTimers();
    restoreWs = FakeWebSocket.install();
    listChatMessages = vi.fn(async (sid: string) => ({
      messages: [msg(1, sid, `history of ${sid}`)],
    }));
    TestBed.configureTestingModule({
      providers: [
        {
          provide: ApiClientService,
          useValue: {
            listChatMessages,
            getFleet: vi.fn(async () => ({ workers: [], tasks: [] })),
            getTaskEvents: vi.fn(async () => ({ events: [] })),
          },
        },
      ],
    });
    store = TestBed.inject(OrchestratorStore);
    socket = TestBed.inject(KonsertSocketService);
  });

  afterEach(() => {
    socket.disconnect();
    restoreWs();
    vi.useRealTimers();
    TestBed.resetTestingModule();
  });

  async function connectAndSelect(sid = 's1'): Promise<FakeWebSocket> {
    socket.connect();
    const ws = FakeWebSocket.last;
    ws.open();
    await store.selectSession(sid);
    return ws;
  }

  it('refuses to send while disconnected, keeps composer unlocked and reports an error', async () => {
    await store.selectSession('s1');
    const ok = socket.sendChatMessage('s1', 'hello');
    expect(ok).toBe(false);
    expect(store.sending()).toBe(false);
    expect(store.error()).toBe(CHAT_NOT_CONNECTED_ERROR);
    expect(store.messages().map((m) => m.content)).toEqual(['history of s1']);
  });

  it('sends over an open socket and unlocks on chat.done', async () => {
    const ws = await connectAndSelect();
    expect(socket.sendChatMessage('s1', 'hello')).toBe(true);
    expect(store.sending()).toBe(true);
    expect(ws.sentJSON().find((e) => e.type === 'chat.send')?.payload).toEqual({
      session_id: 's1',
      content: 'hello',
    });

    ws.receiveJSON('chat.delta', { session_id: 's1', delta: 'Hi' });
    expect(store.streamingBuffer()).toBe('Hi');

    ws.receiveJSON('chat.done', { session_id: 's1' });
    await vi.waitFor(() => expect(store.sending()).toBe(false));
    expect(store.streamingBuffer()).toBe('');
  });

  it('unlocks on chat.error and shows the error', async () => {
    const ws = await connectAndSelect();
    socket.sendChatMessage('s1', 'hello');
    ws.receiveJSON('chat.error', { session_id: 's1', error: 'model exploded' });
    expect(store.sending()).toBe(false);
    expect(store.error()).toBe('model exploded');
  });

  it('switching sessions mid-reply never leaves the new session locked', async () => {
    const ws = await connectAndSelect();
    socket.sendChatMessage('s1', 'hello');
    ws.receiveJSON('chat.delta', { session_id: 's1', delta: 'partial' });

    await store.selectSession('s2');
    expect(store.sending()).toBe(false);
    expect(store.streamingBuffer()).toBe('');

    // Reply for s1 finishing while s2 is active releases the in-flight marker.
    ws.receiveJSON('chat.done', { session_id: 's1' });
    await store.selectSession('s1');
    expect(store.sending()).toBe(false);
  });

  it('a dropped socket mid-reply unlocks the composer and refetches on reconnect', async () => {
    const ws = await connectAndSelect();
    socket.sendChatMessage('s1', 'hello');
    expect(store.sending()).toBe(true);

    ws.serverClose();
    expect(store.sending()).toBe(false);
    expect(store.error()).toBe(CHAT_CONNECTION_LOST_ERROR);

    listChatMessages.mockClear();
    await vi.advanceTimersByTimeAsync(1000);
    const ws2 = FakeWebSocket.last;
    expect(ws2).not.toBe(ws);
    ws2.open();
    await vi.waitFor(() => expect(listChatMessages).toHaveBeenCalledWith('s1'));
    await vi.waitFor(() => expect(store.error()).toBe(''));
  });

  it('rolls back the optimistic message when the socket write throws', async () => {
    const ws = await connectAndSelect();
    ws.send = () => {
      throw new Error('boom');
    };
    expect(socket.sendChatMessage('s1', 'hello')).toBe(false);
    expect(store.sending()).toBe(false);
    expect(store.messages().map((m) => m.content)).toEqual(['history of s1']);
  });

  it('ignores a stale listChatMessages response after switching sessions', async () => {
    let resolveSlow: (v: { messages: ChatMessage[] }) => void = () => undefined;
    listChatMessages.mockImplementationOnce(
      () => new Promise((resolve) => (resolveSlow = resolve)),
    );
    const slow = store.selectSession('slow');
    await store.selectSession('fast');
    resolveSlow({ messages: [msg(9, 'slow', 'stale')] });
    await slow;
    expect(store.activeSessionId()).toBe('fast');
    expect(store.messages().map((m) => m.content)).toEqual(['history of fast']);
  });
});

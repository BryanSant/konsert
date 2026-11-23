import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FIXTURE_CHAT_ID, FIXTURE_WORKER_ID, claudeAccount, ev, resetSeq, sampleTurn } from '../../../testing/agent-chat.fixtures';
import { FakeWebSocket } from '../../../testing/fake-websocket';
import { ChatSession } from '../models/konsert.models';
import { ApiClientService } from '../services/api-client.service';
import { CHAT_PROTOCOL } from '../services/chat-protocol';
import { KonsertSocketService } from '../services/konsert-socket.service';
import { AgentAccountsStore } from './agent-accounts.store';
import { AgentChatStore } from './agent-chat.store';
import { OrchestratorStore } from './orchestrator.store';

const chat: ChatSession = {
  id: FIXTURE_CHAT_ID,
  title: 'Tests',
  provider: '',
  model: 'sonnet',
  created_at: '',
  updated_at: '',
  backend: 'agent',
  worker_id: FIXTURE_WORKER_ID,
  agent: 'claude',
  account_id: 'claude',
};

describe('AgentChatStore', () => {
  let restoreWs: () => void;
  let listAgentChatEvents: ReturnType<typeof vi.fn>;
  let store: AgentChatStore;
  let socket: KonsertSocketService;

  beforeEach(() => {
    vi.useFakeTimers();
    resetSeq();
    restoreWs = FakeWebSocket.install();
    listAgentChatEvents = vi.fn(async () => []);
    TestBed.configureTestingModule({
      providers: [
        {
          provide: ApiClientService,
          useValue: {
            listAgentChatEvents,
            setUnauthorizedHandler: vi.fn(),
            getFleet: vi.fn(async () => ({ workers: [], tasks: [] })),
            listAgentAccounts: vi.fn(async () => ({ accounts: [{ ...claudeAccount(), worker_id: FIXTURE_WORKER_ID }] })),
            patchAgentChat: vi.fn(async () => ({ ...chat, account_id: 'claude2' })),
          },
        },
      ],
    });
    store = TestBed.inject(AgentChatStore);
    socket = TestBed.inject(KonsertSocketService);
    TestBed.inject(OrchestratorStore).sessions.set([chat]);
  });

  afterEach(() => {
    socket.disconnect();
    restoreWs();
    vi.useRealTimers();
    TestBed.resetTestingModule();
  });

  function connect(): FakeWebSocket {
    socket.connect();
    const ws = FakeWebSocket.last;
    ws.open();
    return ws;
  }

  const frames = (ws: FakeWebSocket, type: string) => ws.sentJSON().filter((e) => e.type === type).map((e) => e.payload);

  it('applies agent.chat.event frames and updates the session status', () => {
    const ws = connect();
    for (const e of sampleTurn()) ws.receiveJSON(CHAT_PROTOCOL.wsEvent, e);
    const t = store.transcript(FIXTURE_CHAT_ID);
    expect(t.status).toBe('done');
    expect(t.items.map((i) => i.type)).toEqual(['user', 'thought', 'tool', 'plan', 'assistant', 'turn_end']);
    expect(TestBed.inject(OrchestratorStore).sessions()[0].status).toBe('done');
  });

  it('loads the log, then catches up after_seq on a gap and on reconnect', async () => {
    const events = sampleTurn();
    listAgentChatEvents.mockResolvedValueOnce(events.slice(0, 5));
    await store.open(FIXTURE_CHAT_ID);
    expect(listAgentChatEvents).toHaveBeenLastCalledWith(FIXTURE_CHAT_ID, 0);
    expect(store.transcript(FIXTURE_CHAT_ID).lastSeq).toBe(5);

    // seq 9 arrives while 6..8 were missed: fetch the gap instead of applying out of order.
    listAgentChatEvents.mockResolvedValueOnce(events.slice(5, 9));
    store.onEvent(events[8]);
    await vi.runAllTimersAsync();
    expect(listAgentChatEvents).toHaveBeenLastCalledWith(FIXTURE_CHAT_ID, 5);
    expect(store.transcript(FIXTURE_CHAT_ID).lastSeq).toBe(9);

    // Duplicates are ignored.
    store.onEvent(events[3]);
    expect(store.transcript(FIXTURE_CHAT_ID).lastSeq).toBe(9);

    TestBed.inject(OrchestratorStore).activeSessionId.set(FIXTURE_CHAT_ID);
    listAgentChatEvents.mockResolvedValueOnce(events.slice(9));
    await store.onReconnected();
    expect(listAgentChatEvents).toHaveBeenLastCalledWith(FIXTURE_CHAT_ID, 9);
    expect(store.transcript(FIXTURE_CHAT_ID).status).toBe('done');
  });

  it('sends chat.send with a turn id and an optimistic bubble; keeps the draft when offline', () => {
    expect(store.send(chat, 'hello')).toBe(false);
    expect(store.error()).toContain('Not connected');
    const ws = connect();
    expect(store.send(chat, 'hello', ['/var/tmp/a.png'])).toBe(true);
    const [payload] = frames(ws, CHAT_PROTOCOL.wsSend) as Array<Record<string, unknown>>;
    expect(payload).toMatchObject({ session_id: FIXTURE_CHAT_ID, chat_id: FIXTURE_CHAT_ID, content: 'hello', attachments: ['/var/tmp/a.png'] });
    expect(payload['turn_id']).toMatch(/^t-/);
    const t = store.transcript(FIXTURE_CHAT_ID);
    expect(t.status).toBe('working');
    expect(t.items[0]).toMatchObject({ type: 'user', pending: true, turnId: payload['turn_id'] });
  });

  it('responds to elicitations and controls the session with the protocol frames', () => {
    const ws = connect();
    ws.receiveJSON(
      CHAT_PROTOCOL.wsEvent,
      ev('elicitation', { elicitation: { id: 'el1', kind: 'permission', title: 'Run?', options: [{ id: 'allow_once', label: 'Allow once' }] } }),
    );
    expect(store.statusOf(FIXTURE_CHAT_ID)).toBe('needs_you');
    store.respond(FIXTURE_CHAT_ID, 'el1', { optionId: 'allow_once', label: 'Allow once' });
    store.respond(FIXTURE_CHAT_ID, 'el1', { text: 'use main', label: 'use main' });
    expect(frames(ws, CHAT_PROTOCOL.wsRespond)).toEqual([
      // The hub keys these frames on session_id.
      { chat_id: FIXTURE_CHAT_ID, session_id: FIXTURE_CHAT_ID, elicitation_id: 'el1', option_id: 'allow_once' },
      { chat_id: FIXTURE_CHAT_ID, session_id: FIXTURE_CHAT_ID, elicitation_id: 'el1', text: 'use main' },
    ]);
    store.cancel(FIXTURE_CHAT_ID);
    store.control(FIXTURE_CHAT_ID, { model: 'opus', effort: 'high', approval_mode: '' });
    expect(frames(ws, CHAT_PROTOCOL.wsControl)).toEqual([
      { chat_id: FIXTURE_CHAT_ID, session_id: FIXTURE_CHAT_ID, cancel: true },
      { chat_id: FIXTURE_CHAT_ID, session_id: FIXTURE_CHAT_ID, model: 'opus', effort: 'high' },
    ]);
    expect(TestBed.inject(OrchestratorStore).sessions()[0]).toMatchObject({ model: 'opus', effort: 'high' });
  });

  it('retries the last user message', () => {
    const ws = connect();
    store.send(chat, 'first');
    ws.receiveJSON(CHAT_PROTOCOL.wsEvent, ev('error', { error: { kind: 'rate_limit', message: 'busy', retryable: true } }));
    expect(store.retry(chat)).toBe(true);
    expect((frames(ws, CHAT_PROTOCOL.wsSend) as Array<{ content: string }>).map((f) => f.content)).toEqual(['first', 'first']);
  });

  it('loads accounts on connect and applies fleet.update agent_accounts', async () => {
    const ws = connect();
    await vi.runAllTimersAsync();
    const accounts = TestBed.inject(AgentAccountsStore);
    expect(accounts.accountsFor(FIXTURE_WORKER_ID).map((a) => a.id)).toEqual(['claude']);
    ws.receiveJSON('fleet.update', {
      kind: CHAT_PROTOCOL.fleetAccountsKind,
      worker_id: 'w2',
      accounts: [claudeAccount({ id: 'claude2', label: 'Claude 2', is_default: false })],
    });
    expect(accounts.accountsFor('w2').map((a) => a.label)).toEqual(['Claude 2']);
  });
});

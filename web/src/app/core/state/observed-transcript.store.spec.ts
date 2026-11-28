import { TestBed } from '@angular/core/testing';
import { ApiClientService } from '../services/api-client.service';
import { ObservedTranscriptStore } from './observed-transcript.store';

describe('ObservedTranscriptStore', () => {
  function setup(res: unknown) {
    const getObservedTranscript = vi.fn(async () => res);
    TestBed.configureTestingModule({ providers: [{ provide: ApiClientService, useValue: { getObservedTranscript } }] });
    return { store: TestBed.inject(ObservedTranscriptStore), getObservedTranscript };
  }
  afterEach(() => TestBed.resetTestingModule());

  it('folds the fetched transcript and live pushes into transcript items', async () => {
    const { store } = setup({
      external_id: 's1',
      available: true,
      events: [
        { chat_id: '', seq: 1, kind: 'user_message', turn_id: 'u1', message_id: 'u1', text: 'fix the build', at: '2026-11-27T10:00:00Z' },
        { chat_id: '', seq: 2, kind: 'message', turn_id: 'u1', message_id: 'a1', text: 'On it.', at: '2026-11-27T10:00:01Z' },
      ],
    });
    await store.load('t1');
    let v = store.views()['t1'];
    expect(v.available).toBe(true);
    expect(v.transcript.items.map((i) => i.type)).toEqual(['user', 'assistant']);

    store.onEvents({ external_id: 's1', task_id: 't1', events: [
      { chat_id: '', seq: 3, kind: 'tool_call', turn_id: 'u1', at: '2026-11-27T10:00:02Z', tool_call: { id: 'tc1', name: 'Bash', title: 'Run: go test', status: 'running' } },
    ] } as never);
    v = store.views()['t1'];
    expect(v.transcript.items.map((i) => i.type)).toEqual(['user', 'assistant', 'tool']);
    // A second reply in the same turn is its own message.
    store.onEvents({ external_id: 's1', task_id: 't1', events: [{ chat_id: '', seq: 4, kind: 'message', turn_id: 'u1', message_id: 'a2', text: 'Found it.', at: '' }] } as never);
    expect(store.views()['t1'].transcript.items.filter((i) => i.type === 'assistant').length).toBe(2);
    // A replayed event is ignored.
    store.onEvents({ external_id: 's1', task_id: 't1', events: [{ chat_id: '', seq: 2, kind: 'message', text: 'dup', at: '' }] } as never);
    expect(store.views()['t1'].transcript.items.length).toBe(4);
  });

  it('says why when the machine cannot provide it', async () => {
    const { store } = setup({ external_id: 's1', available: false, reason: 'box isn’t connected' });
    await store.load('t1');
    expect(store.views()['t1']).toMatchObject({ available: false, reason: 'box isn’t connected', loading: false });
    // Pushes for a tab without a transcript are ignored.
    store.onEvents({ external_id: 's1', task_id: 't1', events: [{ chat_id: '', seq: 1, kind: 'message', text: 'x', at: '' }] } as never);
    expect(store.views()['t1'].transcript.items.length).toBe(0);
  });
});

import { beforeEach, describe, expect, it } from 'vitest';
import { FIXTURE_CHAT_ID, ev, resetSeq, sampleTurn } from '../../../testing/agent-chat.fixtures';
import {
  AssistantItem,
  ElicitationItem,
  ErrorItem,
  ThoughtItem,
  ToolItem,
  TranscriptState,
  TurnEndItem,
  addPendingUser,
  applyEvent,
  applyEvents,
  emptyTranscript,
  endsWithQuestion,
  markAnswering,
  pendingElicitations,
  removePendingUser,
} from './chat-transcript';

function run(events = sampleTurn()): TranscriptState {
  return applyEvents(emptyTranscript(FIXTURE_CHAT_ID), events);
}

function ofType<T>(s: TranscriptState, type: string): T[] {
  return s.items.filter((i) => i.type === type) as T[];
}

describe('transcript reducer', () => {
  beforeEach(() => resetSeq());

  it('builds a full turn: user, thinking, tool card, plan, final message, usage at the end', () => {
    const s = run();
    expect(s.items.map((i) => i.type)).toEqual(['user', 'thought', 'tool', 'plan', 'assistant', 'turn_end']);
    expect(s.model).toBe('sonnet');
    expect(s.agentSessionId).toBe('sess-abc');
    expect(s.status).toBe('done');
    expect(s.lastSeq).toBe(14);
    const end = ofType<TurnEndItem>(s, 'turn_end')[0];
    expect(end.usage).toEqual({ input_tokens: 1200, output_tokens: 340, cache_read_tokens: 9000, cost_usd: 0.012 });
    expect(end.repliedWithQuestion).toBe(true);
  });

  it('streams message_delta text and replaces it with the final message', () => {
    const events = sampleTurn();
    let s = emptyTranscript(FIXTURE_CHAT_ID);
    s = applyEvents(s, events.slice(0, 10)); // through both deltas
    const live = ofType<AssistantItem>(s, 'assistant')[0];
    expect(live.text).toBe('All tests pass.');
    expect(live.streaming).toBe(true);
    expect(s.status).toBe('working');
    s = applyEvent(s, events[10]);
    const final = ofType<AssistantItem>(s, 'assistant');
    expect(final).toHaveLength(1);
    expect(final[0].text).toBe('All tests pass. Want me to commit?');
    expect(final[0].streaming).toBe(false);
  });

  it('coalesces thought_delta into one collapsible thought that closes when output starts', () => {
    const events = sampleTurn();
    let s = applyEvents(emptyTranscript(FIXTURE_CHAT_ID), events.slice(0, 5));
    let th = ofType<ThoughtItem>(s, 'thought');
    expect(th).toHaveLength(1);
    expect(th[0].text).toBe('Let me look at the test suite.');
    expect(th[0].streaming).toBe(true);
    s = applyEvent(s, events[5]); // tool call
    th = ofType<ThoughtItem>(s, 'thought');
    expect(th[0].streaming).toBe(false);
  });

  it('updates a tool card in place by id and keeps earlier fields', () => {
    const events = sampleTurn();
    let s = applyEvents(emptyTranscript(FIXTURE_CHAT_ID), events.slice(0, 6));
    expect(s.subtitle).toBe('Run: go test ./...');
    expect(ofType<ToolItem>(s, 'tool')[0].call.status).toBe('running');
    s = applyEvent(s, events[6]);
    const tools = ofType<ToolItem>(s, 'tool');
    expect(tools).toHaveLength(1);
    expect(tools[0].call).toMatchObject({
      id: 'tc1',
      kind: 'execute',
      title: 'Run: go test ./...',
      status: 'completed',
      input: '{"command":"go test ./..."}',
      output: 'ok  \tkonsert/internal\t0.42s',
    });
  });

  it('replaces the plan of a turn instead of appending', () => {
    let s = run();
    s = applyEvent(s, ev('plan', { plan: [{ text: 'Run tests', status: 'completed' }, { text: 'Fix failures', status: 'completed' }] }));
    const plans = s.items.filter((i) => i.type === 'plan');
    expect(plans).toHaveLength(1);
  });

  it('ignores replayed events (seq <= lastSeq) after reconnect catch-up', () => {
    const events = sampleTurn();
    const once = run(events);
    const twice = applyEvents(once, events);
    expect(twice).toBe(once);
  });

  it('keeps an optimistic user message until the hub echoes it, and can roll it back', () => {
    let s = addPendingUser(emptyTranscript(FIXTURE_CHAT_ID), 't9', 'hello');
    expect(s.status).toBe('working');
    expect(s.items[0]).toMatchObject({ type: 'user', pending: true, text: 'hello' });
    expect(removePendingUser(s, 't9').items).toHaveLength(0);
    s = applyEvent(s, ev('user_message', { turn_id: 't9', text: 'hello' }));
    expect(s.items).toHaveLength(1);
    expect(s.items[0]).toMatchObject({ type: 'user', text: 'hello' });
    expect((s.items[0] as { pending?: boolean }).pending).toBeUndefined();
  });

  it('replaces the optimistic prompt even when the hub assigned a different turn id', () => {
    // Seen live: the prompt appeared twice because the echoed user_message had another turn id.
    let s = addPendingUser(emptyTranscript(FIXTURE_CHAT_ID), 'client-turn', 'what is the capital of France?');
    s = applyEvent(s, ev('user_message', { turn_id: 'hub-turn', text: 'what is the capital of France?' }));
    expect(s.items.filter((i) => i.type === 'user')).toHaveLength(1);
  });

  it('splits legacy attachment trailers into file chips', () => {
    const s = applyEvent(
      emptyTranscript(FIXTURE_CHAT_ID),
      ev('user_message', { text: 'see file\n\n[Attached files on host]\n- /tmp/x/a.png' }),
    );
    expect(s.items[0]).toMatchObject({ text: 'see file', files: ['/tmp/x/a.png'] });
  });

  describe('elicitation lifecycle', () => {
    const permission = () =>
      ev('elicitation', {
        elicitation: {
          id: 'el1',
          kind: 'permission',
          title: 'Run rm -rf build?',
          tool_call: { id: 'tc9', name: 'Bash', kind: 'execute', title: 'Run: rm -rf build' },
          options: [
            { id: 'allow_once', label: 'Allow once', kind: 'allow_once' },
            { id: 'allow_always', label: 'Always allow', kind: 'allow_always' },
            { id: 'reject_once', label: 'Deny', kind: 'reject_once' },
          ],
        },
      });

    it('pending -> needs_you, answering, resolved -> working', () => {
      let s = applyEvents(emptyTranscript(FIXTURE_CHAT_ID), [ev('status', { status: 'working' }), permission()]);
      expect(s.status).toBe('needs_you');
      expect(pendingElicitations(s)).toHaveLength(1);
      s = markAnswering(s, 'el1', 'Allow once');
      const el = ofType<ElicitationItem>(s, 'elicitation')[0];
      expect(el.state).toBe('answering');
      expect(el.answer).toBe('Allow once');
      s = applyEvent(s, ev('elicitation_resolved', { elicitation: { id: 'el1', kind: 'permission', title: '' } }));
      expect(ofType<ElicitationItem>(s, 'elicitation')[0].state).toBe('resolved');
      expect(s.status).toBe('working');
      expect(pendingElicitations(s)).toHaveLength(0);
    });

    it('expires a still-pending elicitation when its turn ends', () => {
      let s = applyEvents(emptyTranscript(FIXTURE_CHAT_ID), [permission()]);
      s = applyEvent(s, ev('turn_end', { stop_reason: 'cancelled' }));
      expect(ofType<ElicitationItem>(s, 'elicitation')[0].state).toBe('expired');
      expect(s.status).toBe('cancelled');
    });
  });

  describe('errors', () => {
    const kinds: Array<[string, Record<string, unknown>]> = [
      ['usage_limit', { message: 'limit reached', window: 'seven_day', resets_at: '2026-11-25T23:40:00Z' }],
      ['auth', { message: 'OAuth token has expired', relogin: { command: 'claude /login', env: { CLAUDE_CONFIG_DIR: '~/.claude2' } } }],
      ['exit', { message: 'claude exited', exit_code: 1, stderr: 'boom' }],
      ['rate_limit', { message: 'overloaded', retryable: true }],
      ['not_installed', { message: 'grok not found' }],
      ['model', { message: 'unknown model' }],
      ['protocol', { message: 'bad json' }],
      ['other', { message: 'something' }],
    ];
    for (const [kind, extra] of kinds) {
      it(`a ${kind} error fails the turn and adds an error card`, () => {
        let s = applyEvents(emptyTranscript(FIXTURE_CHAT_ID), [
          ev('status', { status: 'working' }),
          ev('message_delta', { message_id: 'm1', text: 'partial' }),
        ]);
        s = applyEvent(s, ev('error', { error: { kind, ...extra } as never }));
        expect(s.status).toBe('failed');
        const errs = ofType<ErrorItem>(s, 'error');
        expect(errs).toHaveLength(1);
        expect(errs[0].error.kind).toBe(kind);
        expect(ofType<AssistantItem>(s, 'assistant')[0].streaming).toBe(false);
      });
    }

    it('a timeout keeps the turn working (the engine is still alive)', () => {
      let s = applyEvents(emptyTranscript(FIXTURE_CHAT_ID), [ev('status', { status: 'working' })]);
      s = applyEvent(s, ev('error', { error: { kind: 'timeout', message: 'no progress for 10m' } }));
      expect(s.status).toBe('working');
      expect(ofType<ErrorItem>(s, 'error')[0].error.kind).toBe('timeout');
    });
  });

  it('usage that arrives after turn_end attaches to the turn', () => {
    let s = applyEvents(emptyTranscript(FIXTURE_CHAT_ID), [ev('message', { message_id: 'm', text: 'ok.' }), ev('turn_end', { stop_reason: 'end_turn' })]);
    s = applyEvent(s, ev('usage', { usage: { input_tokens: 5, output_tokens: 6 } }));
    expect(ofType<TurnEndItem>(s, 'turn_end')[0].usage).toEqual({ input_tokens: 5, output_tokens: 6 });
    expect(ofType<TurnEndItem>(s, 'turn_end')[0].repliedWithQuestion).toBe(false);
  });

  it('records rate_limit state from the stream', () => {
    const s = applyEvent(emptyTranscript(FIXTURE_CHAT_ID), ev('rate_limit', { rate_limit: { window: 'five_hour', used_percent: 85, state: 'warning' } }));
    expect(s.rateLimit?.used_percent).toBe(85);
  });

  it('detects questions through trailing markdown', () => {
    expect(endsWithQuestion('Should I commit?')).toBe(true);
    expect(endsWithQuestion('Should I **commit?**')).toBe(true);
    expect(endsWithQuestion('Done.')).toBe(false);
  });
});

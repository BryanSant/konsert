import { Component, signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FIXTURE_CHAT_ID, claudeAccount, ev, resetSeq, sampleTurn } from '../../../../testing/agent-chat.fixtures';
import { AgentAccount, AgentChatError, ChatStatus } from '../../../core/models/agent-chat.models';
import { ApiClientService } from '../../../core/services/api-client.service';
import { ElicitationItem, TranscriptItem, applyEvents, emptyTranscript } from '../../../core/state/chat-transcript';
import { StatusPillComponent, statusLook } from '../status-pill.component';
import { ChatTranscriptComponent, usageLine } from './chat-transcript.component';
import { ElicitationAnswer } from './elicitation-card.component';
import { errorCardView } from './error-card.component';

const NOW = new Date(2026, 10, 23, 12, 0);

describe('errorCardView', () => {
  const view = (e: Partial<AgentChatError>) =>
    errorCardView({ message: '', ...e } as AgentChatError, { agent: 'claude', accountLabel: 'Claude 2', now: NOW });

  it('names the usage window and local reset time', () => {
    const tue = new Date(2026, 10, 24, 15, 40).toISOString();
    expect(view({ kind: 'usage_limit', window: 'five_hour', resets_at: new Date(2026, 10, 23, 15, 40).toISOString() }).title).toMatch(
      /^Out of 5-hour usage until 3:40/,
    );
    expect(view({ kind: 'usage_limit', window: 'seven_day', resets_at: tue }).title).toMatch(/^Out of weekly usage until Tue 3:40/);
    expect(view({ kind: 'usage_limit', window: 'seven_day_opus', resets_at: tue }).title).toMatch(/^Out of weekly Opus usage/);
    expect(view({ kind: 'usage_limit', window: 'monthly' }).title).toBe('Out of monthly usage');
    expect(view({ kind: 'usage_limit', window: 'daily' }).title).toBe('Out of daily usage');
  });

  it('has a headline for every kind', () => {
    expect(view({ kind: 'auth' }).title).toBe('Claude 2 needs you to sign in again');
    expect(view({ kind: 'exit', exit_code: 1 }).title).toBe('claude exited with code 1');
    expect(view({ kind: 'exit' }).title).toBe('claude exited');
    expect(view({ kind: 'timeout' }).title).toBe('No progress for a while');
    expect(view({ kind: 'rate_limit' }).title).toBe('The service is busy');
    expect(view({ kind: 'not_installed' }).title).toBe('claude is not installed on this machine');
    expect(view({ kind: 'model' }).title).toBe('That model is not available');
    expect(view({ kind: 'protocol' }).title).toContain('could not read');
    expect(view({ kind: 'other' }).title).toBe('Something went wrong');
  });
});

describe('status pill', () => {
  it('maps every status to a label and tone', () => {
    expect(statusLook('starting').label).toBe('Starting');
    expect(statusLook('working').label).toBe('Working');
    expect(statusLook('needs_you').label).toBe('Needs you');
    expect(statusLook('needs_you').cls).toContain('amber');
    expect(statusLook('done').label).toBe('Done');
    expect(statusLook('failed').label).toBe('Failed');
    expect(statusLook('failed').cls).toContain('red');
    expect(statusLook('cancelled').label).toBe('Cancelled');
    expect(statusLook('idle').label).toBe('Ready');
  });

  it('shows the latest tool title as subtitle only while working', () => {
    const f = TestBed.createComponent(StatusPillComponent);
    f.componentRef.setInput('status', 'working');
    f.componentRef.setInput('subtitle', 'Run: go test ./...');
    f.detectChanges();
    expect(f.nativeElement.textContent).toContain('Working');
    expect(f.nativeElement.textContent).toContain('Run: go test ./...');
    f.componentRef.setInput('status', 'done');
    f.detectChanges();
    expect(f.nativeElement.textContent).not.toContain('go test');
    TestBed.resetTestingModule();
  });
});

@Component({
  standalone: true,
  imports: [ChatTranscriptComponent],
  template: `
    <ks-chat-transcript
      style="height: 300px"
      [items]="items()"
      [status]="status()"
      agent="claude"
      accountLabel="Claude 2"
      workerId="w1"
      accountId="claude2"
      [alternatives]="alternatives()"
      (answer)="answers.push($event)"
      (retry)="retries = retries + 1"
      (switchAccount)="switched.push($event.id)"
      (keepWaiting)="waits.push($event)"
      (stop)="stops = stops + 1"
    />
  `,
})
class Host {
  readonly items = signal<TranscriptItem[]>([]);
  readonly status = signal<ChatStatus>('idle');
  readonly alternatives = signal<AgentAccount[]>([]);
  answers: ElicitationAnswer[] = [];
  retries = 0;
  switched: string[] = [];
  waits: string[] = [];
  stops = 0;
}

describe('ChatTranscriptComponent', () => {
  beforeEach(() => {
    resetSeq();
    TestBed.configureTestingModule({
      providers: [{ provide: ApiClientService, useValue: { setUnauthorizedHandler: vi.fn(), createSession: vi.fn() } }],
    });
  });
  afterEach(() => TestBed.resetTestingModule());

  async function setup(events = sampleTurn()) {
    const fixture = TestBed.createComponent(Host);
    document.body.appendChild(fixture.nativeElement);
    const state = applyEvents(emptyTranscript(FIXTURE_CHAT_ID), events);
    fixture.componentInstance.items.set(state.items);
    fixture.componentInstance.status.set(state.status);
    fixture.detectChanges();
    await fixture.whenStable();
    const el = fixture.nativeElement as HTMLElement;
    const render = async () => {
      fixture.detectChanges();
      await fixture.whenStable();
    };
    return { fixture, el, host: fixture.componentInstance, render, state };
  }

  it('renders a turn: bubble, collapsed thinking, tool card, plan, message, question highlight, usage', async () => {
    const { el, render } = await setup();
    expect(el.querySelector('[data-testid="user-bubble"]')!.textContent).toContain('Run the tests and fix failures');
    const thought = Array.from(el.querySelectorAll('button')).find((b) => b.textContent?.includes('Thought'))!;
    expect(thought.getAttribute('aria-expanded')).toBe('false');
    thought.click();
    await render();
    expect(el.textContent).toContain('Let me look at the test suite.');
    const tool = el.querySelector('[data-tool-status]')!;
    expect(tool.getAttribute('data-tool-status')).toBe('completed');
    expect(tool.textContent).toContain('Run: go test ./...');
    expect(el.querySelector('[data-testid="plan"]')!.textContent).toContain('Fix failures');
    expect(el.querySelector('[data-testid="assistant-message"]')!.textContent).toContain('Want me to commit?');
    expect(el.querySelector('[data-testid="replied-question"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="usage"]')!.textContent).toContain('1.2k in · 340 out · 9.0k cached');
  });

  it('expands a tool card to show input and output', async () => {
    const { el, render } = await setup();
    const btn = el.querySelector('[data-tool-status] button') as HTMLButtonElement;
    btn.click();
    await render();
    expect(el.textContent).toContain('"command": "go test ./..."');
    expect(el.textContent).toContain('konsert/internal');
  });

  it('puts a permission request at the bottom with its options and answers it', async () => {
    const { el, host, render } = await setup([
      ev('user_message', { text: 'clean up' }),
      ev('elicitation', {
        elicitation: {
          id: 'el1',
          kind: 'permission',
          title: 'Allow Bash to run rm -rf build?',
          tool_call: { id: 'tc9', name: 'Bash', kind: 'execute', title: 'Run: rm -rf build' },
          options: [
            { id: 'allow_once', label: 'Allow once', kind: 'allow_once' },
            { id: 'allow_always', label: 'Always allow', kind: 'allow_always' },
            { id: 'reject_once', label: 'Deny', kind: 'reject_once' },
          ],
        },
      }),
      ev('message_delta', { message_id: 'm2', text: 'later text' }),
    ]);
    const cards = el.querySelectorAll('[data-elicitation-kind="permission"]');
    expect(cards).toHaveLength(1);
    // Rendered after the flowing items.
    const all = Array.from(el.querySelectorAll('[data-testid="assistant-message"], [data-elicitation-kind]'));
    expect(all.at(-1)!.getAttribute('data-elicitation-kind')).toBe('permission');
    expect(cards[0].textContent).toContain('Run: rm -rf build');
    const deny = cards[0].querySelector('[data-option="reject_once"]') as HTMLButtonElement;
    expect(deny.classList.contains('ks-btn-danger')).toBe(true);
    (cards[0].querySelector('[data-option="allow_once"]') as HTMLButtonElement).click();
    expect(host.answers).toEqual([{ elicitationId: 'el1', optionId: 'allow_once', label: 'Allow once' }]);

    host.items.update((items) =>
      items.map((i) => (i.type === 'elicitation' ? ({ ...i, state: 'resolved', answer: 'Allow once' } as ElicitationItem) : i)),
    );
    await render();
    expect(el.querySelector('[data-elicitation-state="resolved"]')!.textContent).toContain('Allow once');
  });

  it('answers a question with free text', async () => {
    const { el, host, render } = await setup([
      ev('elicitation', {
        elicitation: {
          id: 'q1',
          kind: 'question',
          title: 'Which database?',
          options: [
            { id: 'pg', label: 'Postgres', kind: 'choice' },
            { id: 'sqlite', label: 'SQLite', kind: 'choice' },
          ],
          allow_text: true,
        },
      }),
    ]);
    const input = el.querySelector('[data-testid="elicitation-text"]') as HTMLInputElement;
    input.value = 'MySQL please';
    input.dispatchEvent(new Event('input'));
    await render();
    (input.closest('form') as HTMLFormElement).dispatchEvent(new Event('submit'));
    expect(host.answers).toEqual([{ elicitationId: 'q1', text: 'MySQL please', label: 'MySQL please' }]);
  });

  it('shows the sign-in card with the relogin command for auth elicitations', async () => {
    const { el } = await setup([
      ev('elicitation', { elicitation: { id: 'a1', kind: 'auth', title: 'Sign in' } }),
    ]);
    // Relogin comes from capabilities when the elicitation has none.
    expect(el.querySelector('[data-elicitation-kind="auth"]')!.textContent).toContain('Claude 2 needs you to sign in again');
    expect(el.textContent).toContain('Open terminal on');
  });

  it('renders error cards by kind with their actions', async () => {
    const tue = new Date(Date.now() + 2 * 86_400_000).toISOString();
    const { el, host, render } = await setup([
      ev('user_message', { text: 'go' }),
      ev('error', { error: { kind: 'usage_limit', message: 'limit reached', window: 'seven_day', resets_at: tue } }),
    ]);
    host.alternatives.set([claudeAccount()]);
    await render();
    const card = el.querySelector('[data-error-kind="usage_limit"]')!;
    expect(card.querySelector('[data-testid="error-title"]')!.textContent).toMatch(/Out of weekly usage until/);
    expect(card.querySelector('[data-testid="error-reset"]')!.textContent).toMatch(/Resets in (1d|2d)/);
    (card.querySelector('[data-switch="claude"]') as HTMLButtonElement).click();
    expect(host.switched).toEqual(['claude']);

    resetSeq(10);
    host.items.set(
      applyEvents(emptyTranscript(FIXTURE_CHAT_ID), [
        ev('error', { error: { kind: 'exit', message: 'process died', exit_code: 1, stderr: 'panic: boom' } }),
        ev('error', { error: { kind: 'timeout', message: 'no events for 10 minutes' } }),
        ev('error', { error: { kind: 'rate_limit', message: 'overloaded', retryable: true } }),
        ev('error', {
          error: { kind: 'auth', message: 'OAuth token has expired', relogin: { command: 'claude /login', env: { CLAUDE_CONFIG_DIR: '~/.claude2' } } },
        }),
      ]).items,
    );
    await render();
    const exit = el.querySelector('[data-error-kind="exit"]')!;
    expect(exit.textContent).toContain('claude exited with code 1');
    expect(exit.querySelector('[data-testid="error-stderr"]')).toBeNull();
    (Array.from(exit.querySelectorAll('button')).find((b) => b.textContent?.includes('error output')) as HTMLButtonElement).click();
    await render();
    expect(exit.querySelector('[data-testid="error-stderr"]')!.textContent).toBe('panic: boom');

    const timeout = el.querySelector('[data-error-kind="timeout"]')!;
    const btn = (label: string, root: Element) =>
      Array.from(root.querySelectorAll('button')).find((b) => b.textContent?.trim() === label) as HTMLButtonElement;
    btn('Stop', timeout).click();
    btn('Keep waiting', timeout).click();
    expect(host.stops).toBe(1);
    expect(host.waits).toHaveLength(1);

    btn('Retry', el.querySelector('[data-error-kind="rate_limit"]')!).click();
    expect(host.retries).toBe(1);

    const auth = el.querySelector('[data-error-kind="auth"]')!;
    expect(auth.textContent).toContain('Claude 2 needs you to sign in again');
    expect(auth.querySelector('[data-testid="relogin-command"]')!.textContent).toBe('CLAUDE_CONFIG_DIR=~/.claude2 claude /login');
  });

  it('formats usage compactly', () => {
    expect(usageLine({ input_tokens: 1200, output_tokens: 340 })).toBe('1.2k in · 340 out');
    expect(usageLine({ input_tokens: 2_500_000, output_tokens: 1, cost_usd: 1.234 })).toBe('2.5M in · 1 out · $1.23');
  });

  it('stops following when the user scrolls up and offers Jump to latest', async () => {
    const { el, render } = await setup();
    const scroller = el.querySelector('[role="log"]') as HTMLElement;
    Object.defineProperty(scroller, 'scrollHeight', { value: 2000, configurable: true });
    Object.defineProperty(scroller, 'clientHeight', { value: 300, configurable: true });
    expect(el.querySelector('[data-testid="jump-latest"]')).toBeNull();
    scroller.scrollTop = 100;
    scroller.dispatchEvent(new Event('scroll'));
    await render();
    const jump = el.querySelector('[data-testid="jump-latest"]') as HTMLButtonElement;
    expect(jump).toBeTruthy();
    jump.click();
    await render();
    expect(scroller.scrollTop).toBe(2000);
    expect(el.querySelector('[data-testid="jump-latest"]')).toBeNull();
  });
});

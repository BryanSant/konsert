import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { QuestionView } from '../../core/models/question.models';
import { ApiClientService } from '../../core/services/api-client.service';
import {
  formatSince,
  groupQuestions,
  moveIndex,
  queueKey,
  sentMessage,
  sortQuestions,
} from '../../core/state/questions-logic';
import { QuestionsStore } from '../../core/state/questions.store';
import { QuestionCardComponent } from './question-card.component';
import { NeedsYouDialogComponent } from '../needs-you/needs-you-dialog.component';
import { SideConversationComponent } from './side-conversation.component';

const NOW = Date.parse('2026-11-23T12:00:00Z');

function q(over: Partial<QuestionView> & { id: string }): QuestionView {
  return {
    sid: `q-${over.id}`,
    chat_id: 'c1',
    kind: 'question',
    source: 'ask_user',
    prompt: `Question ${over.id}?`,
    allow_text: true,
    status: 'open',
    blocked_since: new Date(NOW - 60_000).toISOString(),
    ask_count: 1,
    created_at: new Date(NOW - 60_000).toISOString(),
    updated_at: new Date(NOW - 60_000).toISOString(),
    impact: 1,
    messages: [],
    options: [
      { id: 'option_1', label: 'Yes', consequence: 'Closes today', recommended: true },
      { id: 'option_2', label: 'No, keep the strict exit criteria', consequence: 'Stays open' },
    ],
    rationale: 'The e2e covers it',
    ...over,
  };
}

async function settle(fixture: { detectChanges(): void; whenStable(): Promise<unknown> }): Promise<void> {
  fixture.detectChanges();
  await fixture.whenStable();
  await new Promise((r) => setTimeout(r, 0));
  fixture.detectChanges();
}

function byTestId(root: HTMLElement, id: string): HTMLElement | null {
  return root.querySelector(`[data-testid="${id}"]`);
}

describe('questions logic', () => {
  it('orders by blocking impact, then age; snoozed and closed last', () => {
    const list = [
      q({ id: 'a', impact: 1, blocked_since: new Date(NOW - 30 * 60_000).toISOString() }),
      q({ id: 'b', impact: 4 }),
      q({ id: 'c', impact: 1, blocked_since: new Date(NOW - 60 * 60_000).toISOString() }),
      q({ id: 'd', impact: 9, snoozed_until: new Date(NOW + 60_000).toISOString() }),
      q({ id: 'e', impact: 9, status: 'answered' }),
    ];
    expect(sortQuestions(list, NOW).map((x) => x.id)).toEqual(['b', 'c', 'a', 'd', 'e']);
  });

  it('groups by ticket in queue order', () => {
    const sorted = [
      q({ id: 'a', group_key: 'ticket:KON-2Q0CH', group_label: 'KON-2Q0CH Questions and steering' }),
      q({ id: 'b', group_key: 'chat:c2', chat_title: 'Fix e2e' }),
      q({ id: 'c', group_key: 'ticket:KON-2Q0CH' }),
    ];
    const groups = groupQuestions(sorted);
    expect(groups.map((g) => [g.label, g.items.map((x) => x.id)])).toEqual([
      ['KON-2Q0CH Questions and steering', ['a', 'c']],
      ['Fix e2e', ['b']],
    ]);
  });

  it('maps queue keys and ignores modifiers', () => {
    expect(queueKey({ key: 'j', metaKey: false, ctrlKey: false, altKey: false })).toEqual({ kind: 'move', delta: 1 });
    expect(queueKey({ key: 'k', metaKey: false, ctrlKey: false, altKey: false })).toEqual({ kind: 'move', delta: -1 });
    expect(queueKey({ key: '3', metaKey: false, ctrlKey: false, altKey: false })).toEqual({ kind: 'pick', index: 2 });
    expect(queueKey({ key: '?', metaKey: false, ctrlKey: false, altKey: false })).toEqual({ kind: 'ask' });
    expect(queueKey({ key: 'o', metaKey: false, ctrlKey: false, altKey: false })).toEqual({ kind: 'delegate' });
    expect(queueKey({ key: 'j', metaKey: true, ctrlKey: false, altKey: false })).toBeNull();
    expect(queueKey({ key: '0', metaKey: false, ctrlKey: false, altKey: false })).toBeNull();
    expect(moveIndex(-1, 1, 3)).toBe(0);
    expect(moveIndex(2, 1, 3)).toBe(2);
    expect(moveIndex(0, -1, 3)).toBe(0);
  });

  it('formats blocked time and receipts', () => {
    expect(formatSince(new Date(NOW - 45_000).toISOString(), NOW)).toBe('45s');
    expect(formatSince(new Date(NOW - 12 * 60_000).toISOString(), NOW)).toBe('12m');
    expect(formatSince(new Date(NOW - 125 * 60_000).toISOString(), NOW)).toBe('2h 5m');
    const at = new Date(NOW).toISOString();
    expect(sentMessage(q({ id: 'a', status: 'answered', answer: { label: 'Yes', summary: 's', by: { actor: 'user' }, at, delivered_at: at, delivery: 'tool' } }))).toBe(
      'Sent “Yes” with a summary of your side conversation',
    );
    expect(sentMessage(q({ id: 'a', status: 'answered', answer: { text: 'Friday', by: { actor: 'user' }, at, delivered_at: at, delivery: 'note' } }))).toBe(
      'Sent “Friday” (as a note before its next turn)',
    );
    expect(sentMessage(q({ id: 'a', status: 'answered', answer: { label: 'Yes', by: { actor: 'user' }, at, preparing: true } }))).toContain('writing a summary');
  });
});

describe('questions UI', () => {
  let api: Record<string, ReturnType<typeof vi.fn>>;
  let store: QuestionsStore;

  beforeEach(() => {
    api = {
      listQuestions: vi.fn(async () => ({ questions: store.pending() })),
      answerQuestion: vi.fn(async (id: string, body: { option_id?: string; text?: string }) => {
        const cur = store.question(id)!;
        const opt = cur.options?.find((o) => o.id === body.option_id);
        return { ...cur, status: 'answered', answer: { option_id: body.option_id, label: opt?.label, text: body.text, by: { actor: 'user' }, at: new Date().toISOString(), delivered_at: new Date().toISOString(), delivery: 'tool' } };
      }),
      askAboutQuestion: vi.fn(async (id: string, text: string) => {
        const cur = store.question(id)!;
        return { ...cur, thread: { chat_id: 'fork', answerer: 'fork', status: 'working', started_at: '' }, messages: [{ question_id: id, seq: 1, role: 'user', text, created_at: '' }] };
      }),
      delegateQuestion: vi.fn(async (id: string) => ({ ...store.question(id)!, status: 'delegated', delegation: { state: 'pending', requested_at: '', requested_by: { actor: 'user' } } })),
      snoozeQuestion: vi.fn(async (id: string) => store.question(id)!),
    };
    TestBed.configureTestingModule({ providers: [{ provide: ApiClientService, useValue: api }] });
    store = TestBed.inject(QuestionsStore);
  });

  afterEach(() => {
    store.close();
    TestBed.resetTestingModule();
  });

  it('store applies hub updates, keeps answered ones with their receipt, and counts pending', async () => {
    store.apply(q({ id: 'a' }));
    store.apply(q({ id: 'b', impact: 3 }));
    expect(store.pendingCount()).toBe(2);
    expect(store.pending()[0].id).toBe('b');
    await store.answer(store.question('a')!, { option_id: 'option_2' });
    expect(api['answerQuestion']).toHaveBeenCalledWith('a', { option_id: 'option_2' });
    expect(store.pendingCount()).toBe(1);
    expect(store.visible().map((x) => x.id)).toContain('a'); // receipt stays
    store.apply({ ...store.question('b')!, status: 'expired' });
    expect(store.pendingCount()).toBe(0);
  });

  it('card shows context, consequences and the recommendation, and answers with an option or free text', async () => {
    store.apply(q({ id: 'a', context: 'KON-2Q0CH needs a phone run', why_now: 'Closing waits on it', refs: [{ kind: 'ticket', ref: 'KON-2Q0CH', title: 'Questions and steering', status: 'in_progress', excerpt: 'Clear questions', resolved: true }] }));
    const fixture = TestBed.createComponent(QuestionCardComponent);
    fixture.componentRef.setInput('question', store.question('a')!);
    await settle(fixture);
    const root = fixture.nativeElement as HTMLElement;
    expect(byTestId(root, 'question-context')!.textContent).toContain('Closing waits on it');
    expect(byTestId(root, 'question-refs')!.textContent).toContain('Questions and steering');
    const yes = byTestId(root, 'question-option-option_1')!;
    expect(yes.textContent).toContain('Recommended');
    expect(yes.textContent).toContain('Closes today');
    expect(yes.textContent).toContain('Why: The e2e covers it');

    byTestId(root, 'question-other')!.click();
    await settle(fixture);
    const input = byTestId(root, 'question-other-input') as HTMLInputElement;
    input.value = 'Do the phone run Friday';
    input.dispatchEvent(new Event('input'));
    await settle(fixture);
    byTestId(root, 'question-other-send')!.click();
    await settle(fixture);
    expect(api['answerQuestion']).toHaveBeenCalledWith('a', { text: 'Do the phone run Friday' });
    fixture.componentRef.setInput('question', store.question('a')!);
    await settle(fixture);
    expect(byTestId(root, 'question-receipt')!.textContent).toContain('Sent “Do the phone run Friday”');
  });

  it('panel keyboard: j/k move, 1–9 answer, ? opens the side conversation, o delegates', async () => {
    store.apply(q({ id: 'a', impact: 5 }));
    store.apply(q({ id: 'b', impact: 3 }));
    store.apply(q({ id: 'c', impact: 1 }));
    store.open();
    const fixture = TestBed.createComponent(NeedsYouDialogComponent);
    await settle(fixture);
    const key = (k: string) => document.dispatchEvent(new KeyboardEvent('keydown', { key: k, bubbles: true }));
    expect(store.selectedId()).toBe('a');
    key('j');
    expect(store.selectedId()).toBe('b');
    key('k');
    key('k');
    expect(store.selectedId()).toBe('a');
    key('?');
    await settle(fixture);
    expect(store.threadOpen()).toBe(true);
    expect(byTestId(fixture.nativeElement as HTMLElement, 'side-conversation')).not.toBeNull();
    key('Escape');
    expect(store.threadOpen()).toBe(false);
    key('2');
    await settle(fixture);
    expect(api['answerQuestion']).toHaveBeenCalledWith('a', { option_id: 'option_2' });
    key('j');
    expect(store.selectedId()).toBe('b');
    key('o');
    await settle(fixture);
    expect(api['delegateQuestion']).toHaveBeenCalledWith('b');
    // Keys typed into a field don't act on the queue.
    const input = document.createElement('input');
    document.body.appendChild(input);
    input.dispatchEvent(new KeyboardEvent('keydown', { key: '1', bubbles: true }));
    expect(api['answerQuestion']).toHaveBeenCalledTimes(1);
    input.remove();
  });

  it('side conversation asks, shows the draft, keeps the original options pinned and answerable', async () => {
    store.apply(q({ id: 'a' }));
    const fixture = TestBed.createComponent(SideConversationComponent);
    fixture.componentRef.setInput('question', store.question('a')!);
    await settle(fixture);
    const root = fixture.nativeElement as HTMLElement;
    const ta = byTestId(root, 'thread-input') as HTMLTextAreaElement;
    ta.value = 'What is KON-2Q0CH and why do you recommend Yes?';
    ta.dispatchEvent(new Event('input'));
    await settle(fixture);
    byTestId(root, 'thread-send')!.click();
    await settle(fixture);
    expect(api['askAboutQuestion']).toHaveBeenCalledWith('a', 'What is KON-2Q0CH and why do you recommend Yes?');

    store.apply({
      ...store.question('a')!,
      thread: { chat_id: 'fork', answerer: 'fork', status: 'idle', started_at: '', draft: { option_id: 'option_1', label: 'Yes' } },
      thread_account_label: 'Claude 2',
      machine_name: 'mac',
      messages: [
        { question_id: 'a', seq: 1, role: 'user', text: 'What is KON-2Q0CH?', created_at: '' },
        { question_id: 'a', seq: 2, role: 'answerer', text: 'It is the questions epic.', created_at: '' },
      ],
    });
    fixture.componentRef.setInput('question', store.question('a')!);
    await settle(fixture);
    expect(byTestId(root, 'thread-answerer')!.textContent).toContain('Claude 2 on mac');
    expect(root.querySelectorAll('[data-testid="thread-answer"]').length).toBe(1);
    expect(byTestId(root, 'thread-draft')!.textContent).toContain('Yes');
    expect(byTestId(root, 'thread-option-option_2')).not.toBeNull();
    byTestId(root, 'thread-use-draft')!.click();
    await settle(fixture);
    expect(api['answerQuestion']).toHaveBeenCalledWith('a', { option_id: 'option_1', text: undefined });
  });
});

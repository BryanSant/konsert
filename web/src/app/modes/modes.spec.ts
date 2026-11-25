import { TestBed } from '@angular/core/testing';
import { UrlSegment } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { modeMatcher } from '../app.routes';
import { Hold, OverrideRequest, Veto, formatDuration } from '../core/models/control.models';
import { ChatSession, Task, Worker } from '../core/models/konsert.models';
import { QuestionView } from '../core/models/question.models';
import { ApiClientService } from '../core/services/api-client.service';
import { ControlStore } from '../core/state/control.store';
import { FleetStore } from '../core/state/fleet.store';
import { NeedsYouStore, nextPendingKey, overrideScope, overrideTitle, vetoDetails, vetoEscalated } from '../core/state/needs-you.store';
import { OrchestratorStore } from '../core/state/orchestrator.store';
import { QuestionsStore } from '../core/state/questions.store';
import { WorkInputs, buildWorkItems, groupByProject, machineLoadLabel, occupiesSlot } from '../core/state/work-logic';
import { WorkStore } from '../core/state/work.store';
import { NeedsYouDialogComponent } from '../features/needs-you/needs-you-dialog.component';
import { NeedsYouItemComponent } from '../features/needs-you/needs-you-item.component';
import { modeForKey } from './mode-shell.component';
import { pageCount, pageOf, tilesPerPage, WallStore } from './wall/wall.store';
import { suggestTiles } from './wall/wall-mode.component';
import { receiptsFor } from './work/review-logic';

const NOW = Date.parse('2026-11-23T15:00:00');

const worker = (over: Partial<Worker> = {}): Worker =>
  ({
    id: 'w1',
    sid: 'm-w1',
    name: 'desk',
    host_group: '',
    os: 'linux',
    arch: 'amd64',
    isolation: 'host',
    permission_mode: 'full_access',
    agents: [],
    tags: {},
    max_concurrent: 2,
    status: 'online',
    last_seen_at: new Date(NOW).toISOString(),
    ...over,
  }) as Worker;

const task = (id: string, status: Task['status'], over: Partial<Task> = {}): Task =>
  ({
    id,
    sid: `t-${id}`,
    worker_id: 'w1',
    agent: 'claude',
    status,
    prompt: `Do ${id}\n- first\n- second`,
    workdir: '/src/web',
    effective_dir: '',
    origin: 'orchestrator',
    created_at: new Date(NOW - 3600_000).toISOString(),
    started_at: new Date(NOW - 3000_000).toISOString(),
    ...over,
  }) as Task;

const chat = (id: string, status: string, over: Partial<ChatSession> = {}): ChatSession =>
  ({ id, sid: `c-${id}`, title: `Chat ${id}`, provider: '', model: '', created_at: '', updated_at: new Date(NOW).toISOString(), backend: 'agent', agent: 'codex', worker_id: 'w1', status, ...over }) as ChatSession;

/** Lets chained promise continuations (store -> API mock -> component) run. */
async function flushMicrotasks(): Promise<void> {
  for (let i = 0; i < 20; i++) await Promise.resolve();
}

const veto = (id: string, over: Partial<Veto> = {}): Veto => ({
  id,
  sid: `v-${id}`,
  reason: 'wrong_machine',
  note: 'use CI',
  summary: `The user vetoed task t-${id} (claude on desk: "x") — reason: wrong machine`,
  task_id: id,
  fleet_event_id: 1,
  created_at: new Date(Date.now() - 60_000).toISOString(),
  status: 'sent',
  ack_deadline: new Date(Date.now() + 9 * 60_000).toISOString(),
  ...over,
});

const override = (id: string, over: Partial<OverrideRequest> = {}): OverrideRequest => ({
  id,
  sid: `o-${id}`,
  chat_id: 'c-orch',
  scope_kind: 'machine_slots',
  worker_id: 'w1',
  extra_slots: 1,
  duration_seconds: 1800,
  reason: 'The e2e suite must run next to the build',
  consequences: 'desk may run up to 3 sessions at once instead of 2 for 30m after you approve.',
  status: 'pending',
  created_at: new Date(Date.now() - 30_000).toISOString(),
  use_count: 0,
  ...over,
});

const question = (id: string, over: Partial<QuestionView> = {}): QuestionView =>
  ({
    id,
    sid: `q-${id}`,
    chat_id: 'c1',
    kind: 'question',
    source: 'ask_user',
    prompt: `Question ${id}?`,
    allow_text: true,
    status: 'open',
    blocked_since: new Date(NOW - 60_000).toISOString(),
    ask_count: 1,
    created_at: new Date(NOW - 60_000).toISOString(),
    updated_at: new Date(NOW - 60_000).toISOString(),
    impact: 1,
    messages: [],
    options: [{ id: 'o1', label: 'Yes' }, { id: 'o2', label: 'No' }],
    ...over,
  }) as QuestionView;

function inputs(over: Partial<WorkInputs> = {}): WorkInputs {
  return {
    tasks: [],
    chats: [],
    workers: { w1: worker() },
    projects: [],
    repos: [],
    questions: [],
    holds: [],
    interactive: () => false,
    reviewed: {},
    now: NOW,
    ...over,
  };
}

describe('work items (work-logic)', () => {
  it('puts tasks and agent chats in board columns with a control annunciator', () => {
    const items = buildWorkItems(
      inputs({
        tasks: [
          task('q', 'queued'),
          task('r', 'running'),
          task('w', 'waiting_input'),
          task('s', 'suspended'),
          task('h', 'running'),
          task('d', 'completed', { completed_at: new Date(NOW - 600_000).toISOString() }),
          task('v', 'completed', { completed_at: new Date(NOW - 600_000).toISOString(), git_summary_json: JSON.stringify({ dirty_files: ['?? a.go'] }) }),
          task('x', 'canceled', { completed_at: new Date(NOW - 60_000).toISOString() }),
          task('o', 'idle', { origin: 'external' }),
          task('scratch', 'running', { origin: 'scratch' }),
        ],
        chats: [chat('c1', 'working'), chat('c2', 'needs_you'), chat('orch', 'working', { fleet_tools: true })],
        holds: [{ id: 'h1', sid: 'h-1', scope_kind: 'session', task_id: 'h' } as Hold],
      }),
    );
    const by = Object.fromEntries(items.map((i) => [i.id, i]));
    expect(by['scratch']).toBeUndefined();
    expect(by['orch']).toBeUndefined(); // the orchestrator conversation has its own tab
    expect(by['q']).toMatchObject({ column: 'queued', control: 'QUEUED', live: true });
    expect(by['r']).toMatchObject({ column: 'running', control: 'ORCH', title: 'Do r', goal: 'Do r' });
    expect(by['w']).toMatchObject({ column: 'needs_you', control: 'ASKING' });
    expect(by['s']).toMatchObject({ column: 'running', control: 'SUSPENDED' });
    expect(by['h']).toMatchObject({ control: 'HELD' });
    expect(by['d']).toMatchObject({ column: 'done', control: 'DONE', live: false });
    expect(by['v']).toMatchObject({ column: 'review', hasChanges: true });
    expect(by['x']).toMatchObject({ column: 'done', control: 'STOPPED' });
    expect(by['o']).toMatchObject({ observed: true, live: false, column: null });
    expect(by['c1']).toMatchObject({ kind: 'chat', column: 'running', sessionKind: 'chat' });
    expect(by['c2']).toMatchObject({ column: 'needs_you', control: 'ASKING' });
  });

  it('marks YOU for Take Control, STALE when the machine stops reporting, and counts questions', () => {
    const items = buildWorkItems(
      inputs({
        tasks: [task('a', 'running'), task('b', 'running', { worker_id: 'w2' })],
        workers: { w1: worker(), w2: worker({ id: 'w2', name: 'mac', status: 'offline' }) },
        interactive: (id) => id === 'a',
        questions: [question('1', { task_id: 'a' })],
      }),
    );
    expect(items.find((i) => i.id === 'a')).toMatchObject({ control: 'YOU', questions: 1, column: 'needs_you' });
    expect(items.find((i) => i.id === 'b')).toMatchObject({ control: 'STALE', stale: true, machine: 'mac' });
  });

  it('an approved review moves to Done today', () => {
    const t = task('v', 'completed', { completed_at: new Date(NOW - 60_000).toISOString(), git_summary_json: JSON.stringify({ commits: ['abc fix'] }) });
    expect(buildWorkItems(inputs({ tasks: [t] }))[0].column).toBe('review');
    expect(buildWorkItems(inputs({ tasks: [t], reviewed: { v: 'approved' } }))[0].column).toBe('done');
  });

  it('groups live work by project, attention first', () => {
    const items = buildWorkItems(
      inputs({ tasks: [task('a', 'running'), task('b', 'waiting_input'), task('c', 'running', { workdir: '/src/api' })] }),
    );
    const groups = groupByProject(items.filter((i) => i.live));
    expect(groups.map((g) => g.name)).toEqual(['web', 'api']);
    expect(groups[0].items.map((i) => i.id)).toEqual(['b', 'a']);
  });

  it('labels machine load for the fleet strip and ignores observed sessions for slots', () => {
    expect(machineLoadLabel(worker(), 0).text).toBe('desk empty');
    expect(machineLoadLabel(worker(), 1).text).toBe('desk 1/2');
    expect(machineLoadLabel(worker(), 2)).toEqual({ text: 'desk FULL 2/2', tone: 'warn' });
    expect(machineLoadLabel(worker({ status: 'offline', last_seen_at: new Date(Date.now() - 240_000).toISOString() }), 0)).toMatchObject({ tone: 'stale' });
    expect(occupiesSlot({ status: 'running', origin: 'orchestrator' })).toBe(true);
    expect(occupiesSlot({ status: 'idle', origin: 'external' })).toBe(false);
  });
});

describe('needs-you queue', () => {
  it('walks to the next pending item after the current one, wrapping, or none', () => {
    expect(nextPendingKey(['a', 'b', 'c'], ['a', 'b', 'c'], 'a')).toBe('b');
    expect(nextPendingKey(['a', 'b', 'c'], ['a', 'c'], 'c')).toBe('a');
    expect(nextPendingKey(['a', 'b'], ['b', 'z'], 'b')).toBe('z');
    expect(nextPendingKey(['a'], ['a'], 'a')).toBeNull();
  });

  it('reads a veto\'s reason and knows when it went unacknowledged past its deadline', () => {
    expect(vetoDetails(veto('a'))).toEqual({ reason: 'Wrong machine', note: 'use CI' });
    expect(vetoEscalated(veto('a'))).toBe(false);
    expect(vetoEscalated(veto('a'), Date.now() + 10 * 60_000)).toBe(true);
    expect(vetoEscalated(veto('a', { escalated_at: new Date().toISOString() }))).toBe(true);
    expect(vetoEscalated(veto('a', { acked_at: new Date().toISOString() }), Date.now() + 3600_000)).toBe(false);
  });

  it('describes override requests: title, scope and duration', () => {
    expect(formatDuration(1800)).toBe('30m');
    expect(formatDuration(5400)).toBe('1h30m');
    expect(formatDuration(7200)).toBe('2h');
    expect(overrideTitle(override('a'), 'mac')).toBe('Allow 1 extra session on mac for 30m');
    expect(overrideTitle(override('a', { extra_slots: 2, duration_seconds: 3600 }), 'mac')).toBe('Allow 2 extra sessions on mac for 1h');
    const hold = { id: 'h1', sid: 'h-1', scope_kind: 'machine', summary: 'The user stopped everything on mac', cause: { actor: 'user', confidence: 'certain' }, created_at: '' };
    expect(overrideTitle(override('b', { scope_kind: 'hold', hold_id: 'h1' }), 'mac', hold)).toBe('Lift hold h-1 on mac for 30m');
    expect(overrideScope(override('b', { scope_kind: 'hold', hold_id: 'h1' }), 'mac', hold)).toBe('Hold h-1 on mac: The user stopped everything on mac');
    expect(overrideScope(override('a'), 'mac')).toBe('mac: 1 more session at once than its slot limit');
    expect(overrideScope(override('c', { scope_kind: 'machine' }), 'mac')).toBe('Every machine and resource hold on mac');
  });

  describe('NeedsYouStore and the dialog', () => {
    let api: Record<string, ReturnType<typeof vi.fn>>;
    beforeEach(() => {
      localStorage.clear();
      vi.useFakeTimers();
      api = {
        setUnauthorizedHandler: vi.fn(),
        listQuestions: vi.fn(async () => ({ questions: [] })),
        answerQuestion: vi.fn(async (id: string) => question(id, { status: 'answered', answer: { by: { actor: 'user' }, at: '' } as QuestionView['answer'] })),
        resolveRequest: vi.fn(async (id: string, status: string) => ({ id, status })),
        decideOverride: vi.fn(async (id: string, decision: string) => override(id, { status: decision === 'approve' ? 'approved' : 'denied' })),
        dismissVeto: vi.fn(async (id: string) => veto(id, { dismissed_at: new Date().toISOString(), status: 'dismissed' })),
      };
      TestBed.configureTestingModule({ providers: [{ provide: ApiClientService, useValue: api }] });
    });
    afterEach(() => {
      vi.useRealTimers();
      TestBed.resetTestingModule();
    });

    it('lists vetoes, questions, requests and reviews as entries; answering advances, the last one says All caught up', async () => {
      const store = TestBed.inject(NeedsYouStore);
      const questions = TestBed.inject(QuestionsStore);
      const control = TestBed.inject(ControlStore);
      questions.apply(question('a', { impact: 5 }));
      control.applyRequest({ id: 'r1', sid: 'rq-1', worker_id: 'w1', description: 'Restart nginx', status: 'open', created_at: new Date().toISOString() });
      expect(store.entries().map((e) => e.key)).toEqual(['q:a', 'r:r1']);
      expect(store.count()).toBe(2);

      store.open('q:a');
      expect(store.isOpen()).toBe(true);
      expect(store.current()?.key).toBe('q:a');
      expect(store.position()).toEqual({ index: 0, total: 2 });

      await questions.answer(questions.question('a')!, { option_id: 'o1' });
      store.advance('q:a');
      vi.advanceTimersByTime(1300);
      expect(store.current()?.key).toBe('r:r1');

      await control.resolveRequest(control.requests()[0], 'done');
      store.advance('r:r1');
      vi.advanceTimersByTime(1300);
      expect(store.caughtUp()).toBe(true);
      vi.advanceTimersByTime(2000);
      expect(store.isOpen()).toBe(false);
    });

    it('orders vetoes and override requests; a veto resolves itself once acknowledged', () => {
      const store = TestBed.inject(NeedsYouStore);
      const control = TestBed.inject(ControlStore);
      TestBed.inject(FleetStore).upsertWorker(worker());
      TestBed.inject(QuestionsStore).apply(question('a'));
      control.applyVeto(veto('waiting', { status: 'delivered', delivered_at: new Date().toISOString(), delivered_to: 'the orchestrator (chat c-o)' }));
      control.applyVeto(veto('late', { escalated_at: new Date().toISOString(), status: 'escalated' }));
      control.applyOverride(override('o1'));
      expect(store.entries().map((e) => e.key)).toEqual(['v:late', 'q:a', 'o:o1', 'v:waiting']);
      const [late, , ov, waiting] = store.entries();
      expect(late.tag).toBe('P1');
      expect(late.meta).toContain('not acknowledged');
      expect(waiting.tag).toBe('P3');
      expect(waiting.meta).toContain('waiting for the agent');
      expect(ov).toMatchObject({ kind: 'override', tag: 'P2', sid: 'o-o1', title: 'Allow 1 extra session on desk for 30m' });
      expect(ov.meta).toContain('orchestrator');

      // The agent acknowledges on its next turn: the hub's update removes it.
      control.applyVeto(veto('waiting', { acked_at: new Date().toISOString(), status: 'acknowledged' }));
      expect(store.entries().map((e) => e.key)).toEqual(['v:late', 'q:a', 'o:o1']);
      // A decided override leaves too.
      control.applyOverride(override('o1', { status: 'approved' }));
      expect(store.entries().map((e) => e.key)).toEqual(['v:late', 'q:a']);
    });

    it('a shells reminder is one Needs-you item per session with Kill all, Ask agent and Keep running', async () => {
      const store = TestBed.inject(NeedsYouStore);
      const control = TestBed.inject(ControlStore);
      TestBed.inject(FleetStore).upsertWorker(worker());
      api['keepShells'] = vi.fn(async () => ({ ok: true }));
      api['shellAction'] = vi.fn(async () => ({ ok: true, shells: 2 }));
      const reminder = {
        id: 'task:t1', task_id: 't1', worker_id: 'w1', title: '2 shells left running by Fix login', session_title: 'Fix login', shells: 2,
        shell_list: [{ pid: 41, command: 'npm run dev', started_at: '', since: '' }, { pid: 42, command: 'tsc --watch', started_at: '', since: '' }],
        idle_since: new Date(Date.now() - 31 * 60_000).toISOString(), can_ask: true, mode: 'remind' as const, created_at: new Date().toISOString(),
      };
      control.applyShellReminder(reminder);
      control.applyShellReminder({ ...reminder }); // the same session again: still one item
      expect(store.entries().map((e) => e.key)).toEqual(['sh:task:t1']);
      const [e] = store.entries();
      expect(e).toMatchObject({ kind: 'shells', tag: 'P3', title: '2 shells left running by Fix login', sessionId: 't1' });
      expect(e.meta).toContain('desk');
      expect(e.meta).toContain('idle 31m');

      store.open('sh:task:t1');
      const fixture = TestBed.createComponent(NeedsYouDialogComponent);
      fixture.detectChanges();
      const root = fixture.nativeElement as HTMLElement;
      const card = root.querySelector('[data-testid="needs-you-shells"]')!;
      expect(card.querySelector('[data-testid="shells-list"]')?.textContent).toContain('npm run dev');
      expect(card.querySelector('[data-testid="shells-ask"]')).not.toBeNull();
      card.querySelector<HTMLButtonElement>('[data-testid="shells-keep"]')!.click();
      await flushMicrotasks();
      expect(api['keepShells']).toHaveBeenCalledWith('task', 't1');
      expect(store.entries()).toEqual([]);

      // An ended task can't be asked: no Ask button; Kill all kills every shell.
      control.applyShellReminder({ ...reminder, can_ask: false });
      store.open('sh:task:t1');
      fixture.detectChanges();
      const card2 = root.querySelector('[data-testid="needs-you-shells"]')!;
      expect(card2.querySelector('[data-testid="shells-ask"]')).toBeNull();
      card2.querySelector<HTMLButtonElement>('[data-testid="shells-kill"]')!.click();
      await flushMicrotasks();
      expect(api['shellAction']).toHaveBeenCalledWith('task', 't1', 'kill');
      // The hub's resolved update is idempotent.
      control.applyShellReminder({ ...reminder, resolved: true });
      expect(control.shellReminders()).toEqual([]);
    });

    it('the override card shows scope, duration, reason and consequences; Approve sends the note and moves on', async () => {
      const store = TestBed.inject(NeedsYouStore);
      const control = TestBed.inject(ControlStore);
      TestBed.inject(FleetStore).upsertWorker(worker());
      control.applyOverride(override('o1'));
      control.applyOverride(override('o2', { created_at: new Date(Date.now() - 90_000).toISOString() }));
      store.open('o:o1');
      const fixture = TestBed.createComponent(NeedsYouDialogComponent);
      fixture.detectChanges();
      const root = fixture.nativeElement as HTMLElement;
      const card = root.querySelector('[data-testid="needs-you-override"]')!;
      expect(card.getAttribute('data-override')).toBe('o-o1');
      expect(card.querySelector('[data-testid="override-scope"]')?.textContent).toContain('desk: 1 more session at once');
      expect(card.querySelector('[data-testid="override-duration"]')?.textContent).toContain('30m');
      expect(card.querySelector('[data-testid="override-reason"]')?.textContent).toContain('e2e suite');
      expect(card.querySelector('[data-testid="override-consequences"]')?.textContent).toContain('up to 3 sessions');
      const note = card.querySelector<HTMLInputElement>('[data-testid="override-note"]')!;
      note.value = 'today only';
      note.dispatchEvent(new Event('input'));
      card.querySelector<HTMLButtonElement>('[data-testid="override-approve"]')!.click();
      await flushMicrotasks();
      expect(api['decideOverride']).toHaveBeenCalledWith('o1', 'approve', 'today only');
      expect(control.overrides().map((o) => o.id)).toEqual(['o2']);
      fixture.detectChanges();
      expect(root.querySelector('[data-testid="override-decided"]')?.textContent).toContain('Approved');
      vi.advanceTimersByTime(1300);
      expect(store.current()?.key).toBe('o:o2');
      fixture.destroy();
    });

    it('the veto card waits for the agent; only an escalated veto can be dismissed', async () => {
      const store = TestBed.inject(NeedsYouStore);
      const control = TestBed.inject(ControlStore);
      control.applyVeto(veto('w', { status: 'delivered', delivered_at: new Date().toISOString(), delivered_to: 'the orchestrator (chat c-o)' }));
      store.open('v:w');
      const fixture = TestBed.createComponent(NeedsYouDialogComponent);
      fixture.detectChanges();
      const root = fixture.nativeElement as HTMLElement;
      expect(root.querySelector('[data-testid="veto-waiting"]')).toBeTruthy();
      expect(root.querySelector('[data-testid="veto-steps"]')?.textContent).toContain('Told to the orchestrator (chat c-o)');
      expect(root.querySelector('[data-testid="veto-dismiss"]')).toBeNull();
      expect(root.querySelector('[data-testid="veto-ack"]')).toBeNull();

      control.applyVeto(veto('w', { status: 'escalated', escalated_at: new Date().toISOString(), delivered_at: new Date().toISOString() }));
      fixture.detectChanges();
      expect(root.querySelector('[data-testid="needs-you-veto"]')?.getAttribute('data-veto-status')).toBe('escalated');
      expect(root.querySelector('[data-testid="veto-escalated"]')?.textContent).toContain('Not acknowledged');
      root.querySelector<HTMLButtonElement>('[data-testid="veto-dismiss"]')!.click();
      await flushMicrotasks();
      expect(api['dismissVeto']).toHaveBeenCalledWith('w');
      expect(control.vetoes()).toEqual([]);
      fixture.detectChanges();
      expect(root.querySelector('[data-testid="veto-resolved"]')?.textContent).toContain('Dismissed by you');
      fixture.destroy();
    });

    it('the dialog moves with j/k and arrows, shows its position, and Esc closes it', async () => {
      const store = TestBed.inject(NeedsYouStore);
      const questions = TestBed.inject(QuestionsStore);
      questions.apply(question('a', { impact: 5 }));
      questions.apply(question('b', { impact: 3 }));
      store.open('q:b');
      const fixture = TestBed.createComponent(NeedsYouDialogComponent);
      fixture.detectChanges();
      const root = fixture.nativeElement as HTMLElement;
      expect(root.querySelector('[data-testid="needs-you-position"]')?.textContent).toContain('2 of 2');
      expect(root.querySelectorAll('[aria-current="step"]').length).toBe(1);
      const key = (k: string) => document.dispatchEvent(new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true }));
      key('k');
      expect(store.current()?.key).toBe('q:a');
      key('ArrowRight');
      expect(store.current()?.key).toBe('q:b');
      key('Escape');
      expect(store.isOpen()).toBe(false);
      fixture.destroy();
    });

    it('the list item is one labelled button that opens the dialog on its entry', () => {
      const store = TestBed.inject(NeedsYouStore);
      TestBed.inject(QuestionsStore).apply(question('a'));
      const fixture = TestBed.createComponent(NeedsYouItemComponent);
      fixture.componentRef.setInput('entry', store.entries()[0]);
      fixture.detectChanges();
      const btn = (fixture.nativeElement as HTMLElement).querySelector('button')!;
      expect(btn.getAttribute('aria-label')).toContain('Question: Question a?');
      expect(btn.getAttribute('data-question')).toBe('q-a');
      btn.click();
      expect(store.currentKey()).toBe('q:a');
    });

    it('mode surfaces: no native title tooltips; icon-only buttons are labelled', async () => {
      TestBed.inject(QuestionsStore).apply(question('a'));
      TestBed.inject(FleetStore).upsertWorker(worker());
      TestBed.inject(FleetStore).upsertTask(task('a', 'running'));
      const { NeedsYouRailComponent } = await import('./command/needs-you-rail.component');
      const { LiveWorkComponent } = await import('./command/live-work.component');
      for (const cmp of [NeedsYouRailComponent, LiveWorkComponent, NeedsYouDialogComponent]) {
        if (cmp === NeedsYouDialogComponent) TestBed.inject(NeedsYouStore).open('q:a');
        const fixture = TestBed.createComponent(cmp as never);
        fixture.detectChanges();
        const root = fixture.nativeElement as HTMLElement;
        expect(root.querySelectorAll('[title]').length).toBe(0);
        for (const b of Array.from(root.querySelectorAll('button'))) {
          const name = b.getAttribute('aria-label') || b.textContent?.trim();
          expect(name, b.outerHTML.slice(0, 120)).toBeTruthy();
        }
        fixture.destroy();
      }
    });

    it('WorkStore counts working, queued and reviews from fleet and chats', async () => {
      const fleet = TestBed.inject(FleetStore);
      fleet.upsertWorker(worker());
      fleet.upsertTask(task('a', 'running'));
      fleet.upsertTask(task('b', 'queued'));
      TestBed.inject(OrchestratorStore).sessions.set([chat('c', 'working')]);
      const work = TestBed.inject(WorkStore);
      expect(work.counts()).toMatchObject({ working: 2, queued: 1, review: 0 });
      // Expedite lives on the hub: the call's (or a pushed) review moves the card.
      const review = (expedite: boolean) => ({ id: 'rv1', sid: 'rv-1', task_id: 'a', expedite, created_at: '', updated_at: new Date().toISOString() });
      api['setExpedite'] = vi.fn(async (_kind: string, _id: string, on: boolean) => ({ review: review(on) }));
      await work.toggleExpedite('a');
      expect(api['setExpedite']).toHaveBeenCalledWith('task', 'a', true);
      expect(work.board().expedite.running.map((i) => i.id)).toEqual(['a']);
      // Another client takes it out: the pushed review wins.
      vi.setSystemTime(Date.now() + 1000);
      fleet.upsertReview(review(false));
      expect(work.board().expedite.running).toEqual([]);
      expect(work.isExpedited('a')).toBe(false);
    });

    it('WorkStore takes review decisions from the hub, not this browser', async () => {
      const fleet = TestBed.inject(FleetStore);
      fleet.upsertWorker(worker());
      const changes = JSON.stringify({ commits: ['abc fix'] });
      fleet.upsertTask(task('v', 'completed', { git_summary_json: changes, completed_at: new Date().toISOString() }));
      const work = TestBed.inject(WorkStore);
      expect(work.reviews().map((i) => i.id)).toEqual(['v']);
      const decided = { id: 'rv2', sid: 'rv-2', task_id: 'v', decision: 'approved' as const, delivery_mode: 'keep_branch' as const, delivery_status: 'delivered' as const, expedite: false, created_at: '', updated_at: new Date().toISOString() };
      api['reviewTask'] = vi.fn(async () => ({ review: decided }));
      const res = await work.review('v', 'approved');
      expect(api['reviewTask']).toHaveBeenCalledWith('v', 'approved', undefined);
      expect(res.review.delivery_status).toBe('delivered');
      expect(work.reviews()).toEqual([]);
      expect(work.byId()['v'].column).toBe('done');
      expect(localStorage.length).toBe(0);
    });
  });
});

describe('wall', () => {
  beforeEach(() => localStorage.clear());

  it('pages without reordering and keeps the user order of pins', () => {
    expect(tilesPerPage('1x2')).toBe(2);
    expect(tilesPerPage('3x3')).toBe(9);
    expect(pageOf([1, 2, 3, 4, 5], '1x2', 2)).toEqual([5]);
    expect(pageCount(10, '3x3')).toBe(2);
    TestBed.configureTestingModule({});
    const wall = TestBed.inject(WallStore);
    wall.pin('a');
    wall.pin('b');
    wall.pin('a');
    expect(wall.pinned()).toEqual(['a', 'b']);
    wall.move('b', -1);
    expect(wall.pinned()).toEqual(['b', 'a']);
    wall.prune((id) => id === 'a');
    expect(wall.pinned()).toEqual(['a']);
    TestBed.resetTestingModule();
  });

  it('suggests asking and running live work, never observed or queued sessions', () => {
    const items = buildWorkItems(
      inputs({ tasks: [task('r', 'running'), task('w', 'waiting_input'), task('q', 'queued'), task('o', 'running', { origin: 'external' })] }),
    );
    expect(suggestTiles(items, [], 4)).toEqual(['w', 'r']);
    expect(suggestTiles(items, ['w'], 4)).toEqual(['r']);
  });
});

describe('review', () => {
  it('reads receipts from git, milestones and the summary', () => {
    const [it] = buildWorkItems(
      inputs({ tasks: [task('v', 'completed', { summary: 'All good', git_summary_json: JSON.stringify({ commits: ['abc fix'], dirty_files: ['?? x.go'] }) })] }),
    );
    const r = receiptsFor(it, [{ id: 1, task_id: 'v', kind: 'milestone', message: 'Tests pass', created_at: '' }]);
    expect(r.map((x) => x.text)).toEqual(['abc fix', 'Uncommitted: x.go', 'Tests pass', 'All good']);
  });
});

describe('mode shell', () => {
  it('maps keys 1–4 to modes unless typing, modified or in a dialog/terminal', () => {
    const base = { metaKey: false, ctrlKey: false, altKey: false, shiftKey: false, defaultPrevented: false, target: document.body };
    expect(modeForKey({ ...base, key: '2' })).toBe('work');
    expect(modeForKey({ ...base, key: '4' })).toBe('wall');
    expect(modeForKey({ ...base, key: '5' })).toBeNull();
    expect(modeForKey({ ...base, key: '1', metaKey: true })).toBeNull();
    expect(modeForKey({ ...base, key: '1', target: document.createElement('textarea') })).toBeNull();
  });

  it('matches /mode and /mode/<sid> with one route', () => {
    const seg = (p: string) => new UrlSegment(p, {});
    const m = modeMatcher('command');
    expect(m([seg('command')])?.posParams).toEqual({});
    expect(m([seg('command'), seg('t-abc12')])?.posParams?.['sid'].path).toBe('t-abc12');
    expect(m([seg('work')])).toBeNull();
    expect(m([seg('command'), seg('a'), seg('b')])).toBeNull();
  });
});

import { TestBed } from '@angular/core/testing';
import { UrlSegment } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { modeMatcher } from '../app.routes';
import { Hold } from '../core/models/control.models';
import { ChatSession, Task, Worker } from '../core/models/konsert.models';
import { QuestionView } from '../core/models/question.models';
import { ApiClientService } from '../core/services/api-client.service';
import { ControlStore } from '../core/state/control.store';
import { FleetStore } from '../core/state/fleet.store';
import { NeedsYouStore, nextPendingKey, recentVetoes, vetoDetails } from '../core/state/needs-you.store';
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

  it('keeps vetoes for two hours unless acknowledged and reads their reason', () => {
    const ev = { id: 1, kind: 'veto', action: 'wrong_machine', created_at: new Date(Date.now() - 60_000).toISOString(), summary: '', cause: { actor: 'user', confidence: 'certain' }, meta_json: '{"reason":"wrong_machine","note":"use CI"}' };
    const old = { ...ev, id: 2, created_at: new Date(Date.now() - 3 * 3600_000).toISOString() };
    expect(recentVetoes([ev, old], {}).map((e) => e.id)).toEqual([1]);
    expect(recentVetoes([ev], { 'veto:1': Date.now() })).toEqual([]);
    expect(vetoDetails(ev)).toEqual({ reason: 'Wrong machine', note: 'use CI' });
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
      const decided = { id: 'rv2', sid: 'rv-2', task_id: 'v', decision: 'approved' as const, github_status: 'no_pr' as const, expedite: false, created_at: '', updated_at: new Date().toISOString() };
      api['reviewTask'] = vi.fn(async () => ({ review: decided }));
      const res = await work.review('v', 'approved');
      expect(api['reviewTask']).toHaveBeenCalledWith('v', 'approved', undefined);
      expect(res.review.github_status).toBe('no_pr');
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

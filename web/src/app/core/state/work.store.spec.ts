import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SessionActivity } from '../models/activity.models';
import { ChatSession, Project, Repo, Task, Worker } from '../models/konsert.models';
import { ApiClientService } from '../services/api-client.service';
import { activityFromTaskStatus, bucketOf, groupWork, heatLevelOf, nowTitle, taskNow, taskTitle } from './activity-logic';
import { ActivityStore } from './activity.store';
import { FleetStore } from './fleet.store';
import { OrchestratorStore } from './orchestrator.store';
import { WorkStore } from './work.store';

const proj = (id: string, name: string, extra: Partial<Project> = {}): Project =>
  ({ id, sid: 'p-' + id, workspace_id: 'ws', name, is_default: false, created_at: '', ...extra }) as Project;
const defProj = { id: 'def', sid: 'p-def', workspace_id: 'ws', name: 'Default Project', is_default: true, created_at: '' } as Project;
const repo = (projectId: string, path: string, worker = 'w1'): Repo =>
  ({ id: 'r-' + path, sid: 'r', project_id: projectId, worker_id: worker, name: path, path, created_at: '' }) as Repo;

function task(id: string, status: Task['status'], extra: Partial<Task> = {}): Task {
  return {
    id,
    sid: 't-' + id,
    worker_id: 'w1',
    agent: 'claude',
    status,
    prompt: `Prompt of ${id}\nmore`,
    project_id: 'def',
    workdir: '/elsewhere',
    effective_dir: '',
    created_at: '2026-11-23T10:00:00Z',
    ...extra,
  } as Task;
}

function chat(id: string, status: string, extra: Partial<ChatSession> = {}): ChatSession {
  return { id, sid: 'c-' + id, title: `Chat ${id}`, provider: '', model: '', created_at: '', updated_at: '', backend: 'agent', worker_id: 'w1', agent: 'claude', status, ...extra } as ChatSession;
}

function act(key: { task_id?: string; chat_id?: string }, state: SessionActivity['state'], extra: Partial<SessionActivity> = {}): SessionActivity {
  return { ...key, state, since: '2026-11-23T10:00:00Z', at: '2026-11-23T10:00:01Z', ...extra };
}

describe('activity logic', () => {
  it('buckets by activity, with a terminal task status winning', () => {
    expect(bucketOf(act({ task_id: 't' }, 'working'))).toBe('running');
    expect(bucketOf(act({ task_id: 't' }, 'speaking'))).toBe('running');
    expect(bucketOf(act({ task_id: 't' }, 'asking'))).toBe('needs_you');
    expect(bucketOf(act({ task_id: 't' }, 'blocked'))).toBe('blocked');
    expect(bucketOf(act({ task_id: 't' }, 'queued'))).toBe('queued');
    expect(bucketOf(act({ task_id: 't' }, 'error'))).toBe('done');
    expect(bucketOf(act({ task_id: 't' }, 'working'), 'completed')).toBe('done');
  });

  it('derives a coarse activity from a task status', () => {
    expect(activityFromTaskStatus(task('a', 'queued')).state).toBe('queued');
    expect(activityFromTaskStatus(task('a', 'waiting_input')).state).toBe('asking');
    const paused = activityFromTaskStatus(task('a', 'suspended', { suspended_by: 'pause' }));
    expect(paused.state).toBe('blocked');
    expect(paused.label).toContain('Pause');
    const failed = activityFromTaskStatus(task('a', 'failed', { summary: 'boom\nstack' }));
    expect(failed).toMatchObject({ state: 'error', label: 'boom', coarse: true });
  });

  it('never leaves a title or now line blank', () => {
    const bare = task('a', 'running', { prompt: '' });
    expect(taskTitle(bare).text).toBe('claude t-a');
    expect(taskTitle(task('a', 'running', { ticket_id: 'KON-2Q0CH', ticket_title: 'Questions' })).text).toBe('KON-2Q0CH Questions');
    expect(taskTitle(task('a', 'running', { title: 'Mobile card' })).source).toBe('title');
    // Orchestrator narration > live tool label > title.
    const t = task('a', 'running', { title: 'Mobile card' });
    expect(taskNow(t, act({ task_id: 'a' }, 'thinking'))).toEqual({ text: 'Mobile card', source: 'title' });
    expect(taskNow(t, act({ task_id: 'a' }, 'working', { tool: 'Bash', label: 'Run: go test ./...' }))).toEqual({
      text: 'Run: go test ./...',
      source: 'activity',
    });
    expect(taskNow({ ...t, narration: 'Porting the card' }, act({ task_id: 'a' }, 'working', { label: 'x' })).source).toBe('orchestrator');
  });

  it("shows the hub's summary of the running tool and keeps the raw label as the hover title", () => {
    const raw = 'Bash: cd /private/var/tmp/konsert-live/x && python -m pytest -q';
    const a = act({ task_id: 'a' }, 'working', { tool: 'Bash', label: raw, summary: 'running tests' });
    const t = task('a', 'running', { title: 'Word count' });
    expect(taskNow(t, a)).toEqual({ text: 'running tests', source: 'activity' });
    expect(nowTitle(a)).toBe(raw);
    expect(nowTitle(act({ task_id: 'a' }, 'thinking', { label: 'old' }))).toBeUndefined();
    const [g] = groupWork({ projects: [defProj], repos: [], tasks: [t], chats: [], activityOf: () => a });
    expect(g.items[0].now).toBe('running tests');
    expect(g.items[0].nowTitle).toBe(raw);
  });

  it('maps heat scores to levels at the documented thresholds', () => {
    expect(heatLevelOf(0.39)).toBe('cool');
    expect(heatLevelOf(0.4)).toBe('warm');
    expect(heatLevelOf(0.75)).toBe('warm');
    expect(heatLevelOf(0.76)).toBe('hot');
  });

  it('groups by project, maps chats and observed sessions by cwd, puts the rest under Other work', () => {
    const groups = groupWork({
      projects: [defProj, proj('p1', 'Konsert', { headline: 'Ship mobile questions' }), proj('p2', 'Website')],
      repos: [repo('p1', '/src/konsert'), repo('p2', '/src/site')],
      tasks: [
        task('t1', 'running', { project_id: 'p1', title: 'Card' }),
        task('t2', 'waiting_input', { project_id: 'p1' }),
        task('t3', 'queued', { project_id: 'p2' }),
        task('obs', 'running', { origin: 'external', effective_dir: '/src/site/web' }),
        task('loose', 'completed'),
        task('scratch', 'running', { origin: 'scratch' }),
        task('sh', 'running', { agent: 'shell' }),
      ],
      chats: [chat('c1', 'working', { cwd: '/src/konsert/web' }), chat('c2', 'done', { cwd: '/tmp' }), chat('orch', 'working', { fleet_tools: true })],
      activityOf: (k) => (k === 'task:t1' ? act({ task_id: 't1' }, 'working', { label: 'Run: go test' }) : undefined),
    });
    expect(groups.map((g) => g.name)).toEqual(['Konsert', 'Website', 'Other work']);
    const [konsert, website, other] = groups;
    expect(konsert.headline).toBe('Ship mobile questions');
    expect(konsert.headlineSource).toBe('orchestrator');
    expect(konsert.counts).toMatchObject({ running: 2, needs_you: 1, queued: 0, done: 0 });
    // Needs-you first.
    expect(konsert.items[0].id).toBe('t2');
    expect(konsert.items.find((i) => i.id === 't1')?.now).toBe('Run: go test');
    expect(website.counts).toMatchObject({ running: 1, queued: 1 });
    expect(website.items.find((i) => i.id === 'obs')?.observed).toBe(true);
    // Fallback headline from facts.
    expect(website.headline).toMatch(/^Working on /);
    expect(other.items.map((i) => i.id).sort()).toEqual(['c2', 'loose']);
    expect(other.headline).toBe('2 done');
    for (const g of groups) {
      expect(g.headline.trim()).not.toBe('');
      for (const it of g.items) {
        expect(it.title.trim()).not.toBe('');
        expect(it.now.trim()).not.toBe('');
      }
    }
  });
});

describe('WorkStore + ActivityStore', () => {
  let fleet: FleetStore;
  let activity: ActivityStore;
  let orch: OrchestratorStore;
  let work: WorkStore;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [{ provide: ApiClientService, useValue: { getFleet: vi.fn() } }],
    });
    fleet = TestBed.inject(FleetStore);
    activity = TestBed.inject(ActivityStore);
    orch = TestBed.inject(OrchestratorStore);
    work = TestBed.inject(WorkStore);
    fleet.projects.set([defProj, proj('p1', 'Konsert')]);
    fleet.repos.set([repo('p1', '/src/konsert')]);
  });

  afterEach(() => TestBed.resetTestingModule());

  it('keeps counts correct as tasks move and activity arrives', () => {
    fleet.upsertTask(task('t1', 'queued', { project_id: 'p1' }));
    fleet.upsertTask(task('t2', 'queued', { project_id: 'p1' }));
    const counts = () => work.projectGroups().find((g) => g.projectId === 'p1')!.counts;
    expect(counts()).toMatchObject({ queued: 2, running: 0, needs_you: 0, done: 0 });

    fleet.upsertTask(task('t1', 'running', { project_id: 'p1' }));
    expect(counts()).toMatchObject({ queued: 1, running: 1 });

    activity.apply(act({ task_id: 't1' }, 'asking', { label: 'Allow rm -rf build/?' }));
    expect(counts()).toMatchObject({ queued: 1, running: 0, needs_you: 1 });
    expect(work.itemsByKey()['task:t1'].now).toBe('Allow rm -rf build/?');

    // An older report never reverts a newer one.
    activity.apply(act({ task_id: 't1' }, 'working', { at: '2026-11-23T09:00:00Z' }));
    expect(activity.forTask('t1')?.state).toBe('asking');

    fleet.upsertTask(task('t1', 'completed', { project_id: 'p1' }));
    fleet.upsertTask(task('t2', 'failed', { project_id: 'p1' }));
    expect(counts()).toMatchObject({ queued: 0, running: 0, needs_you: 0, done: 2, failed: 1 });
    expect(work.totals().done).toBe(2);
  });

  it('exposes activity per chat and per task, with status fallbacks', () => {
    orch.sessions.set([chat('c1', 'needs_you', { cwd: '/src/konsert' })]);
    fleet.upsertTask(task('t1', 'running', { project_id: 'p1' }));
    expect(work.chatActivity('c1')?.state).toBe('asking');
    expect(work.taskActivity('t1')?.state).toBe('thinking');
    activity.applySnapshot([act({ chat_id: 'c1' }, 'speaking', { tokens_per_sec: 42 }), act({ task_id: 't1' }, 'working', { tool: 'Edit' })]);
    expect(activity.byChat()['c1'].tokens_per_sec).toBe(42);
    expect(activity.byTask()['t1'].tool).toBe('Edit');
    expect(work.projectGroups().find((g) => g.projectId === 'p1')!.counts.running).toBe(2);
    activity.forget('chat:c1');
    expect(activity.forChat('c1')).toBeUndefined();
  });

  it('exposes heat per online worker in the fleet store', () => {
    const heat = { score: 0.8, raw: 0.9, level: 'hot' as const, cause: 'cpu' as const, updated_at: '' };
    fleet.upsertWorker({ id: 'w1', name: 'w1', host_group: 'w1', status: 'online', heat } as Worker);
    expect(fleet.heatByWorker()['w1']).toEqual(heat);
    // An update without heat (facts, wake lock) keeps it.
    fleet.upsertWorker({ id: 'w1', name: 'w1', host_group: 'w1', status: 'online' } as Worker);
    expect(fleet.heatByWorker()['w1']?.level).toBe('hot');
    fleet.upsertWorker({ id: 'w1', name: 'w1', host_group: 'w1', status: 'offline' } as Worker);
    expect(fleet.heatByWorker()['w1']).toBeUndefined();
  });
});

import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { HeatHistory, SessionActivity } from '../core/models/activity.models';
import { ChatSession, Project, Task, Worker } from '../core/models/konsert.models';
import { groupWork } from '../core/state/activity-logic';
import { WorkItem, buildWorkItems } from '../core/state/work-logic';
import { ActivitySpinnerComponent } from '../shared/activity-spinner.component';
import { activityPhrase, describeSpinner, heatFill, machineInitials, rippleSeconds, spinnerStateOf } from '../shared/spinner-logic';
import { DONE_FADE_MS, buildStory, queuedLabel } from './command/live-work-logic';
import { describeStrip, heatStrip, heatText, machineBadge, machineRows, queueReasons } from './fleet/fleet-logic';

const NOW = Date.parse('2026-11-23T15:00:00Z');
const iso = (ms: number) => new Date(ms).toISOString();

const worker = (id: string, name: string, over: Partial<Worker> = {}): Worker =>
  ({ id, sid: `m-${id}`, name, host_group: '', os: 'linux', arch: 'amd64', isolation: 'host', permission_mode: 'full_access', agents: ['claude', 'codex'], tags: {}, max_concurrent: 2, status: 'online', last_seen_at: iso(NOW), ...over }) as Worker;

const task = (id: string, status: Task['status'], over: Partial<Task> = {}): Task =>
  ({ id, sid: `t-${id}`, worker_id: 'w1', agent: 'claude', status, prompt: `Do ${id}`, title: `Task ${id}`, workdir: '/src/jk', effective_dir: '', origin: 'orchestrator', project_id: 'p1', created_at: iso(NOW - 3600_000), started_at: iso(NOW - 3000_000), ...over }) as Task;

const act = (task_id: string, state: SessionActivity['state'], over: Partial<SessionActivity> = {}): SessionActivity => ({ task_id, state, since: iso(NOW - 10_000), at: iso(NOW - 5_000), ...over });

const project: Project = { id: 'p1', sid: 'p-1', name: 'JumpKick', headline: 'Adding remote cache capabilities' } as Project;

function storyFor(tasks: Task[], acts: SessionActivity[], chats: ChatSession[] = [], workers = [worker('w1', 'desk'), worker('w2', 'mini-pc')]) {
  const byKey = Object.fromEntries(acts.map((a) => [`task:${a.task_id}`, a]));
  const groups = groupWork({ projects: [project], repos: [], tasks, chats, activityOf: (k) => byKey[k] });
  const wById = Object.fromEntries(workers.map((w) => [w.id, w]));
  const items: WorkItem[] = buildWorkItems({ tasks, chats, workers: wById, projects: [project], repos: [], questions: [], holds: [], interactive: () => false, reviewed: {}, now: NOW });
  return buildStory({
    groups,
    items: Object.fromEntries(items.map((i) => [i.id, i])),
    allItems: items,
    workers: wById,
    heat: { w1: { score: 0.58, raw: 0.6, level: 'warm', cause: 'cpu', updated_at: iso(NOW) } },
    now: NOW,
    today: NOW - 10 * 3600_000,
  });
}

describe('spinner logic', () => {
  it('maps activity and control to ring states', () => {
    expect(spinnerStateOf({ state: 'working' })).toBe('working');
    expect(spinnerStateOf({ state: 'blocked' })).toBe('held');
    expect(spinnerStateOf({ state: 'thinking' }, 'YOU')).toBe('you');
    expect(spinnerStateOf({ state: 'thinking' }, 'HELD')).toBe('held');
    expect(spinnerStateOf(undefined)).toBe('done');
  });

  it('makes initials, heat fills and descriptions', () => {
    expect(machineInitials('mini-pc')).toBe('mp');
    expect(machineInitials('desk')).toBe('dk');
    expect(machineInitials('wsl')).toBe('wl');
    expect(machineInitials('')).toBe('··');
    expect(heatFill(0.1).drift).toBe('14s');
    expect(heatFill(0.97).drift).toBe('4.5s');
    expect(heatFill(null).drift).toBe('0s');
    expect(describeSpinner('working', [{ name: 'desk', heat: 0.55, cause: 'cpu' }])).toBe('working · desk · warm 55% · cpu');
    expect(describeSpinner('queued', [])).toBe('queued · not placed yet');
    expect(describeSpinner('speaking', [{ name: 'mac', heat: 0.9 }], { tps: 38.4 })).toContain('writing 38 tok/s');
    expect(activityPhrase('thinking', undefined, undefined, 0)).toBe('thinking…');
    expect(activityPhrase('speaking', 38.4, undefined, 0)).toBe('writing · 38 tok/s');
    expect(activityPhrase('done', undefined, 'Ready', 3 * 60_000)).toBe('done 3m ago');
    expect(activityPhrase('done', undefined, 'Cancelled', 0)).toBe('Cancelled · done just now');
    expect(activityPhrase('queued', undefined, undefined, 0)).toBe('');
    expect(rippleSeconds(80)).toBe(0.5);
    expect(rippleSeconds(0)).toBe(1.5);
  });

  it('renders an accessible spinner with a badge and a second machine', async () => {
    const f = TestBed.createComponent(ActivitySpinnerComponent);
    f.componentRef.setInput('state', 'asking');
    f.componentRef.setInput('machines', [
      { name: 'desk', heat: 0.3, cause: 'cpu' },
      { name: 'gpu-box', heat: 0.93, cause: 'gpu' },
    ]);
    await f.whenStable();
    const el = f.nativeElement as HTMLElement;
    expect(el.getAttribute('role')).toBe('img');
    expect(el.getAttribute('aria-label')).toBe('asks you · desk · cool 30% · cpu + gpu-box · hot 93% · gpu');
    expect(el.querySelector('.ks-sp-badge')?.textContent).toBe('?');
    expect(el.querySelector('.ks-sp-second')?.textContent?.trim()).toBe('gb');
    expect(el.querySelector('.ks-sp-avatar')?.textContent?.trim()).toBe('dk');
  });
});

describe('live-work story', () => {
  it('tells each project: headline, bar, summary, in-flight, queued, fading done', () => {
    const tasks = [
      task('a', 'running'),
      task('b', 'running', { worker_id: 'w2' }),
      task('c', 'waiting_input'),
      task('q1', 'queued', { worker_id: '', created_at: iso(NOW - 100) }),
      task('q2', 'queued', { worker_id: '' }),
      task('d', 'completed', { completed_at: iso(NOW - 60_000) }),
      task('old', 'completed', { completed_at: iso(NOW - 3600_000) }),
    ];
    const story = storyFor(tasks, [act('a', 'working', { label: 'Run: go test ./...' }), act('b', 'speaking', { tokens_per_sec: 38 })]);
    expect(story.map((p) => p.name)).toEqual(['JumpKick']);
    const p = story[0];
    expect(p.headline).toBe('Adding remote cache capabilities');
    expect(p.headlineByOrchestrator).toBe(true);
    expect(p.bar).toEqual({ done: 2, running: 2, needsYou: 1, queued: 2 });
    expect(p.summary).toBe('3 in flight on 2 machines · 2 queued · 1 needs you · 2 done today');
    expect(p.inFlight[0].state).toBe('asking');
    expect(new Set(p.inFlight.map((l) => l.state))).toEqual(new Set(['asking', 'working', 'speaking']));
    const a = p.inFlight.find((l) => l.id === 'a')!;
    expect(a.now).toBe('Run: go test ./...');
    expect(a.machines).toEqual([{ name: 'desk', heat: 0.58, cause: 'cpu' }]);
    expect(p.inFlight.find((l) => l.id === 'b')!.machines[0].heat).toBeNull();
    expect(p.queued.map((l) => l.id)).toEqual(['q1', 'q2']);
    expect(p.queued[0].machines).toEqual([]);
    expect(queuedLabel(p)).toBe('2 queued · next: Task q1');
    // Done 1 minute ago shows (fading); done an hour ago doesn't.
    expect(p.done.map((l) => l.id)).toEqual(['d']);
    expect(p.done[0].opacity).toBeLessThan(0.75);
    expect(p.done[0].opacity).toBeGreaterThan(0.35);
  });

  it('drops a project whose work finished more than the fade window ago', () => {
    const story = storyFor([task('d', 'completed', { completed_at: iso(NOW - DONE_FADE_MS - 1000) })], []);
    expect(story).toEqual([]);
  });

  it('puts observed sessions and your terminals under Other work', () => {
    const tasks = [
      task('o', 'running', { origin: 'external', project_id: '', workdir: '/tmp/scratch' }),
      task('sh', 'running', { agent: 'shell', session_kind: 'terminal', project_id: '', prompt: '' }),
    ];
    const story = storyFor(tasks, [act('o', 'working', { coarse: true, source: 'hook' })]);
    expect(story.map((p) => p.name)).toEqual(['Other work']);
    const other = story[0];
    expect(other.headline).toBe('Sessions you started yourself, plus your terminals');
    const o = other.inFlight.find((l) => l.id === 'o')!;
    expect(o.coarse).toBe(true);
    const sh = other.inFlight.find((l) => l.id === 'sh')!;
    expect(sh.terminal).toBe(true);
    expect(sh.now).toBe('shell open');
  });
});

describe('fleet machine rows', () => {
  it('lists each machine with its sessions, free slots, next queued and badge', () => {
    const workers = [worker('w1', 'desk', { max_concurrent: 3, metrics: { cpu_percent: 45.2, mem_used_mb: 7000, mem_total_mb: 10000, load_avg: 3.24 } }), worker('w2', 'mac', { status: 'offline' })];
    const tasks = [task('a', 'running'), task('q', 'queued')];
    const groups = groupWork({ projects: [project], repos: [], tasks, chats: [], activityOf: () => undefined });
    const items = buildWorkItems({ tasks, chats: [], workers: { w1: workers[0], w2: workers[1] }, projects: [project], repos: [], questions: [], holds: [], interactive: () => false, reviewed: {}, now: NOW });
    const rows = machineRows({ workers, tasks, groups, items: Object.fromEntries(items.map((i) => [i.id, i])), allItems: items, heat: {}, now: NOW });
    expect(rows[0]).toMatchObject({ badge: 'OK', running: 1, free: 2, next: 'Task q', load: 'cpu 45% · mem 70% · load 3.2' });
    expect(rows[0].sessions.map((s) => [s.title, s.project, s.state])).toEqual([['Task a', 'JumpKick', 'thinking']]);
    expect(rows[1]).toMatchObject({ badge: 'OFFLINE', free: 0, load: '', heatText: 'no heat data while offline' });
    expect(machineBadge(worker('x', 'x', { max_concurrent: 1 }), 1)).toBe('FULL');
    expect(machineBadge(worker('x', 'x', { drained: true }), 0)).toBe('DRAINING');
  });

  it('describes heat with the dominant cause first', () => {
    const w = worker('w1', 'mac');
    expect(heatText(w, { score: 0.88, raw: 0.9, level: 'hot', cause: 'cpu', components: { cpu: 0.88, mem: 0.71, slots: 0.5 }, updated_at: '' })).toBe('hot 88% · cpu 88% · mem 71%');
    expect(heatText(w, undefined)).toBe('no heat data yet');
  });

  it('builds the heat strip from history with no-data gaps', () => {
    const h: HeatHistory = {
      range: '12h',
      start: iso(NOW - 12 * 3600_000),
      end: iso(NOW),
      step_seconds: 900,
      workers: [{ worker_id: 'w1', samples: [null, { score: 0.2, max: 0.3, cause: 'cpu' }, { score: 0.8, max: 0.91, cause: 'gpu' }, null] }],
    };
    const cells = heatStrip(h, 'w1', 4);
    expect(cells.map((c) => c.color === null)).toEqual([true, false, false, true]);
    expect(describeStrip('desk', cells, '12 hours')).toBe('desk heat over the last 12 hours: mostly cool, peak hot 91% (gpu); no data for 50% of it');
    expect(heatStrip(null, 'w1', 3).every((c) => c.color === null)).toBe(true);
    expect(describeStrip('desk', heatStrip(null, 'w1', 3), '12 hours')).toBe('desk: no heat history for the last 12 hours');
  });

  it('explains why queued work waits', () => {
    const workers = [worker('w1', 'mac', { max_concurrent: 1 }), worker('w2', 'desk', { drained: true }), worker('w3', 'wsl', { agents: ['codex'] })];
    const tasks = [task('r', 'running'), task('q', 'queued', { worker_id: '' }), task('p', 'queued', { worker_id: 'w1' })];
    const q = queueReasons(tasks, workers, (t) => t.title ?? '');
    expect(q.map((e) => e.sid)).toEqual(['t-q', 't-p']);
    expect(q[0].reasons.map((r) => r.text)).toEqual(['mac FULL (1/1)', 'desk draining', 'wsl ✗ no claude']);
    expect(q[1].reasons.map((r) => r.text)).toEqual(['mac FULL (1/1)']);
  });
});

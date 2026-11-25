import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { HeatHistory, SessionActivity } from '../core/models/activity.models';
import { ChatSession, Project, Task, Worker } from '../core/models/konsert.models';
import { groupWork } from '../core/state/activity-logic';
import { WorkItem, buildWorkItems } from '../core/state/work-logic';
import { ActivitySpinnerComponent } from '../shared/activity-spinner.component';
import {
  activityPhrase,
  describeDetail,
  describeSpinner,
  heatFill,
  machineInitials,
  orbitDots,
  rippleSeconds,
  spinnerDetailOf,
  spinnerStateOf,
  withDetail,
} from '../shared/spinner-logic';
import { DONE_FADE_MS, buildStory, queuedLabel } from './command/live-work-logic';
import {
  askAvailability,
  describeStrip,
  heatStrip,
  heatText,
  machineBadge,
  machineRows,
  offlineText,
  queueReasons,
  shellChip,
  shellRows,
  subagentChip,
  subagentText,
  transportHint,
  transportIcon,
  transportLabel,
  windowsStartText,
} from './fleet/fleet-logic';

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
    // A parent waiting on its sub-agents: a plain grey ring; a sub-agent asking: amber.
    expect(spinnerStateOf({ state: 'working', waiting: true })).toBe('waiting');
    expect(spinnerStateOf({ state: 'working', waiting: true, subagents: [{ id: 'a', state: 'asking', since: '' }] })).toBe('asking');
    expect(spinnerStateOf({ state: 'done', subagents: [{ id: 'a', state: 'asking', since: '' }] })).toBe('done');
  });

  it('derives shells and sub-agents for the badge, dots and words', () => {
    expect(spinnerDetailOf({ state: 'thinking' })).toBeNull();
    const d = spinnerDetailOf({
      state: 'thinking',
      shells: 3,
      shells_busy: 2,
      subagents: [
        { id: 'a', state: 'working', since: '' },
        { id: 'b', state: 'speaking', since: '' },
      ],
    });
    expect(d).toEqual({ shells: 3, shellsBusy: 2, subs: ['working', 'speaking'] });
    expect(describeDetail(d)).toBe('3 shells (2 busy) · 2 sub-agents');
    // The mockup's hover text.
    expect(describeSpinner('thinking', [{ name: 'desk', heat: 0.55, cause: 'cpu' }], { detail: d })).toBe(
      'desk · warm 55% · cpu — thinking · 3 shells (2 busy) · 2 sub-agents',
    );
    // Done: shells left running and background sub-agents stay.
    expect(spinnerDetailOf({ state: 'done', shells: 1, subagents: [{ id: 'a', state: 'working', since: '' }] })).toEqual({ shells: 1, shellsBusy: 0, subs: ['working'] });
    // Four dots, then "+N".
    const o = orbitDots(['working', 'working', 'thinking', 'speaking', 'working', 'thinking']);
    expect(o.dots.length).toBe(4);
    expect(o.more).toBe(2);
    expect(o.dots.map((x) => x.angle)).toEqual([30, 120, 210, 300]);
    // Line text: counts replace a generic phrase, follow a specific one.
    expect(withDetail('thinking…', 'thinking', d)).toBe('3 shells · 2 sub-agents');
    expect(withDetail('running tests', 'working', d)).toBe('running tests · 3 shells · 2 sub-agents');
    expect(withDetail('done 3m ago', 'done', { shells: 2, shellsBusy: 0, subs: [] })).toBe('done 3m ago · 2 shells left running');
    expect(withDetail('asks you: x', 'asking', d)).toBe('asks you: x');
    expect(activityPhrase('asking', undefined, 'Sub-agent “Docs” asks: Allow Write?', 0)).toBe('Sub-agent “Docs” asks: Allow Write?');
  });

  it('draws the shell badge and the orbiting sub-agent dots', async () => {
    const f = TestBed.createComponent(ActivitySpinnerComponent);
    f.componentRef.setInput('state', 'waiting');
    f.componentRef.setInput('machines', [{ name: 'desk', heat: 0.55, cause: 'cpu' }]);
    f.componentRef.setInput('detail', { shells: 3, shellsBusy: 2, subs: ['working', 'speaking', 'thinking', 'asking', 'working'] });
    await f.whenStable();
    const el = f.nativeElement as HTMLElement;
    expect(el.querySelector('.ks-sp-ring')?.getAttribute('data-ring')).toBe('waiting');
    expect(el.querySelector('[data-testid="spinner-shells"]')?.textContent?.trim()).toBe('❯3');
    expect(el.querySelector('.ks-sp-shells-busy')).not.toBeNull();
    expect(el.querySelectorAll('.ks-sp-dot').length).toBe(4);
    expect(el.querySelector('[data-testid="spinner-more"]')?.textContent?.trim()).toBe('+1');
    expect(el.getAttribute('aria-label')).toBe('desk · warm 55% · cpu — waiting on sub-agents · 3 shells (2 busy) · 5 sub-agents');
    // Idle shells: a still badge; none: no badge.
    f.componentRef.setInput('detail', { shells: 1, shellsBusy: 0, subs: [] });
    await f.whenStable();
    expect(el.querySelector('.ks-sp-shells-busy')).toBeNull();
    expect(el.querySelectorAll('.ks-sp-dot').length).toBe(0);
    f.componentRef.setInput('detail', null);
    await f.whenStable();
    expect(el.querySelector('[data-testid="spinner-shells"]')).toBeNull();
  });

  it('makes initials, heat fills and descriptions', () => {
    expect(machineInitials('mini-pc')).toBe('mp');
    expect(machineInitials('desk')).toBe('dk');
    expect(machineInitials('wsl')).toBe('wl');
    expect(machineInitials('')).toBe('··');
    expect(heatFill(0.1).drift).toBe('14s');
    expect(heatFill(0.97).drift).toBe('4.5s');
    expect(heatFill(null).drift).toBe('0s');
    expect(describeSpinner('working', [{ name: 'desk', heat: 0.55, cause: 'cpu' }])).toBe('desk · warm 55% · cpu — working');
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
    expect(el.getAttribute('aria-label')).toBe('desk · cool 30% · cpu + gpu-box · hot 93% · gpu — asks you');
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

  it('spells out shells and sub-agents, and keeps a finished session while its shells run', () => {
    const workers = [worker('w1', 'desk', { max_concurrent: 3 })];
    const tasks = [task('a', 'running'), task('d', 'completed', { completed_at: iso(NOW - 3600_000) }), task('e', 'completed', { completed_at: iso(NOW - 3600_000) })];
    const shells = [
      { pid: 41, command: 'npm run dev', started_at: iso(NOW - 720_000), since: iso(NOW - 600_000), busy: false },
      { pid: 42, command: 'go test ./internal/cache/...', started_at: iso(NOW - 41_000), since: iso(NOW - 41_000), busy: true },
    ];
    const acts: Record<string, SessionActivity> = {
      'task:a': act('a', 'working', {
        waiting: true,
        shells: 2,
        shells_busy: 1,
        shell_list: shells,
        subagents: [{ id: 's1', title: 'S3 client tests', state: 'working', label: 'Run: go test', since: iso(NOW - 120_000) }],
      }),
      'task:d': act('d', 'done', { shells: 1, shell_list: [{ ...shells[0], dangling: true }] }),
      'task:e': act('e', 'done'),
    };
    const groups = groupWork({ projects: [project], repos: [], tasks, chats: [], activityOf: (k) => acts[k] });
    const items = buildWorkItems({ tasks, chats: [], workers: { w1: workers[0] }, projects: [project], repos: [], questions: [], holds: [], interactive: () => false, reviewed: {}, now: NOW });
    const rows = machineRows({ workers, tasks, groups, items: Object.fromEntries(items.map((i) => [i.id, i])), allItems: items, heat: {}, now: NOW });
    const byId = Object.fromEntries(rows[0].sessions.map((s) => [s.id, s]));
    // The finished task with a shell left running stays (to clean it up); the other goes.
    expect(Object.keys(byId).sort()).toEqual(['a', 'd']);
    const a = byId['a'];
    expect(a.state).toBe('waiting');
    expect(shellChip(a.detail, a.done)).toBe('❯ 2 shells · 1 busy');
    expect(subagentChip(a.detail)).toBe('◉ 1 sub-agent');
    expect(subagentText(a.subagents[0])).toBe('S3 client tests · Run: go test');
    expect(shellRows(a.shellList, NOW).map((r) => [r.pid, r.text, r.age])).toEqual([
      [41, 'npm run dev', 'idle'],
      [42, 'go test ./internal/cache/...', '41s'],
    ]);
    const d = byId['d'];
    expect(shellChip(d.detail, d.done)).toBe('❯ 1 shell left running');
    expect(askAvailability(d)).toMatchObject({ ok: false });
    expect(askAvailability(a)).toMatchObject({ ok: true });
    expect(askAvailability({ kind: 'chat', done: true, state: 'done' })).toMatchObject({ ok: true });
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

  it('sizes the heat strip to the points the hub returned (hourly ranges have fewer)', () => {
    const h: HeatHistory = {
      range: '24h',
      start: iso(NOW - 24 * 3600_000),
      end: iso(NOW),
      step_seconds: 3600,
      tier: 'hourly',
      workers: [{ worker_id: 'w1', samples: Array.from({ length: 24 }, () => null) }],
    };
    expect(heatStrip(h, 'w1').length).toBe(24);
    expect(heatStrip(h, 'not-in-history').length).toBe(24);
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

describe('machine transport', () => {
  it('labels how each machine reaches the hub', () => {
    expect(transportLabel(worker('a', 'box', { transport: 'ssh', ssh_target: 'me@box' }))).toBe('ssh');
    expect(transportLabel(worker('b', 'hub', { transport: 'local' }))).toBe('local');
    expect(transportLabel(worker('c', 'far', { transport: 'outbound' }))).toBe('outbound');
    expect(transportLabel(worker('d', 'old'))).toBe('outbound');
    expect(transportIcon(worker('a', 'box', { transport: 'ssh' }))).toBe('key');
    expect(transportIcon(worker('b', 'hub', { transport: 'local' }))).toBe('monitor');
    expect(transportIcon(worker('c', 'far'))).toBe('globe');
    expect(transportHint(worker('a', 'box', { transport: 'ssh', ssh_target: 'me@box' }))).toContain('over SSH (me@box)');
    expect(transportHint(worker('c', 'far', { transport: 'outbound' }))).toContain('machine credential');
  });
});

describe('machine restart state', () => {
  it('says when the hub restarts a daemon that died, and when a host is unreachable', () => {
    const next = iso(NOW + 5 * 60_000);
    const down = worker('a', 'box', { status: 'offline', transport: 'ssh', restart: { state: 'down', attempt: 3, next_at: next, error: 'konsertd is not running here', since: iso(NOW - 600_000) } });
    expect(offlineText(down, NOW)).toBe('konsertd down — restarting (attempt 3, next in 5m): konsertd is not running here');
    // The last start's error wins over the attach error.
    expect(offlineText({ ...down, launch_state: 'failed', launch_error: '[configuring_service] konsertd install failed' }, NOW)).toBe(
      'konsertd down — restarting (attempt 3, next in 5m): [configuring_service] konsertd install failed',
    );
    expect(offlineText({ ...down, launch_state: 'starting' }, NOW)).toBe('konsertd down — restarting (attempt 3)…');
    expect(offlineText(down, NOW + 6 * 60_000)).toBe('konsertd down — restarting (attempt 3, next soon): konsertd is not running here');
    expect(offlineText(worker('b', 'box', { status: 'offline', restart: { state: 'down', error: 'gone', since: iso(NOW) } }), NOW)).toBe('konsertd down: gone');
    expect(offlineText(worker('c', 'far', { status: 'offline', restart: { state: 'unreachable', error: 'ssh: connect to host far port 22: Connection refused', since: iso(NOW) } }), NOW)).toBe(
      'unreachable over SSH — re-dialing: ssh: connect to host far port 22: Connection refused',
    );
    expect(offlineText(worker('d', 'x', { status: 'offline', launch_state: 'starting' }), NOW)).toBe('starting konsertd…');
    expect(offlineText(worker('e', 'x', { status: 'offline' }), NOW)).toBe('offline: not reporting');
  });
});

describe('windows start mode', () => {
  const win = (start_mode?: 'background_task' | 'service' | 'user_session', over: Partial<Worker> = {}) =>
    worker('w', 'minipc', { os: 'windows', ssh_target: 'bryan@minipc', facts: { start_mode } as Worker['facts'], ...over });
  it('says when konsertd starts and what to do for headless use', () => {
    expect(windowsStartText(win('background_task'))).toContain('whether or not anyone is signed in');
    expect(windowsStartText(win('service'))).toContain('Windows service');
    const logon = windowsStartText(win('user_session'));
    expect(logon).toContain('starts only when bryan signs in');
    expect(logon).toContain('administrator');
    expect(logon).toContain('konsertd install --service');
    expect(windowsStartText(win(undefined))).toBe('');
    expect(windowsStartText(worker('l', 'linux'))).toBe('');
  });
  it('explains an offline logon-task machine instead of restarting blindly', () => {
    expect(offlineText(win('user_session', { status: 'offline' }), NOW)).toContain('starts only when bryan signs in');
    const down = win('user_session', { status: 'offline', restart: { state: 'down', attempt: 2, error: 'konsertd is not running here', since: iso(NOW) } });
    expect(offlineText(down, NOW)).toMatch(/^konsertd down: konsertd is not running here\. Windows: konsertd starts only when bryan signs in/);
    expect(offlineText(win('background_task', { status: 'offline' }), NOW)).toBe('offline: not reporting');
  });
});

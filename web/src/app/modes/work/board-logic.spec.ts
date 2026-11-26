import { describe, expect, it } from 'vitest';
import { Project, Task, Ticket, TicketEdge } from '../../core/models/syre.models';
import { WorkItem } from '../../core/state/work-logic';
import { BoardInputs, buildTicketBoard, columnForStatus, defaultLaneBy, descendantWorkTickets, digestLineFromJSON, isRollupBlocked, rollupOf, unfinishedBlockers } from './board-logic';

const project = (id: string, name: string): Project => ({ id, sid: `p-${id}`, name, is_default: false, created_at: '' });

function ticket(partial: Partial<Ticket> & Pick<Ticket, 'id' | 'title'>): Ticket {
  return {
    sid: partial.sid ?? partial.id,
    project_id: 'proj',
    kind: 'feature',
    body: '',
    acceptance: [],
    labels: [],
    priority: 'P2',
    status: 'ready',
    created_at: '2026-11-25T00:00:00Z',
    updated_at: '2026-11-25T00:00:00Z',
    ...partial,
  };
}

function edge(ticketId: string, blockedById: string): TicketEdge {
  return { ticket_id: ticketId, blocked_by_id: blockedById, created_at: '' };
}

function adHoc(partial: Partial<WorkItem> & Pick<WorkItem, 'id' | 'title'>): WorkItem {
  return {
    sid: partial.sid ?? partial.id,
    kind: 'task',
    sessionKind: 'agent',
    agent: 'mock',
    workerId: '',
    machine: 'desk',
    projectKey: 'p:proj',
    projectName: 'Billing',
    status: 'running',
    column: 'running',
    control: 'AUTO',
    live: true,
    stale: false,
    observed: false,
    origin: 'commander',
    goal: '',
    now: 'Working',
    questions: 0,
    hasChanges: false,
    createdAt: '',
    updatedAt: '',
    ...partial,
  };
}

function board(over: Partial<BoardInputs> = {}) {
  return buildTicketBoard({
    tickets: [],
    edges: [],
    digests: {},
    projects: [project('proj', 'Billing')],
    workers: {},
    tasks: [],
    adHoc: [],
    expedited: () => false,
    laneBy: 'epic',
    showBacklog: false,
    showClosed: false,
    ...over,
  });
}

describe('column mapping', () => {
  it('maps status categories onto board columns', () => {
    expect(columnForStatus('backlog')).toBe('backlog');
    expect(columnForStatus('ready')).toBe('queued');
    expect(columnForStatus('running')).toBe('running');
    expect(columnForStatus('needs_you')).toBe('needs_you');
    expect(columnForStatus('review')).toBe('review');
    expect(columnForStatus('done')).toBe('done');
    expect(columnForStatus('failed')).toBe('done');
    expect(columnForStatus('canceled')).toBe('done');
  });

  it('hides backlog and failed/canceled until their toggles are on', () => {
    const tickets = [
      ticket({ id: 'b', title: 'Later', status: 'backlog' }),
      ticket({ id: 'f', title: 'Broke', status: 'failed' }),
      ticket({ id: 'c', title: 'Stopped', status: 'canceled' }),
      ticket({ id: 'd', title: 'Shipped', status: 'done' }),
    ];
    const hidden = board({ tickets, laneBy: 'project' });
    expect(hidden.columns).toEqual(['queued', 'running', 'needs_you', 'review', 'done']);
    const done = hidden.lanes.find((l) => l.key === 'p:proj')!.cells.done.map((c) => c.id);
    expect(done).toEqual(['d']);
    const shown = board({ tickets, laneBy: 'project', showBacklog: true, showClosed: true });
    expect(shown.columns[0]).toBe('backlog');
    expect(shown.lanes.find((l) => l.key === 'p:proj')!.cells.backlog.map((c) => c.id)).toEqual(['b']);
    expect(shown.lanes.find((l) => l.key === 'p:proj')!.cells.done.map((c) => c.id).sort()).toEqual(['c', 'd', 'f']);
  });

  it('leaves containers off the board and keeps ad-hoc work that has no ticket', () => {
    const tickets = [
      ticket({ id: 'epic', title: 'Checkout', kind: 'epic', status: 'ready' }),
      ticket({ id: 'a', title: 'Pay', parent_id: 'epic', status: 'running' }),
    ];
    const linked = adHoc({ id: 'task-a', title: 'Linked', task: { id: 'task-a', ticket_id: 'a' } as Task });
    const free = adHoc({ id: 'task-b', sid: 't-free', title: 'Scratch' });
    const b = board({ tickets, adHoc: [linked, free], laneBy: 'project' });
    const ids = b.lanes.flatMap((l) => Object.values(l.cells).flat().map((c) => c.id));
    expect(ids).toContain('a');
    expect(ids).toContain('task-b');
    expect(ids).not.toContain('epic');
    expect(ids).not.toContain('task-a');
    expect(b.lanes.find((l) => l.name === 'Ad-hoc')).toBeUndefined();
  });
});

describe('blocked detection', () => {
  const a = ticket({ id: 'a', sid: 'tk-a', title: 'A', status: 'ready' });
  const b = ticket({ id: 'b', sid: 'tk-b', title: 'B', status: 'ready' });
  const byId = { a, b };

  it('treats a ready ticket as blocked until every blocker is done', () => {
    const edges = [edge('b', 'a')];
    expect(unfinishedBlockers('b', edges, byId).map((x) => x.sid)).toEqual(['tk-a']);
    expect(isRollupBlocked(b, edges, byId)).toBe(true);
    const queued = board({ tickets: [a, b], edges, laneBy: 'project' }).lanes.find((l) => l.key === 'p:proj')!.cells.queued;
    const card = queued.find((c) => c.id === 'b')!;
    expect(card.blocked).toBe(true);
    expect(card.blockers.map((x) => x.sid)).toEqual(['tk-a']);
    expect(queued.find((c) => c.id === 'a')!.blocked).toBe(false);
  });

  it('clears the block when the blocker is done', () => {
    const doneA = { ...a, status: 'done' as const };
    const edges = [edge('b', 'a')];
    expect(unfinishedBlockers('b', edges, { a: doneA, b })).toEqual([]);
    const queued = board({ tickets: [doneA, b], edges, laneBy: 'project' }).lanes.find((l) => l.key === 'p:proj')!.cells.queued;
    expect(queued.find((c) => c.id === 'b')!.blocked).toBe(false);
  });

  it('counts needs_you and open blockers in a rollup, not closed tickets', () => {
    const ask = ticket({ id: 'n', title: 'Ask', status: 'needs_you' });
    const failed = ticket({ id: 'f', title: 'Fail', status: 'failed' });
    const edges = [edge('b', 'a'), edge('f', 'a')];
    const items = [a, b, ask, failed];
    const by = Object.fromEntries(items.map((t) => [t.id, t]));
    expect(rollupOf(items, edges, by)).toMatchObject({ total: 4, done: 0, running: 0, blocked: 2 });
  });
});

describe('swimlanes and rollups', () => {
  const initiative = ticket({ id: 'init', title: 'Pay down debt', kind: 'initiative' });
  const epic = ticket({ id: 'epic', title: 'Checkout', kind: 'epic', parent_id: 'init' });
  const other = ticket({ id: 'epic2', title: 'Billing', kind: 'epic', parent_id: 'init' });
  const a = ticket({ id: 'a', title: 'A', parent_id: 'epic', status: 'done', acceptance: [{ text: 'ships', done: true }] });
  const b = ticket({ id: 'b', title: 'B', parent_id: 'epic', status: 'running', priority: 'P0', attempt: 2 });
  const c = ticket({ id: 'c', title: 'C', parent_id: 'epic2', status: 'ready' });
  const loose = ticket({ id: 'z', title: 'Loose', status: 'ready' });
  const tickets = [initiative, epic, other, a, b, c, loose];
  const edges = [edge('c', 'a')];

  it('defaults to epic when any epic exists', () => {
    expect(defaultLaneBy(tickets)).toBe('epic');
    expect(defaultLaneBy([loose])).toBe('project');
  });

  it('groups by epic, with P0 in Expedite and a descendant rollup on the lane', () => {
    const b = board({ tickets, edges, laneBy: 'epic' });
    expect(b.lanes[0].name).toBe('Expedite');
    expect(b.lanes[0].cells.running.map((c) => c.id)).toEqual(['b']);
    expect(b.lanes[0].cells.running[0].attempt).toBe(2);
    const checkout = b.lanes.find((l) => l.name === 'Checkout')!;
    expect(checkout.cells.done.map((c) => c.id)).toEqual(['a']);
    expect(checkout.rollup).toMatchObject({ total: 2, done: 1, running: 1, blocked: 0 });
    expect(b.lanes.find((l) => l.name === 'Billing')!.cells.queued.map((c) => c.id)).toEqual(['c']);
    expect(b.lanes.find((l) => l.name === 'No epic')!.cells.queued.map((c) => c.id)).toEqual(['z']);
    expect(descendantWorkTickets('init', tickets).map((t) => t.id).sort()).toEqual(['a', 'b', 'c']);
  });

  it('groups by initiative and by project', () => {
    const byInit = board({ tickets, edges, laneBy: 'initiative' });
    const lane = byInit.lanes.find((l) => l.name === 'Pay down debt')!;
    expect(lane.cells.done.map((c) => c.id)).toEqual(['a']);
    expect(lane.cells.queued.map((c) => c.id)).toEqual(['c']);
    expect(lane.rollup).toMatchObject({ total: 3, done: 1, running: 1, blocked: 0 });
    const byProject = board({ tickets, edges, laneBy: 'project' });
    const proj = byProject.lanes.find((l) => l.name === 'Billing')!;
    expect(proj.rollup).toMatchObject({ total: 4, done: 1, running: 1 });
    expect(Object.values(proj.cells).flat().map((c) => c.id).sort()).toEqual(['a', 'c', 'z']);
  });

  it('puts an expedited ad-hoc card in Expedite and shows lease, digest and acceptance on a ticket', () => {
    const running = ticket({
      id: 'r',
      title: 'Run',
      status: 'running',
      attempt: 3,
      acceptance: [
        { text: 'one', done: true },
        { text: 'two', done: false },
      ],
      lease: { worker_id: 'w1', task_id: 'task-1', expires_at: '' },
    });
    const task = { id: 'task-1', ticket_id: 'r', agent: 'claude', digest_json: '' } as Task;
    const b = board({
      tickets: [running],
      laneBy: 'project',
      workers: { w1: { name: 'desk' } },
      tasks: [task],
      digests: { r: { ticket_id: 'r', summary: 'halfway through the migration', agent: 'claude', machine: 'desk' } },
      adHoc: [adHoc({ id: 'free', title: 'Scratch', column: 'running' })],
      expedited: (id) => id === 'free',
    });
    expect(b.lanes[0].cells.running.map((c) => c.title)).toEqual(['Scratch']);
    const card = b.lanes.find((l) => l.key === 'p:proj')!.cells.running[0];
    expect(card.leaseMachine).toBe('desk');
    expect(card.leaseAgent).toBe('claude');
    expect(card.digest).toBe('halfway through the migration');
    expect(card.acceptanceDone).toBe(1);
    expect(card.acceptanceTotal).toBe(2);
    expect(card.attempt).toBe(3);
  });
});

describe('digest lines', () => {
  it('reads the summary out of digest JSON', () => {
    expect(digestLineFromJSON('t', '{"summary":"landed it","agent":"claude","machine":"desk"}')).toMatchObject({
      summary: 'landed it',
      agent: 'claude',
      machine: 'desk',
    });
    expect(digestLineFromJSON('t', 'not json')).toBeNull();
  });
});

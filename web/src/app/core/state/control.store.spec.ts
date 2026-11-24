import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  Cause,
  FleetEvent,
  Hold,
  UserRequest,
  causeLine,
  isTimelineEvent,
  mergeFleetEvents,
  resourceLabel,
} from '../models/control.models';
import { ApiClientService } from '../services/api-client.service';
import { ControlStore } from './control.store';

const userUI: Cause = { actor: 'user', source: 'ui', confidence: 'certain' };

function hold(id: string, over: Partial<Hold> = {}): Hold {
  return {
    id,
    sid: `h-${id}`,
    scope_kind: 'resource',
    worker_id: 'w1',
    resource: { kind: 'service', name: 'nginx' },
    summary: 'You stopped nginx',
    cause: { actor: 'user', source: 'terminal', confidence: 'high' },
    created_at: `2026-11-23T10:0${id.length}:00Z`,
    ...over,
  };
}

function req(id: string, over: Partial<UserRequest> = {}): UserRequest {
  return {
    id,
    sid: `rq-${id}`,
    worker_id: 'w1',
    description: 'Restart nginx',
    status: 'open',
    created_at: '2026-11-23T10:00:00Z',
    ...over,
  };
}

function ev(id: number, over: Partial<FleetEvent> = {}): FleetEvent {
  return {
    id,
    created_at: '2026-11-23T10:00:00Z',
    kind: 'action',
    action: 'cancel',
    cause: userUI,
    summary: `event ${id}`,
    ...over,
  };
}

describe('control models', () => {
  it('describes causes from the viewer’s perspective', () => {
    expect(causeLine('canceled', userUI)).toBe('Canceled by you via the Konsert UI');
    expect(causeLine('suspended', { actor: 'orchestrator', confidence: 'certain' })).toBe(
      'Suspended by the orchestrator',
    );
    expect(causeLine('failed', { actor: 'user', source: 'terminal', confidence: 'likely' })).toBe(
      'Failed by you via a Konsert terminal (likely)',
    );
    expect(causeLine('canceled', undefined)).toBe('');
  });

  it('labels resources and filters the timeline', () => {
    expect(resourceLabel({ kind: 'service', name: 'nginx' })).toBe('service nginx');
    expect(resourceLabel({ kind: 'port', port: 80 })).toBe('port 80');
    expect(isTimelineEvent(ev(1))).toBe(true);
    expect(
      isTimelineEvent(ev(2, { kind: 'status', cause: { actor: 'agent', confidence: 'certain' } })),
    ).toBe(false);
    expect(isTimelineEvent(ev(3, { kind: 'status', cause: userUI }))).toBe(true);
    expect(
      isTimelineEvent(ev(4, { kind: 'hold', cause: { actor: 'system', confidence: 'certain' } })),
    ).toBe(true);
    expect(
      mergeFleetEvents([ev(1), ev(3)], [ev(2), ev(3, { summary: 'updated' })]).map((e) => [
        e.id,
        e.summary,
      ]),
    ).toEqual([
      [3, 'updated'],
      [2, 'event 2'],
      [1, 'event 1'],
    ]);
  });
});

describe('ControlStore', () => {
  let api: Record<string, ReturnType<typeof vi.fn>>;
  let store: ControlStore;

  beforeEach(() => {
    api = {
      getFleetPause: vi.fn(async () => ({
        paused: true,
        paused_by: 'alice',
        paused_at: '2026-11-23T10:42:00Z',
      })),
      listHolds: vi.fn(async () => ({
        holds: [hold('a'), hold('bb', { released_at: '2026-11-23T11:00:00Z' })],
      })),
      listRequests: vi.fn(async () => ({ requests: [req('r1'), req('r2', { status: 'done' })] })),
      listFleetEvents: vi.fn(async () => ({
        events: [
          ev(5),
          ev(4, { kind: 'status', cause: { actor: 'agent', confidence: 'certain' } }),
        ],
        next_cursor: 4,
      })),
      pauseFleet: vi.fn(async () => ({ paused: true, paused_by: 'me', affected: 2 })),
      resumeFleet: vi.fn(async () => ({ paused: false, affected: 2 })),
      releaseHold: vi.fn(async (id: string) => ({
        ...hold(id),
        released_at: '2026-11-23T12:00:00Z',
      })),
      resolveRequest: vi.fn(async (id: string, status: string) => ({ ...req(id), status })),
      suspendTask: vi.fn(async () => ({ ok: true })),
      vetoChat: vi.fn(async () => {
        throw new Error('chat not found');
      }),
      setWorkerDrained: vi.fn(async () => ({ ok: true, worker_id: 'w1', drained: true })),
      listOverrides: vi.fn(async () => ({
        overrides: [
          { id: 'o1', sid: 'o-1', scope_kind: 'machine_slots', duration_seconds: 1800, reason: 'r', consequences: 'c', status: 'pending', created_at: '2026-11-23T10:00:00Z', use_count: 0 },
          { id: 'o2', sid: 'o-2', scope_kind: 'hold', duration_seconds: 600, reason: 'r', consequences: 'c', status: 'approved', created_at: '2026-11-23T09:00:00Z', use_count: 1 },
        ],
      })),
      listVetoes: vi.fn(async () => ({
        vetoes: [
          { id: 'v1', sid: 'v-1', reason: 'not_now', summary: 's', fleet_event_id: 3, created_at: '2026-11-23T10:00:00Z', status: 'sent', ack_deadline: '2026-11-23T10:10:00Z' },
          { id: 'v2', sid: 'v-2', reason: 'not_now', summary: 's', fleet_event_id: 4, created_at: '2026-11-23T10:01:00Z', status: 'acknowledged', acked_at: '2026-11-23T10:02:00Z', ack_deadline: '2026-11-23T10:11:00Z' },
        ],
      })),
      decideOverride: vi.fn(async (id: string) => ({ id, status: 'denied' })),
    };
    TestBed.configureTestingModule({ providers: [{ provide: ApiClientService, useValue: api }] });
    store = TestBed.inject(ControlStore);
  });

  afterEach(() => TestBed.resetTestingModule());

  it('loads pause, active holds, open requests and timeline events', async () => {
    await store.load();
    expect(store.paused()).toBe(true);
    expect(store.pause().paused_by).toBe('alice');
    expect(store.holds().map((h) => h.id)).toEqual(['a']);
    expect(store.requests().map((r) => r.id)).toEqual(['r1']);
    expect(store.events().map((e) => e.id)).toEqual([5]);
    expect(store.eventsCursor()).toBe(4);
    expect(store.overrides().map((o) => o.id)).toEqual(['o1']);
    expect(store.vetoes().map((v) => v.id)).toEqual(['v1']);
    expect(api['listOverrides']).toHaveBeenCalledWith({ status: 'pending' });
    expect(api['listVetoes']).toHaveBeenCalledWith({ open: true });
    expect(store.attentionCount()).toBe(2);
    expect(api['listHolds']).toHaveBeenCalledWith({ active: true });
    expect(api['listFleetEvents']).toHaveBeenCalledWith(expect.objectContaining({ notable: true }));
  });

  it('applies live hold, request, event and pause updates', () => {
    store.applyHold(hold('a'));
    store.applyHold(hold('b', { scope_kind: 'session', task_id: 't1' }));
    expect(store.holds()).toHaveLength(2);
    expect(store.sessionHold('t1')?.id).toBe('b');
    expect(store.holdsFor('w1')).toHaveLength(2);
    store.applyHold(hold('a', { released_at: '2026-11-23T12:00:00Z' }));
    expect(store.holds().map((h) => h.id)).toEqual(['b']);

    store.applyRequest(req('r1'));
    expect(store.requestsFor('w1')).toHaveLength(1);
    store.applyRequest(req('r1', { status: 'done' }));
    expect(store.requests()).toHaveLength(0);

    store.applyEvent(ev(9));
    store.applyEvent(ev(10, { kind: 'status', cause: { actor: 'agent', confidence: 'certain' } }));
    expect(store.events().map((e) => e.id)).toEqual([9]);

    store.applyPause({ paused: true, paused_by: 'bob' });
    expect(store.paused()).toBe(true);
    store.applyPause({ paused: false });
    expect(store.paused()).toBe(false);
  });

  it('denies an override request with a note', async () => {
    await store.load();
    expect(await store.decideOverride(store.overrides()[0], 'deny', 'not today')).toBe(true);
    expect(api['decideOverride']).toHaveBeenCalledWith('o1', 'deny', 'not today');
    expect(store.overrides()).toEqual([]);
  });

  it('pauses and resumes everything', async () => {
    expect(await store.pauseEverything()).toBe(true);
    expect(store.paused()).toBe(true);
    expect(store.pause().paused_by).toBe('me');
    expect(await store.resumeEverything()).toBe(true);
    expect(store.paused()).toBe(false);
  });

  it('releases holds and resolves requests', async () => {
    store.applyHold(hold('a'));
    store.applyRequest(req('r1'));
    expect(await store.releaseHold(store.holds()[0])).toBe(true);
    expect(api['releaseHold']).toHaveBeenCalledWith('a', undefined);
    expect(store.holds()).toHaveLength(0);
    expect(await store.resolveRequest(store.requests()[0], 'declined', 'no')).toBe(true);
    expect(api['resolveRequest']).toHaveBeenCalledWith('r1', 'declined', 'no');
    expect(store.requests()).toHaveLength(0);
  });

  it('reports failures and clears busy flags', async () => {
    expect(await store.suspendTask('t1')).toBe(true);
    expect(await store.vetoChat('c1', 'dangerous', 'rm -rf')).toBe(false);
    expect(store.error()).toBe('chat not found');
    expect(store.isBusy('c1')).toBe(false);
    expect(await store.setDrained('w1', true)).toBe(true);
    expect(api['setWorkerDrained']).toHaveBeenCalledWith('w1', true);
  });
});

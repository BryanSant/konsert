import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { TaskEvent } from '../models/konsert.models';
import { ApiClientService } from '../services/api-client.service';
import { MAX_EVENTS_PER_TASK, TelemetryStore, mergeEvents } from './telemetry.store';

function ev(id: number, taskId = 't1', message = `e${id}`): TaskEvent {
  return {
    id,
    task_id: taskId,
    kind: 'log',
    message,
    created_at: new Date(1_700_000_000_000 + id * 1000).toISOString(),
  } as TaskEvent;
}

describe('TelemetryStore', () => {
  let getTaskEvents: ReturnType<typeof vi.fn>;
  let store: TelemetryStore;

  beforeEach(() => {
    getTaskEvents = vi.fn(async () => ({ events: [] as TaskEvent[] }));
    TestBed.configureTestingModule({
      providers: [{ provide: ApiClientService, useValue: { getTaskEvents } }],
    });
    store = TestBed.inject(TelemetryStore);
  });

  afterEach(() => TestBed.resetTestingModule());

  it('ignores live events for tasks nobody has hydrated', () => {
    store.appendEvent(ev(1, 'unviewed'));
    expect(store.eventsByTask()['unviewed']).toBeUndefined();
  });

  it('hydrates once and appends monotonic live events in order', async () => {
    getTaskEvents.mockResolvedValueOnce({ events: [ev(1), ev(2)] });
    await store.hydrateTask('t1');
    await store.hydrateTask('t1');
    expect(getTaskEvents).toHaveBeenCalledTimes(1);

    store.appendEvent(ev(3));
    store.appendEvent(ev(3)); // duplicate delivery
    expect(store.eventsByTask()['t1'].map((e) => e.id)).toEqual([1, 2, 3]);
  });

  it('merges out-of-order events into position', async () => {
    getTaskEvents.mockResolvedValueOnce({ events: [ev(1), ev(5)] });
    await store.hydrateTask('t1');
    store.appendEvent(ev(3));
    expect(store.eventsByTask()['t1'].map((e) => e.id)).toEqual([1, 3, 5]);
  });

  it('keeps live events that arrive while hydration is in flight', async () => {
    let resolve: (v: { events: TaskEvent[] }) => void = () => undefined;
    getTaskEvents.mockImplementationOnce(() => new Promise((r) => (resolve = r)));
    const p = store.hydrateTask('t1');
    store.appendEvent(ev(4));
    resolve({ events: [ev(1), ev(2), ev(3)] });
    await p;
    expect(store.eventsByTask()['t1'].map((e) => e.id)).toEqual([1, 2, 3, 4]);
  });

  it('caps per-task history', async () => {
    const many = Array.from({ length: MAX_EVENTS_PER_TASK + 50 }, (_, i) => ev(i + 1));
    getTaskEvents.mockResolvedValueOnce({ events: many });
    await store.hydrateTask('t1');
    store.appendEvent(ev(MAX_EVENTS_PER_TASK + 51));
    const list = store.eventsByTask()['t1'];
    expect(list.length).toBe(MAX_EVENTS_PER_TASK);
    expect(list[list.length - 1].id).toBe(MAX_EVENTS_PER_TASK + 51);
    expect(list[0].id).toBe(52);
  });

  it('fills the gap after a reconnect using after_id', async () => {
    getTaskEvents.mockResolvedValueOnce({ events: [ev(1), ev(2)] });
    await store.hydrateTask('t1');

    getTaskEvents.mockResolvedValueOnce({ events: [ev(3), ev(4)] });
    await store.onReconnected();
    expect(getTaskEvents).toHaveBeenLastCalledWith('t1', 2);
    expect(store.eventsByTask()['t1'].map((e) => e.id)).toEqual([1, 2, 3, 4]);
  });

  it('retries hydration after a failed fetch', async () => {
    getTaskEvents.mockRejectedValueOnce(new Error('offline'));
    await store.hydrateTask('t1');
    getTaskEvents.mockResolvedValueOnce({ events: [ev(1)] });
    await store.hydrateTask('t1');
    expect(store.eventsByTask()['t1'].map((e) => e.id)).toEqual([1]);
  });

  it('mergeEvents dedupes id-less events by content', () => {
    const a = { ...ev(0), message: 'same' };
    expect(mergeEvents([a], [{ ...a }]).length).toBe(1);
  });
});

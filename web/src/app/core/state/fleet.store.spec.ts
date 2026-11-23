import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FleetSnapshotResponse, Task, Worker } from '../models/konsert.models';
import { ApiClientService } from '../services/api-client.service';
import { FleetStore } from './fleet.store';

function task(id: string, status: Task['status']): Task {
  return { id, worker_id: 'w1', agent: 'claude', status, created_at: '' } as Task;
}

function worker(id: string, status: Worker['status']): Worker {
  return { id, name: id, host_group: id, status } as Worker;
}

describe('FleetStore snapshot refresh vs live updates', () => {
  let resolvers: Array<(snap: FleetSnapshotResponse) => void>;
  let store: FleetStore;

  beforeEach(() => {
    resolvers = [];
    TestBed.configureTestingModule({
      providers: [
        {
          provide: ApiClientService,
          useValue: {
            getFleet: vi.fn(() => new Promise<FleetSnapshotResponse>((r) => resolvers.push(r))),
          },
        },
      ],
    });
    store = TestBed.inject(FleetStore);
  });

  afterEach(() => TestBed.resetTestingModule());

  it('replays live upserts that arrived while the snapshot was in flight', async () => {
    const p = store.refresh();
    store.upsertTask(task('t1', 'completed'));
    store.upsertWorker(worker('w1', 'offline'));

    // The (older) snapshot still has the task running and the worker online.
    resolvers[0]({
      workers: [worker('w1', 'online')],
      tasks: [task('t1', 'running'), task('t2', 'queued')],
    } as FleetSnapshotResponse);
    await p;

    expect(store.tasksById()['t1'].status).toBe('completed');
    expect(store.tasksById()['t2'].status).toBe('queued');
    expect(store.workersById()['w1'].status).toBe('offline');
    expect(store.loading()).toBe(false);
  });

  it('replays live deletions too', async () => {
    store.upsertWorker(worker('w2', 'online'));
    const p = store.refresh();
    store.removeWorker('w2');
    resolvers[0]({
      workers: [worker('w2', 'online')],
      tasks: [],
    } as unknown as FleetSnapshotResponse);
    await p;
    expect(store.workersById()['w2']).toBeUndefined();
  });

  it('handles overlapping refreshes, each replaying only ops since it started', async () => {
    const first = store.refresh();
    store.upsertTask(task('t1', 'running'));
    const second = store.refresh();
    store.upsertTask(task('t1', 'completed'));

    resolvers[1]({
      workers: [],
      tasks: [task('t1', 'running')],
    } as unknown as FleetSnapshotResponse);
    await second;
    expect(store.tasksById()['t1'].status).toBe('completed');
    expect(store.loading()).toBe(true);

    resolvers[0]({ workers: [], tasks: [] } as unknown as FleetSnapshotResponse);
    await first;
    expect(store.tasksById()['t1'].status).toBe('completed');
    expect(store.loading()).toBe(false);
  });

  it('does not record ops when no refresh is pending', async () => {
    store.upsertTask(task('t1', 'running'));
    const p = store.refresh();
    resolvers[0]({ workers: [], tasks: [] } as unknown as FleetSnapshotResponse);
    await p;
    expect(store.tasksById()['t1']).toBeUndefined();
  });
});

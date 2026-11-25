import { TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { Task } from '../models/syre.models';
import { TakenControlStore } from './taken-control.store';

const task = (id: string, status: Task['status'], agent = 'shell'): Task =>
  ({ id, sid: `t-${id}`, worker_id: 'w1', agent, status, prompt: '', workdir: '~', effective_dir: '', created_at: '' }) as Task;

describe('TakenControlStore', () => {
  let store: TakenControlStore;
  beforeEach(() => {
    store = TestBed.inject(TakenControlStore);
  });

  it('toggles control per task', () => {
    expect(store.isInteractive('a')).toBe(false);
    store.toggleInteractiveMode('a');
    expect(store.isInteractive('a')).toBe(true);
    store.setInteractiveMode('a', false);
    expect(store.takenControl()).toEqual({});
  });

  it('reports a shell that exits while watched (also on a non-zero exit) and hands it back', () => {
    store.exitedShells([task('s', 'running'), task('f', 'running')]);
    store.setInteractiveMode('s', true);
    expect(store.exitedShells([task('s', 'completed'), task('f', 'failed')])).toEqual(['s', 'f']);
    expect(store.isInteractive('s')).toBe(false);
  });

  it('ignores shells that were already finished and agent sessions', () => {
    expect(store.exitedShells([task('done', 'completed')])).toEqual([]);
    store.exitedShells([task('a', 'running', 'claude')]);
    expect(store.exitedShells([task('a', 'completed', 'claude')])).toEqual([]);
  });
});

import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { GitInspectResult, Task } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { FleetStore } from '../core/state/fleet.store';
import { TabsStore } from '../core/state/tabs.store';
import { TelemetryDrawerComponent } from './telemetry-drawer.component';

function task(id: string, status: Task['status'] = 'running'): Task {
  return {
    id,
    worker_id: 'w1',
    agent: 'claude',
    status,
    workdir: `/src/${id}`,
    created_at: new Date().toISOString(),
  } as Task;
}

function git(branch: string): GitInspectResult {
  return { is_git_repo: true, branch, is_worktree: false, added: 1, deleted: 0, files: [] };
}

describe('TelemetryDrawerComponent git inspector', () => {
  let pending: Array<{ taskId?: string; resolve: (r: GitInspectResult) => void }>;
  let inspectGit: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.useFakeTimers();
    pending = [];
    inspectGit = vi.fn(
      (params: { taskId?: string }) =>
        new Promise<GitInspectResult>((resolve) =>
          pending.push({ taskId: params.taskId, resolve }),
        ),
    );
    TestBed.configureTestingModule({
      providers: [
        {
          provide: ApiClientService,
          useValue: { inspectGit, getTaskEvents: vi.fn(async () => ({ events: [] })) },
        },
      ],
    });
  });

  afterEach(() => {
    vi.useRealTimers();
    TestBed.resetTestingModule();
  });

  function setup() {
    const fleet = TestBed.inject(FleetStore);
    const tabs = TestBed.inject(TabsStore);
    fleet.upsertTask(task('a'));
    fleet.upsertTask(task('b'));
    tabs.openTaskTab(fleet.tasksById()['a'], 'w1');
    const fixture = TestBed.createComponent(TelemetryDrawerComponent);
    fixture.componentRef.setInput('panel', 'git');
    fixture.detectChanges();
    TestBed.tick();
    return { fleet, tabs, fixture, cmp: fixture.componentInstance };
  }

  it("never shows a previous task's diff after switching tasks", async () => {
    const { fleet, tabs, cmp } = setup();
    expect(pending.map((p) => p.taskId)).toEqual(['a']);

    tabs.openTaskTab(fleet.tasksById()['b'], 'w1');
    TestBed.tick();
    expect(pending.map((p) => p.taskId)).toEqual(['a', 'b']);

    pending[1].resolve(git('branch-b'));
    await vi.waitFor(() => expect(cmp.liveGit()?.branch).toBe('branch-b'));
    pending[0].resolve(git('branch-a')); // stale, resolves last
    await Promise.resolve();
    await Promise.resolve();
    expect(cmp.liveGit()?.branch).toBe('branch-b');
  });

  it('clears the previous diff immediately when the task changes', async () => {
    const { fleet, tabs, cmp } = setup();
    pending[0].resolve(git('branch-a'));
    await vi.waitFor(() => expect(cmp.liveGit()?.branch).toBe('branch-a'));
    tabs.openTaskTab(fleet.tasksById()['b'], 'w1');
    TestBed.tick();
    expect(cmp.liveGit()).toBeNull();
  });

  it('does not restart polling when the active task object is merely updated', () => {
    const { fleet } = setup();
    expect(inspectGit).toHaveBeenCalledTimes(1);
    fleet.upsertTask({ ...task('a'), status: 'waiting_input' });
    fleet.upsertTask({ ...task('a'), status: 'running' });
    TestBed.tick();
    expect(inspectGit).toHaveBeenCalledTimes(1);
  });

  it('polls silently on an interval and stops when disabled', () => {
    const { fixture } = setup();
    vi.advanceTimersByTime(6000);
    expect(inspectGit).toHaveBeenCalledTimes(2);
    fixture.componentRef.setInput('enabled', false);
    TestBed.tick();
    vi.advanceTimersByTime(30000);
    expect(inspectGit).toHaveBeenCalledTimes(2);
  });
});

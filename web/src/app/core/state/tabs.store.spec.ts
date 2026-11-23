import { TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { Task, TaskStatus } from '../models/konsert.models';
import { ORCHESTRATOR_TAB_ID, TABS_STORAGE_KEY, TabsStore } from './tabs.store';

function task(id: string, agent: string, status: TaskStatus): Task {
  return {
    id,
    worker_id: 'w1',
    agent,
    workdir: '/home/dev/konsert',
    mode: 'interactive',
    origin: 'direct',
    permission_mode: 'full_access',
    orphan_policy: 'fail',
    prompt: '',
    status,
    summary: '',
    created_at: new Date().toISOString(),
  } as Task;
}

describe('TabsStore.closeExitedShells', () => {
  let tabs: TabsStore;

  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({});
    tabs = TestBed.inject(TabsStore);
  });

  it('closes a shell tab when its shell exits while open', () => {
    const shell = task('sh-1', 'shell', 'running');
    tabs.openTaskTab(shell);
    tabs.closeExitedShells([shell]);
    expect(tabs.openTabs().some((t) => t.id === 'sh-1')).toBe(true);

    tabs.closeExitedShells([{ ...shell, status: 'completed' }]);
    expect(tabs.openTabs().some((t) => t.id === 'sh-1')).toBe(false);
    expect(tabs.activeTabId()).toBe(ORCHESTRATOR_TAB_ID);
  });

  it('also closes on a non-zero exit (exit after a failing command)', () => {
    const shell = task('sh-2', 'shell', 'running');
    tabs.openTaskTab(shell);
    tabs.closeExitedShells([shell]);
    tabs.closeExitedShells([{ ...shell, status: 'failed' }]);
    expect(tabs.openTabs().some((t) => t.id === 'sh-2')).toBe(false);
  });

  it('keeps an already-finished shell open when the user opens it to read output', () => {
    const done = task('sh-3', 'shell', 'completed');
    tabs.closeExitedShells([done]);
    tabs.openTaskTab(done);
    tabs.closeExitedShells([done]);
    expect(tabs.openTabs().some((t) => t.id === 'sh-3')).toBe(true);
  });

  it('keeps agent session tabs open after they finish', () => {
    const agent = task('ag-1', 'claude', 'running');
    tabs.openTaskTab(agent);
    tabs.closeExitedShells([agent]);
    tabs.closeExitedShells([{ ...agent, status: 'completed' }]);
    expect(tabs.openTabs().some((t) => t.id === 'ag-1')).toBe(true);
  });

  it('closes the scratch panel when its shell exits', () => {
    const shell = task('sh-4', 'shell', 'running');
    tabs.scratchTaskId.set('sh-4');
    tabs.scratchOpen.set(true);
    tabs.closeExitedShells([shell]);
    tabs.closeExitedShells([{ ...shell, status: 'completed' }]);
    expect(tabs.scratchTaskId()).toBeNull();
    expect(tabs.scratchOpen()).toBe(false);
  });
});

describe('TabsStore persistence', () => {
  beforeEach(() => {
    localStorage.clear();
    TestBed.resetTestingModule();
  });

  function freshStore(): TabsStore {
    TestBed.resetTestingModule();
    TestBed.configureTestingModule({});
    return TestBed.inject(TabsStore);
  }

  it('restores open tabs, the active tab and the split after a reload', () => {
    const tabs = freshStore();
    tabs.openTaskTab(task('t-direct', 'shell', 'running'));
    tabs.openTaskTab({ ...task('t-orch', 'claude', 'running'), origin: 'orchestrator' });
    tabs.setInteractiveMode('t-orch', true);
    tabs.toggleSplit('t-direct');
    tabs.setSplitRatio(0.6);
    TestBed.tick();

    const reloaded = freshStore();
    expect(reloaded.openTabs().map((t) => t.id)).toEqual([ORCHESTRATOR_TAB_ID, 't-direct', 't-orch']);
    expect(reloaded.activeTabId()).toBe('t-orch');
    expect(reloaded.splitTabId()).toBe('t-direct');
    expect(reloaded.splitRatio()).toBe(0.6);
  });

  it('never restores Take Control: tabs reopen in their default mode', () => {
    const tabs = freshStore();
    tabs.openTaskTab({ ...task('t-orch', 'claude', 'running'), origin: 'orchestrator' });
    tabs.setInteractiveMode('t-orch', true);
    TestBed.tick();

    const reloaded = freshStore();
    expect(reloaded.openTabs().find((t) => t.id === 't-orch')?.interactiveMode).toBe(false);
  });

  it('prunes restored tabs whose task no longer exists', () => {
    const tabs = freshStore();
    tabs.openTaskTab(task('t-keep', 'shell', 'running'));
    tabs.openTaskTab(task('t-gone', 'shell', 'running'));
    tabs.scratchTaskId.set('t-scratch-gone');
    TestBed.tick();

    const reloaded = freshStore();
    reloaded.pruneMissingTasks([task('t-keep', 'shell', 'running')]);
    expect(reloaded.openTabs().map((t) => t.id)).toEqual([ORCHESTRATOR_TAB_ID, 't-keep']);
    expect(reloaded.scratchTaskId()).toBeNull();
  });

  it('ignores corrupt saved state', () => {
    localStorage.setItem(TABS_STORAGE_KEY, '{not json');
    expect(freshStore().openTabs().map((t) => t.id)).toEqual([ORCHESTRATOR_TAB_ID]);
  });
});

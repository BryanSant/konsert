import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { Task, Worker } from '../models/konsert.models';
import { FleetStore } from './fleet.store';
import { ORCHESTRATOR_TAB_ID, TabsStore } from './tabs.store';

describe('FleetStore and TabsStore (Zoneless Signals)', () => {
  it('groups workers by host_group, aggregates listening ports, and builds live agent roster', () => {
    TestBed.configureTestingModule({});
    const fleet = TestBed.inject(FleetStore);

    const w1: Worker = {
      id: 'w1',
      name: 'bocabox',
      host_group: 'fedora-main',
      os: 'linux',
      arch: 'amd64',
      isolation: 'host',
      permission_mode: 'full_access',
      agents: ['claude', 'agy', 'shell'],
      tags: {},
      max_concurrent: 2,
      status: 'online',
      last_seen_at: new Date().toISOString(),
      metrics: {
        cpu_percent: 25,
        mem_used_mb: 4096,
        mem_total_mb: 16384,
        load_avg: 1.2,
        listening_ports: [{ port: 8420, process: 'konsert', address: '127.0.0.1' }],
      },
    };
    const w2: Worker = {
      ...w1,
      id: 'w2',
      name: 'daybreak',
      status: 'offline',
    };
    fleet.upsertWorker(w1);
    fleet.upsertWorker(w2);

    const task: Task = {
      id: 'task-1',
      worker_id: 'w1',
      agent: 'claude',
      workdir: '/home/dev/konsert',
      effective_dir: '/home/dev/konsert',
      branch: 'feat/demo',
      mode: 'oneshot',
      origin: 'orchestrator',
      permission_mode: 'full_access',
      orphan_policy: 'fail',
      prompt: 'test',
      status: 'running',
      summary: '',
      created_at: new Date().toISOString(),
    };
    fleet.upsertTask(task);

    expect(fleet.onlineWorkersCount()).toBe(1);
    expect(fleet.activeTasksCount()).toBe(1);
    expect(fleet.listeningPorts().length).toBe(1);
    expect(fleet.listeningPorts()[0].port).toBe(8420);
    expect(fleet.agentsRoster().length).toBe(1);
    expect(fleet.agentsRoster()[0].workerName).toBe('bocabox');
  });

  it('defaults Orchestrated tabs to Read-Only and Direct sessions to Interactive', () => {
    TestBed.configureTestingModule({});
    const tabs = TestBed.inject(TabsStore);

    expect(tabs.activeTabId()).toBe(ORCHESTRATOR_TAB_ID);
    tabs.closeTab(ORCHESTRATOR_TAB_ID);
    expect(tabs.openTabs().length).toBe(1);

    const orchTask: Task = {
      id: 'task-orch',
      worker_id: 'w1',
      agent: 'agy',
      workdir: '/repo',
      effective_dir: '/repo',
      branch: '',
      mode: 'oneshot',
      origin: 'orchestrator',
      permission_mode: 'full_access',
      orphan_policy: 'fail',
      prompt: 'hello',
      status: 'running',
      summary: '',
      created_at: new Date().toISOString(),
    };
    tabs.openTaskTab(orchTask, 'mac-studio');
    expect(tabs.activeTab()?.interactiveMode).toBe(false);

    const directTask: Task = {
      ...orchTask,
      id: 'task-direct',
      agent: 'shell',
      mode: 'interactive',
      origin: 'direct',
    };
    tabs.openTaskTab(directTask, 'mac-studio');
    expect(tabs.activeTab()?.interactiveMode).toBe(true);

    tabs.closeTab('task-direct');
    expect(tabs.activeTabId()).toBe('task-orch');
  });

  it('scopes visible tabs to active Space when multiple tabs share a Space', () => {
    TestBed.configureTestingModule({});
    const fleet = TestBed.inject(FleetStore);
    const tabs = TestBed.inject(TabsStore);

    const space = {
      id: 'sp-1',
      worker_id: 'w1',
      name: 'konsert',
      path: '/home/dev/konsert',
      created_at: new Date().toISOString(),
    };
    fleet.upsertSpace(space);

    const shellTask: Task = {
      id: 't-shell',
      worker_id: 'w1',
      space_id: 'sp-1',
      agent: 'shell',
      workdir: '/home/dev/konsert',
      effective_dir: '/home/dev/konsert',
      branch: '',
      mode: 'interactive',
      origin: 'direct',
      permission_mode: 'full_access',
      orphan_policy: 'fail',
      prompt: '',
      status: 'running',
      summary: '',
      created_at: new Date().toISOString(),
    };
    const claudeTask: Task = { ...shellTask, id: 't-claude', agent: 'claude' };

    tabs.selectSpace(space, [shellTask, claudeTask], 'bocabox');
    expect(tabs.activeSpaceId()).toBe('sp-1');
    expect(tabs.visibleTabs().length).toBe(3); // Orchestrator + shell + claude
    expect(tabs.activeTabId()).toBe('t-shell');
  });
});

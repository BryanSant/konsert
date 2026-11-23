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

    // Plain shell sessions must NOT appear in the lower-left Agents roster
    const shellTask: Task = {
      ...task,
      id: 'task-shell-1',
      agent: 'shell',
      origin: 'direct',
    };
    fleet.upsertTask(shellTask);

    expect(fleet.onlineWorkersCount()).toBe(1);
    expect(fleet.activeTasksCount()).toBe(2);
    expect(fleet.listeningPorts().length).toBe(1);
    expect(fleet.listeningPorts()[0].port).toBe(8420);
    expect(fleet.agentsRoster().length).toBe(1);
    expect(fleet.agentsRoster()[0].task.agent).toBe('claude');
    expect(fleet.agentsRoster()[0].workerName).toBe('bocabox');

    // removeWorker removes worker and its spaces
    fleet.upsertSpace({
      id: 'sp-w1',
      worker_id: 'w1',
      name: 'konsert',
      path: '/home/dev/konsert',
      created_at: new Date().toISOString(),
    });
    fleet.removeWorker('w1');
    expect(fleet.workersById()['w1']).toBeUndefined();
    expect(fleet.spaces().length).toBe(0);
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

  it('resolves unified Chat Session Machine/Agent/Model via default -> last used -> guess cascade', () => {
    TestBed.configureTestingModule({});
    const fleet = TestBed.inject(FleetStore);

    const localW: Worker = {
      id: 'w-local',
      name: 'localhost',
      host_group: 'localhost',
      os: 'darwin',
      arch: 'arm64',
      isolation: 'host',
      permission_mode: 'full_access',
      agents: ['shell', 'claude', 'agy'],
      tags: { local: 'true' },
      max_concurrent: 4,
      status: 'online',
      last_seen_at: new Date().toISOString(),
    };
    const remoteW: Worker = {
      ...localW,
      id: 'w-remote',
      name: 'bocabox',
      host_group: 'homelab',
      tags: {},
      agents: ['shell', 'grok'],
    };
    fleet.upsertWorker(remoteW);
    fleet.upsertWorker(localW);

    // 1. First-run guess prefers localhost + preferred installed agent (agy) + guessed model
    const r1 = fleet.resolveSessionDefaults();
    expect(r1.machine_id).toBe('w-local');
    expect(r1.machine_source).toBe('localhost');
    expect(r1.agent).toBe('agy');
    expect(r1.agent_source).toBe('guess');
    expect(r1.model).toBe('gemini-2.5-pro');
    expect(r1.model_source).toBe('guess');

    // 2. Last-used overrides guess
    fleet.settings.update((s) => ({ ...s, last_used_agent: 'claude', last_used_model: 'claude-opus-4-1' }));
    const r2 = fleet.resolveSessionDefaults();
    expect(r2.agent).toBe('claude');
    expect(r2.agent_source).toBe('last_used');
    expect(r2.model).toBe('claude-opus-4-1');
    expect(r2.model_source).toBe('last_used');

    // 3. Configured default overrides last-used
    fleet.settings.update((s) => ({ ...s, default_agent: 'agy', default_model: 'gemini-2.5-flash' }));
    const r3 = fleet.resolveSessionDefaults();
    expect(r3.agent).toBe('agy');
    expect(r3.agent_source).toBe('default');
    expect(r3.model).toBe('gemini-2.5-flash');
    expect(r3.model_source).toBe('default');

    // 4. Taxonomy cascading removal
    fleet.upsertWorkspace({ id: 'ws-1', name: 'Work', is_default: false, created_at: new Date().toISOString() });
    fleet.upsertProject({ id: 'p-1', workspace_id: 'ws-1', name: 'App1', is_default: false, created_at: new Date().toISOString() });
    fleet.upsertRepo({ id: 'r-1', project_id: 'p-1', worker_id: 'w-local', name: 'api', path: '/api', created_at: new Date().toISOString() });
    fleet.upsertWorktree({ id: 'wt-1', repo_id: 'r-1', branch: 'main', path: '/api', is_primary: true, created_at: new Date().toISOString() });
    fleet.removeWorkspace('ws-1');
    expect(fleet.workspaces().length).toBe(0);
    expect(fleet.projects().length).toBe(0);
    expect(fleet.repos().length).toBe(0);
    expect(fleet.worktrees().length).toBe(0);
  });
});

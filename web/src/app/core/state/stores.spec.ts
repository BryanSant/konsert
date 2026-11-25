import { TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import {
  extractTaskIdsFromToolPayload,
  parseUserMessageWithAttachments,
} from '../../features/chat/legacy-adapter';
import { parseMarkdown } from '../../shared/markdown-view.component';
import { Task, Worker } from '../models/syre.models';
import { FleetStore } from './fleet.store';

describe('FleetStore (Zoneless Signals)', () => {
  beforeEach(() => {
    localStorage.clear();
    TestBed.resetTestingModule();
  });

  it('tracks workers and tasks, and removing a worker drops its spaces', () => {
    TestBed.configureTestingModule({});
    const fleet = TestBed.inject(FleetStore);

    const w1: Worker = {
      id: 'w1',
      sid: 'w1-sid',
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
      sid: 'task-1-sid',
      worker_id: 'w1',
      agent: 'claude',
      workdir: '/home/dev/syre',
      effective_dir: '/home/dev/syre',
      branch: 'feat/demo',
      mode: 'oneshot',
      origin: 'commander',
      permission_mode: 'full_access',
      orphan_policy: 'fail',
      prompt: 'test',
      status: 'running',
      summary: '',
      created_at: new Date().toISOString(),
    };
    fleet.upsertTask(task);

    const shellTask: Task = {
      ...task,
      id: 'task-shell-1',
      agent: 'shell',
      origin: 'direct',
    };
    fleet.upsertTask(shellTask);

    expect(fleet.onlineWorkersCount()).toBe(1);
    expect(Object.keys(fleet.tasksById())).toHaveLength(2);

    // removeWorker removes worker and its spaces
    fleet.upsertSpace({
      id: 'sp-w1',
      sid: 'sp-w1-sid',
      worker_id: 'w1',
      name: 'syre',
      path: '/home/dev/syre',
      created_at: new Date().toISOString(),
    });
    fleet.removeWorker('w1');
    expect(fleet.workersById()['w1']).toBeUndefined();
    expect(fleet.spaces().length).toBe(0);
  });

  it('resolves unified Chat Session Machine/Agent/Model via default -> last used -> guess cascade', () => {
    TestBed.configureTestingModule({});
    const fleet = TestBed.inject(FleetStore);

    const localW: Worker = {
      id: 'w-local',
      sid: 'w-local-sid',
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

    // 1. First-run guess prefers localhost + preferred installed agent (agy) (model left to the account default)
    const r1 = fleet.resolveSessionDefaults();
    expect(r1.machine_id).toBe('w-local');
    expect(r1.machine_source).toBe('localhost');
    expect(r1.agent).toBe('agy');
    expect(r1.agent_source).toBe('guess');
    // No guessed model: the agent account's own default applies.
    expect(r1.model).toBe('');
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
    fleet.upsertWorkspace({ id: 'ws-1', sid: 'ws-1-sid', name: 'Work', is_default: false, created_at: new Date().toISOString() });
    fleet.upsertProject({ id: 'p-1', sid: 'p-1-sid', workspace_id: 'ws-1', name: 'App1', is_default: false, created_at: new Date().toISOString() });
    fleet.upsertRepo({ id: 'r-1', sid: 'r-1-sid', project_id: 'p-1', worker_id: 'w-local', name: 'api', path: '/api', created_at: new Date().toISOString() });
    fleet.upsertWorktree({ id: 'wt-1', sid: 'wt-1-sid', repo_id: 'r-1', branch: 'main', path: '/api', is_primary: true, created_at: new Date().toISOString() });
    fleet.removeWorkspace('ws-1');
    expect(fleet.workspaces().length).toBe(0);
    expect(fleet.projects().length).toBe(0);
    expect(fleet.repos().length).toBe(0);
    expect(fleet.worktrees().length).toBe(0);
  });

  it('parses markdown, attachments and tool card task ids', () => {
    const blocks = parseMarkdown('# Summary\n\n- Check `main.go`\n\n```go\nfmt.Println("ok")\n```');
    expect(blocks.map((b) => b.kind)).toEqual(['h1', 'ul', 'code']);
    expect(blocks[2].lang).toBe('go');

    const parsedMsg = parseUserMessageWithAttachments(
      'Please inspect this log\n\n[Attached files on host]\n- /var/tmp/syre-attachments/sess1-1700000000-abcd1234-error.log'
    );
    expect(parsedMsg.text).toBe('Please inspect this log');
    expect(parsedMsg.files).toEqual(['error.log']);

    const ids = extractTaskIdsFromToolPayload(
      JSON.stringify({ tasks: [{ task_id: 'task-aaa111' }, { id: 'task-bbb222', agent: 'claude' }] })
    );
    expect(ids).toEqual(['task-aaa111', 'task-bbb222']);
  });
});

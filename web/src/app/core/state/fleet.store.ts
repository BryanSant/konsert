import { Injectable, computed, inject, signal } from '@angular/core';
import {
  AppSettings,
  Project,
  Repo,
  SessionDefaults,
  Space,
  Task,
  UsageSummary,
  Worker,
  Workspace,
  Worktree,
} from '../models/konsert.models';
import { WorkerHeat } from '../models/activity.models';
import { ApiClientService } from '../services/api-client.service';
import { ActivityStore } from './activity.store';
import { ControlStore } from './control.store';

@Injectable({ providedIn: 'root' })
export class FleetStore {
  private readonly api = inject(ApiClientService);
  private readonly control = inject(ControlStore);
  private readonly activity = inject(ActivityStore);

  readonly workers = signal<Worker[]>([]);
  /** Hub build version from GET /api/fleet, for worker version-mismatch warnings. */
  readonly controllerVersion = signal('');
  readonly tasks = signal<Task[]>([]);
  readonly spaces = signal<Space[]>([]);
  readonly workspaces = signal<Workspace[]>([]);
  readonly projects = signal<Project[]>([]);
  readonly repos = signal<Repo[]>([]);
  readonly worktrees = signal<Worktree[]>([]);
  readonly settings = signal<AppSettings>({
    llm_provider: 'openai',
    llm_model: '',
    llm_base_url: '',
    llm_api_key_set: false,
    default_agent: '',
    default_model: '',
    last_used_machine: '',
    last_used_agent: '',
    last_used_model: '',
    default_permission_mode: 'full_access',
    terminal_font_size: 13,
    notify_sound: true,
    notify_desktop: false,
  });
  readonly usage = signal<UsageSummary>({
    total_input_tokens: 0,
    total_output_tokens: 0,
    total_requests: 0,
    by_provider: [],
  });
  readonly loading = signal(false);

  /**
   * Live (WebSocket) mutations applied while a snapshot refresh is in flight. Each
   * refresh replays the ops recorded after it started on top of its snapshot, so a
   * slower HTTP response never reverts newer fleet.update frames.
   */
  private liveLog: Array<() => void> = [];
  private refreshesInFlight = 0;
  private refreshSeq = 0;
  private appliedRefreshSeq = 0;

  readonly workersById = computed(() => {
    const map: Record<string, Worker> = {};
    for (const w of this.workers()) {
      map[w.id] = w;
    }
    return map;
  });

  readonly tasksById = computed(() => {
    const map: Record<string, Task> = {};
    for (const t of this.tasks()) {
      map[t.id] = t;
    }
    return map;
  });

  /**
   * Machine heat per online worker: the hub's smoothed 0..1 score with its level (cool
   * < 0.4, warm 0.4-0.75, hot > 0.75) and dominant cause (cpu, mem, gpu, slots).
   */
  readonly heatByWorker = computed(() => {
    const map: Record<string, WorkerHeat> = {};
    for (const w of this.workers()) {
      if (w.status === 'online' && w.heat) map[w.id] = w.heat;
    }
    return map;
  });

  readonly onlineWorkersCount = computed(() =>
    this.workers().filter((w) => w.status === 'online').length
  );

  /** Applies a live mutation now and records it for replay if a refresh is pending. */
  private live(op: () => void): void {
    op();
    if (this.refreshesInFlight > 0) this.liveLog.push(op);
  }

  async refresh(): Promise<void> {
    this.loading.set(true);
    this.refreshesInFlight++;
    const logStart = this.liveLog.length;
    const seq = ++this.refreshSeq;
    try {
      const snap = await this.api.getFleet();
      // A newer refresh already landed; this snapshot is older than what we show.
      if (seq < this.appliedRefreshSeq) return;
      this.appliedRefreshSeq = seq;
      this.workers.set(snap.workers ?? []);
      this.controllerVersion.set(snap.controller_version ?? '');
      this.tasks.set(snap.tasks ?? []);
      if (snap.spaces) {
        this.spaces.set(snap.spaces);
      }
      if (snap.taxonomy) {
        this.workspaces.set(snap.taxonomy.workspaces ?? []);
        this.projects.set(snap.taxonomy.projects ?? []);
        this.repos.set(snap.taxonomy.repos ?? []);
        this.worktrees.set(snap.taxonomy.worktrees ?? []);
      }
      if (snap.usage) {
        this.usage.set(snap.usage);
      }
      if (snap.pause) {
        this.control.applyPause(snap.pause);
      }
      if (snap.activity) {
        this.activity.applySnapshot(snap.activity);
      }
      for (const op of this.liveLog.slice(logStart)) op();
    } finally {
      this.refreshesInFlight--;
      if (this.refreshesInFlight === 0) {
        this.liveLog = [];
        this.loading.set(false);
      }
    }
  }

  /**
   * Asks the hub to start a hub-managed machine's konsertd again. The machine shows
   * "starting" until the hub reports the outcome over the socket.
   */
  async retryStart(workerId: string): Promise<void> {
    this.live(() =>
      this.workers.update((list) =>
        list.map((w) => (w.id === workerId ? { ...w, launch_state: 'starting' as const, launch_error: undefined } : w)),
      ),
    );
    try {
      await this.api.startWorker(workerId);
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'Could not start konsertd';
      this.live(() =>
        this.workers.update((list) =>
          list.map((w) => (w.id === workerId ? { ...w, launch_state: 'failed' as const, launch_error: msg } : w)),
        ),
      );
    }
  }

  async loadSettings(): Promise<void> {
    try {
      const s = await this.api.getSettings();
      this.settings.set(s);
    } catch {
      // ignore
    }
  }

  upsertSpace(space: Space): void {
    this.live(() => {
      this.spaces.update((list) => {
        const idx = list.findIndex((s) => s.id === space.id);
        if (idx === -1) return [...list, space];
        const next = [...list];
        next[idx] = { ...next[idx], ...space };
        return next;
      });
    });
  }

  removeSpace(spaceId: string): void {
    this.live(() => {
      this.spaces.update((list) => list.filter((s) => s.id !== spaceId));
    });
  }

  removeWorker(workerId: string): void {
    this.live(() => {
      this.workers.update((list) => list.filter((w) => w.id !== workerId));
      this.spaces.update((list) => list.filter((s) => s.worker_id !== workerId));
      this.repos.update((list) => list.filter((r) => r.worker_id !== workerId));
    });
  }

  upsertWorkspace(ws: Workspace): void {
    this.live(() => {
      this.workspaces.update((list) => {
        const idx = list.findIndex((x) => x.id === ws.id);
        if (idx === -1) return [...list, ws];
        const next = [...list];
        next[idx] = { ...next[idx], ...ws };
        return next;
      });
    });
  }

  removeWorkspace(id: string): void {
    this.live(() => {
      this.workspaces.update((list) => list.filter((x) => x.id !== id));
      const removedProjIds = new Set(this.projects().filter((p) => p.workspace_id === id).map((p) => p.id));
      this.projects.update((list) => list.filter((p) => p.workspace_id !== id));
      const removedRepoIds = new Set(this.repos().filter((r) => removedProjIds.has(r.project_id)).map((r) => r.id));
      this.repos.update((list) => list.filter((r) => !removedProjIds.has(r.project_id)));
      this.worktrees.update((list) => list.filter((wt) => !removedRepoIds.has(wt.repo_id)));
    });
  }

  upsertProject(proj: Project): void {
    this.live(() => {
      this.projects.update((list) => {
        const idx = list.findIndex((x) => x.id === proj.id);
        if (idx === -1) return [...list, proj];
        const next = [...list];
        next[idx] = { ...next[idx], ...proj };
        return next;
      });
    });
  }

  removeProject(id: string): void {
    this.live(() => {
      this.projects.update((list) => list.filter((p) => p.id !== id));
      const removedRepoIds = new Set(this.repos().filter((r) => r.project_id === id).map((r) => r.id));
      this.repos.update((list) => list.filter((r) => r.project_id !== id));
      this.worktrees.update((list) => list.filter((wt) => !removedRepoIds.has(wt.repo_id)));
    });
  }

  upsertRepo(repo: Repo): void {
    this.live(() => {
      this.repos.update((list) => {
        const idx = list.findIndex((x) => x.id === repo.id);
        if (idx === -1) return [...list, repo];
        const next = [...list];
        next[idx] = { ...next[idx], ...repo };
        return next;
      });
    });
  }

  removeRepo(id: string): void {
    this.live(() => {
      this.repos.update((list) => list.filter((r) => r.id !== id));
      this.worktrees.update((list) => list.filter((wt) => wt.repo_id !== id));
    });
  }

  upsertWorktree(wt: Worktree): void {
    this.live(() => {
      this.worktrees.update((list) => {
        const idx = list.findIndex((x) => x.id === wt.id);
        if (idx === -1) return [...list, wt];
        const next = [...list];
        next[idx] = { ...next[idx], ...wt };
        return next;
      });
    });
  }

  removeWorktree(id: string): void {
    this.live(() => {
      this.worktrees.update((list) => list.filter((wt) => wt.id !== id));
    });
  }

  /**
   * Resolves Machine (default localhost), Agent (default -> last used -> guess),
   * and Model (default -> last used -> guess) for a new Chat session.
   */
  resolveSessionDefaults(explicitMachineId = '', explicitAgent = '', explicitModel = ''): SessionDefaults {
    const s = this.settings();
    const allWorkers = this.workers();
    const online = allWorkers.filter((w) => w.status === 'online');
    const pool = online.length > 0 ? online : allWorkers;

    let chosen: Worker | undefined;
    let machineId = '';
    let machineSource: SessionDefaults['machine_source'] = 'guess';

    if (explicitMachineId) {
      chosen = pool.find((w) => w.id === explicitMachineId) ?? allWorkers.find((w) => w.id === explicitMachineId);
      machineId = chosen?.id || explicitMachineId;
      machineSource = 'explicit';
    } else {
      const local = pool.find(
        (w) =>
          w.name.toLowerCase() === 'localhost' ||
          w.host_group.toLowerCase() === 'localhost' ||
          w.tags?.['local'] === 'true'
      );
      if (local) {
        chosen = local;
        machineId = local.id;
        machineSource = 'localhost';
      } else if (s.last_used_machine) {
        const last = pool.find((w) => w.id === s.last_used_machine);
        if (last) {
          chosen = last;
          machineId = last.id;
          machineSource = 'last_used';
        }
      }
      if (!chosen && pool.length > 0) {
        chosen = pool[0];
        machineId = pool[0].id;
        machineSource = 'guess';
      }
    }

    const availableAgents = (chosen?.agents ?? []).filter((a) => a !== 'shell');
    const hasAgent = (a: string) => !chosen || availableAgents.length === 0 || availableAgents.includes(a);

    let agent = '';
    let agentSource: SessionDefaults['agent_source'] = 'guess';
    if (explicitAgent) {
      agent = explicitAgent;
      agentSource = 'explicit';
    } else if (s.default_agent && hasAgent(s.default_agent)) {
      agent = s.default_agent;
      agentSource = 'default';
    } else if (s.last_used_agent && s.last_used_agent !== 'shell' && hasAgent(s.last_used_agent)) {
      agent = s.last_used_agent;
      agentSource = 'last_used';
    } else {
      const preferred = ['agy', 'claude', 'codex', 'grok', 'copilot', 'cursor', 'mock'];
      agent = preferred.find((p) => availableAgents.includes(p)) || availableAgents[0] || 'claude';
      agentSource = 'guess';
    }

    let model = '';
    let modelSource: SessionDefaults['model_source'] = 'guess';
    if (explicitModel) {
      model = explicitModel;
      modelSource = 'explicit';
    } else if (s.default_model) {
      model = s.default_model;
      modelSource = 'default';
    } else if (s.last_used_model) {
      model = s.last_used_model;
      modelSource = 'last_used';
    }
    // Otherwise leave it empty: the agent account's own default model applies (the
    // composer reads real models from capabilities; nothing is guessed here).

    return {
      machine_id: machineId,
      machine_source: machineSource,
      agent,
      agent_source: agentSource,
      model,
      model_source: modelSource,
      available_agents: availableAgents,
    };
  }

  upsertWorker(worker: Worker): void {
    this.live(() => {
      this.workers.update((list) => {
        const idx = list.findIndex((w) => w.id === worker.id);
        if (idx === -1) return [...list, worker];
        const next = [...list];
        next[idx] = {
          ...next[idx],
          ...worker,
          metrics: worker.metrics ?? next[idx].metrics,
          // Heat comes with heartbeats; other updates keep it, going offline clears it.
          heat: worker.status === 'online' ? (worker.heat ?? next[idx].heat) : undefined,
          facts: worker.facts ?? next[idx].facts,
          // Every worker view carries its current launch state; absent means cleared.
          launch_state: worker.launch_state,
          launch_error: worker.launch_error,
        };
        return next;
      });
    });
  }

  upsertTask(task: Task): void {
    this.live(() => {
      this.tasks.update((list) => {
        const idx = list.findIndex((t) => t.id === task.id);
        if (idx === -1) return [task, ...list];
        const next = [...list];
        next[idx] = { ...next[idx], ...task };
        return next;
      });
    });
  }

  setUsage(summary: UsageSummary): void {
    this.live(() => {
      this.usage.set(summary);
    });
  }
}

export function basenamePath(p: string): string {
  if (!p) return '';
  if (p === '~') return '~';
  const cleaned = p.replace(/[\\/]+$/, '');
  const parts = cleaned.split(/[\\/]/);
  return parts[parts.length - 1] || cleaned;
}

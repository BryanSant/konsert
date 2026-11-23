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
import { ApiClientService } from '../services/api-client.service';

export interface WorkerNodeView {
  worker: Worker;
  spaces: Space[];
  tasks: Task[];
  activeCount: number;
  effectiveStatus: 'running' | 'waiting_input' | 'idle' | 'offline';
}

export interface HostGroupView {
  hostGroup: string;
  workers: WorkerNodeView[];
  onlineCount: number;
  runningCount: number;
}

export interface FleetPortView {
  workerId: string;
  workerName: string;
  hostGroup: string;
  port: number;
  process: string;
  address: string;
}

export interface AgentRosterItem {
  task: Task;
  workerName: string;
  label: string;
  isActive: boolean;
  stateGroup: 'working' | 'idle' | 'recent';
}

@Injectable({ providedIn: 'root' })
export class FleetStore {
  private readonly api = inject(ApiClientService);

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

  readonly spacesById = computed(() => {
    const map: Record<string, Space> = {};
    for (const sp of this.spaces()) {
      map[sp.id] = sp;
    }
    return map;
  });

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

  readonly activeTasksCount = computed(() =>
    this.tasks().filter((t) => t.status === 'running' || t.status === 'waiting_input' || t.status === 'assigned').length
  );

  readonly onlineWorkersCount = computed(() =>
    this.workers().filter((w) => w.status === 'online').length
  );

  readonly groupedFleet = computed<HostGroupView[]>(() => {
    const allWorkers = this.workers();
    const allTasks = this.tasks().filter((t) => t.origin !== 'scratch');
    const allSpaces = this.spaces();
    const groupMap = new Map<string, WorkerNodeView[]>();

    for (const w of allWorkers) {
      const workerSpaces = allSpaces.filter((sp) => sp.worker_id === w.id);
      const workerTasks = allTasks
        .filter((t) => t.worker_id === w.id)
        .sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime())
        .slice(0, 12);

      const activeTasks = workerTasks.filter(
        (t) => t.status === 'running' || t.status === 'waiting_input' || t.status === 'assigned'
      );
      const hasWaiting = activeTasks.some((t) => t.status === 'waiting_input');
      let effectiveStatus: WorkerNodeView['effectiveStatus'] = 'offline';
      if (w.status === 'online') {
        if (hasWaiting) effectiveStatus = 'waiting_input';
        else if (activeTasks.length > 0) effectiveStatus = 'running';
        else effectiveStatus = 'idle';
      }

      const node: WorkerNodeView = {
        worker: w,
        spaces: workerSpaces,
        tasks: workerTasks,
        activeCount: activeTasks.length,
        effectiveStatus,
      };

      const hg = w.host_group || w.name || 'default';
      const list = groupMap.get(hg) ?? [];
      list.push(node);
      groupMap.set(hg, list);
    }

    return Array.from(groupMap.entries())
      .map(([hostGroup, nodes]) => ({
        hostGroup,
        workers: nodes.sort((a, b) => a.worker.name.localeCompare(b.worker.name)),
        onlineCount: nodes.filter((n) => n.worker.status === 'online').length,
        runningCount: nodes.reduce((acc, n) => acc + n.activeCount, 0),
      }))
      .sort((a, b) => a.hostGroup.localeCompare(b.hostGroup));
  });

  /** Herdr-style live Agents roster across all hosts (coding agents only, deduplicated & grouped Working / Idle / Recent). */
  readonly agentsRoster = computed<AgentRosterItem[]>(() => {
    const wMap = this.workersById();
    const rank = (status: string): number => {
      if (status === 'waiting_input') return 0;
      if (status === 'running' || status === 'assigned') return 1;
      if (status === 'idle') return 2;
      return 3;
    };

    const filtered = this.tasks().filter((t) => {
      if (t.origin === 'scratch' || t.agent === 'shell') return false;
      // Hide legacy orphaned external rows previously marked failed on stale worker disconnect.
      if (t.origin === 'external' && t.status === 'failed' && (t.summary || '').includes('worker went offline')) {
        return false;
      }
      return true;
    });

    // Deduplicate external sessions by (worker_id, agent, dir) so restarts/re-scans never create double sets.
    const sorted = [...filtered].sort((a, b) => {
      const ra = rank(a.status);
      const rb = rank(b.status);
      if (ra !== rb) return ra - rb;
      return new Date(b.created_at).getTime() - new Date(a.created_at).getTime();
    });

    const seenExternal = new Set<string>();
    const deduped: Task[] = [];
    for (const t of sorted) {
      if (t.origin === 'external') {
        const key = `${t.worker_id}|${t.agent}|${(t.effective_dir || t.workdir || '').replace(/[\\/]+$/, '')}`;
        if (seenExternal.has(key)) continue;
        seenExternal.add(key);
      }
      deduped.push(t);
    }

    return deduped
      .map((t) => {
        const isWorking = t.status === 'running' || t.status === 'waiting_input' || t.status === 'assigned';
        const isIdle = t.status === 'idle';
        const stateGroup: AgentRosterItem['stateGroup'] = isWorking ? 'working' : isIdle ? 'idle' : 'recent';
        const dirName = basenamePath(t.effective_dir || t.workdir);
        const label = t.branch ? `${dirName} · ${t.branch}` : dirName || t.agent;
        return {
          task: t,
          workerName: wMap[t.worker_id]?.name || t.worker_id || 'queued',
          label,
          isActive: isWorking,
          stateGroup,
        };
      })
      .slice(0, 20);
  });

  /** Aggregated listening ports across all online workers for the footer Ports popover. */
  readonly listeningPorts = computed<FleetPortView[]>(() => {
    const out: FleetPortView[] = [];
    for (const w of this.workers()) {
      if (w.status !== 'online' || !w.metrics?.listening_ports) continue;
      for (const p of w.metrics.listening_ports) {
        out.push({
          workerId: w.id,
          workerName: w.name,
          hostGroup: w.host_group,
          port: p.port,
          process: p.process || 'tcp',
          address: p.address || '*',
        });
      }
    }
    return out.sort((a, b) => a.port - b.port);
  });

  /** Aggregated fleet memory & CPU summary for the Resource Manager popover. */
  readonly resourceTotals = computed(() => {
    let memUsedMB = 0;
    let memTotalMB = 0;
    let cpuSum = 0;
    let cpuNodes = 0;
    for (const w of this.workers()) {
      if (w.status !== 'online' || !w.metrics) continue;
      memUsedMB += w.metrics.mem_used_mb || 0;
      memTotalMB += w.metrics.mem_total_mb || 0;
      cpuSum += w.metrics.cpu_percent || 0;
      cpuNodes++;
    }
    return {
      memUsedMB,
      memTotalMB,
      memPercent: memTotalMB > 0 ? Math.min(100, Math.round((memUsedMB / memTotalMB) * 100)) : 0,
      avgCpuPercent: cpuNodes > 0 ? Math.round(cpuSum / cpuNodes) : 0,
      activePtys: this.activeTasksCount(),
    };
  });

  readonly queuedTasks = computed(() =>
    this.tasks().filter((t) => t.status === 'queued')
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
      for (const op of this.liveLog.slice(logStart)) op();
    } finally {
      this.refreshesInFlight--;
      if (this.refreshesInFlight === 0) {
        this.liveLog = [];
        this.loading.set(false);
      }
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
          facts: worker.facts ?? next[idx].facts,
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

import { Injectable, computed, inject, signal } from '@angular/core';
import {
  AppSettings,
  AutopilotProjectState,
  AutopilotState,
  PolicyDirective,
  Project,
  Repo,
  SessionDefaults,
  Task,
  UsageSummary,
  Worker,
} from '../models/syre.models';
import { WorkerHeat } from '../models/activity.models';
import { MachineGroup } from '../../shared/machines/machine-target';
import { Review, reviewKey } from '../models/review.models';
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
  readonly projects = signal<Project[]>([]);
  readonly repos = signal<Repo[]>([]);
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
  /** Review decisions and Expedite flags by "task:<id>" / "chat:<id>" (hub-stored, SYR-BM8JR). */
  readonly reviews = signal<Record<string, Review>>({});
  /** Active policy directives from GET /api/fleet and fleet.update kind "directive". */
  readonly directives = signal<PolicyDirective[]>([]);
  /** The user's saved machine groups (SYR-FJA2T). */
  readonly machineGroups = signal<MachineGroup[]>([]);
  /** Operator labels by machine ID. */
  readonly labelsById = computed<Record<string, string[]>>(() => Object.fromEntries(this.workers().map((w) => [w.id, w.labels ?? []])));
  /** Every label in use, for type-ahead (first spelling wins, sorted). */
  readonly allLabels = computed(() => {
    const seen = new Map<string, string>();
    for (const w of this.workers()) for (const l of w.labels ?? []) if (!seen.has(l.toLowerCase())) seen.set(l.toLowerCase(), l);
    return [...seen.values()].sort((a, b) => a.localeCompare(b));
  });
  /** Fleet-wide and per-project Autopilot Play/Pause state and ticket counts. */
  readonly autopilot = signal<AutopilotState>({
    global: false,
    fleet_paused: false,
    state: 'paused',
    counts: { dispatchable: 0, running: 0, needs_you: 0, review: 0 },
    projects: [],
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
      if (snap.projects) {
        this.projects.set(snap.projects);
      }
      if (snap.repos) {
        this.repos.set(snap.repos);
      }
      if (snap.usage) {
        this.usage.set(snap.usage);
      }
      if (snap.pause) {
        this.control.applyPause(snap.pause);
      }
      if (snap.autopilot) {
        this.autopilot.set(snap.autopilot);
      }
      if (snap.activity) {
        this.activity.applySnapshot(snap.activity);
      }
      if (snap.reviews) {
        this.reviews.set(Object.fromEntries(snap.reviews.map((r) => [reviewKey(r), r])));
      }
      this.directives.set(snap.directives ?? []);
      this.machineGroups.set(snap.machine_groups ?? []);
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
   * Asks the hub to start a hub-managed machine's syred again. The machine shows
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
      const msg = err instanceof Error ? err.message : 'Could not start syred';
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

  /** Ends a directive (the receipt Undo). The hub drops it from the active set. */
  async endDirective(id: string): Promise<PolicyDirective> {
    const d = await this.api.endDirective(id);
    this.applyDirective(d);
    return d;
  }

  /**
   * One directive pushed by the hub (fleet.update kind "directive") or returned by
   * create/end. An ended directive, or one whose until has passed, drops out.
   * Recorded through live() so an in-flight GET /api/fleet cannot revert it.
   */
  applyDirective(d: PolicyDirective): void {
    this.live(() => {
      this.directives.update((list) => {
        const next = list.filter((x) => x.id !== d.id);
        if (d.ended_at) return next;
        if (d.until) {
          const at = Date.parse(d.until);
          if (!Number.isNaN(at) && at <= Date.now()) return next;
        }
        return [...next, d];
      });
    });
  }

  /** Autopilot snapshot pushed by the hub (fleet.update kind "autopilot"). */
  applyAutopilot(state: AutopilotState): void {
    this.live(() => {
      this.autopilot.set(state);
    });
  }

  /** Merges a single project's autopilot response into the live snapshot. */
  applyAutopilotProject(ps: AutopilotProjectState): void {
    this.live(() => {
      this.autopilot.update((cur) => {
        const idx = cur.projects.findIndex((p) => p.project_id === ps.project_id);
        const projects = idx === -1 ? [...cur.projects, ps] : cur.projects.map((p, i) => (i === idx ? ps : p));
        const anyPlaying = projects.some((p) => p.play);
        const anyFinished = projects.some((p) => p.state === 'finished');
        const state = cur.fleet_paused ? 'paused' : cur.global || anyPlaying ? 'playing' : anyFinished ? 'finished' : 'paused';
        return { ...cur, state, projects };
      });
      this.projects.update((list) =>
        list.map((p) =>
          p.id === ps.project_id
            ? { ...p, autopilot: ps.autopilot, autopilot_paused: ps.autopilot_paused }
            : p,
        ),
      );
    });
  }

  /** A review pushed by the hub (fleet.update kind "review") or returned by a call. */
  upsertReview(r: Review): void {
    this.live(() => {
      this.reviews.update((m) => {
        const k = reviewKey(r);
        const cur = m[k];
        // An older copy (a slow response after a newer push) never wins.
        if (cur && Date.parse(cur.updated_at) > Date.parse(r.updated_at)) return m;
        return { ...m, [k]: r };
      });
    });
  }

  removeWorker(workerId: string): void {
    this.live(() => {
      this.workers.update((list) => list.filter((w) => w.id !== workerId));
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
      this.repos.update((list) => list.filter((r) => r.project_id !== id));
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
          // Likewise its policy: absent means none (cleared).
          policy: worker.policy,
          policy_refusing: worker.policy_refusing,
        };
        return next;
      });
    });
  }

  /** A machine's labels changed (fleet.update kind "machine_labels"). */
  applyMachineLabels(workerId: string, labels: string[]): void {
    this.live(() => this.workers.update((list) => list.map((w) => (w.id === workerId ? { ...w, labels } : w))));
  }

  applyMachineGroups(groups: MachineGroup[]): void {
    this.live(() => this.machineGroups.set(groups));
  }

  async addLabel(workerId: string, label: string): Promise<void> {
    const res = await this.api.addMachineLabel(workerId, label);
    this.applyMachineLabels(res.worker_id, res.labels);
  }

  async removeLabel(workerId: string, label: string): Promise<void> {
    const res = await this.api.removeMachineLabel(workerId, label);
    this.applyMachineLabels(res.worker_id, res.labels);
  }

  /** Saves a group (new without id) and returns it as stored. */
  async saveGroup(group: { id?: string; name: string; worker_ids: string[] }): Promise<MachineGroup> {
    const g = await this.api.saveMachineGroup(group);
    this.live(() => this.machineGroups.update((list) => [...list.filter((x) => x.id !== g.id), g].sort((a, b) => a.name.localeCompare(b.name))));
    return g;
  }

  async deleteGroup(id: string): Promise<void> {
    await this.api.deleteMachineGroup(id);
    this.live(() => this.machineGroups.update((list) => list.filter((g) => g.id !== id)));
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

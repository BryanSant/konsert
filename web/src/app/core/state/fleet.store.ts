import { Injectable, computed, inject, signal } from '@angular/core';
import { Task, UsageSummary, Worker } from '../models/konsert.models';
import { ApiClientService } from '../services/api-client.service';

export interface WorkerNodeView {
  worker: Worker;
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
}

@Injectable({ providedIn: 'root' })
export class FleetStore {
  private readonly api = inject(ApiClientService);

  readonly workers = signal<Worker[]>([]);
  readonly tasks = signal<Task[]>([]);
  readonly usage = signal<UsageSummary>({
    total_input_tokens: 0,
    total_output_tokens: 0,
    total_requests: 0,
    by_provider: [],
  });
  readonly loading = signal(false);

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
    const groupMap = new Map<string, WorkerNodeView[]>();

    for (const w of allWorkers) {
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

  /** Herdr-style live Agents roster across all hosts (active first, then recent). */
  readonly agentsRoster = computed<AgentRosterItem[]>(() => {
    const wMap = this.workersById();
    return this.tasks()
      .filter((t) => t.origin !== 'scratch')
      .map((t) => {
        const isActive = t.status === 'running' || t.status === 'waiting_input' || t.status === 'assigned';
        const dirName = basenamePath(t.effective_dir || t.workdir);
        const label = t.branch ? `${dirName} · ${t.branch}` : dirName || t.agent;
        return {
          task: t,
          workerName: wMap[t.worker_id]?.name || t.worker_id || 'queued',
          label,
          isActive,
        };
      })
      .sort((a, b) => {
        if (a.isActive !== b.isActive) return a.isActive ? -1 : 1;
        return new Date(b.task.created_at).getTime() - new Date(a.task.created_at).getTime();
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

  async refresh(): Promise<void> {
    this.loading.set(true);
    try {
      const snap = await this.api.getFleet();
      this.workers.set(snap.workers ?? []);
      this.tasks.set(snap.tasks ?? []);
      if (snap.usage) {
        this.usage.set(snap.usage);
      }
    } finally {
      this.loading.set(false);
    }
  }

  upsertWorker(worker: Worker): void {
    this.workers.update((list) => {
      const idx = list.findIndex((w) => w.id === worker.id);
      if (idx === -1) return [...list, worker];
      const next = [...list];
      next[idx] = { ...next[idx], ...worker, metrics: worker.metrics ?? next[idx].metrics };
      return next;
    });
  }

  upsertTask(task: Task): void {
    this.tasks.update((list) => {
      const idx = list.findIndex((t) => t.id === task.id);
      if (idx === -1) return [task, ...list];
      const next = [...list];
      next[idx] = { ...next[idx], ...task };
      return next;
    });
  }

  setUsage(summary: UsageSummary): void {
    this.usage.set(summary);
  }
}

export function basenamePath(p: string): string {
  if (!p) return '';
  if (p === '~') return '~';
  const cleaned = p.replace(/[\\/]+$/, '');
  const parts = cleaned.split(/[\\/]/);
  return parts[parts.length - 1] || cleaned;
}

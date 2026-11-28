import { Worker } from '../../core/models/syre.models';

/**
 * How a machine's specs read everywhere they appear: the machine table, the target
 * picker, machine detail and fleet mode (SYR-FJA2T). Pure, from Worker + facts + metrics.
 */

/** Bytes as a short human size (1.2 MB). */
export function formatBytes(n: number | undefined | null): string {
  const v = Math.max(0, n ?? 0);
  if (v < 1024) return `${v} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let x = v / 1024;
  let i = 0;
  while (x >= 1024 && i < units.length - 1) {
    x /= 1024;
    i++;
  }
  return `${x >= 100 ? x.toFixed(0) : x.toFixed(1)} ${units[i]}`;
}

/** Megabytes as "512 MB" or "15.8 GB". */
export function formatMB(mb: number | undefined | null): string {
  const v = Math.max(0, mb ?? 0);
  if (!v) return '0 MB';
  if (v >= 1024) return `${(v / 1024).toFixed(1)} GB`;
  return `${Math.round(v)} MB`;
}

function osName(w: Worker): string {
  return (
    w.facts?.os_name ||
    (w.os === 'darwin'
      ? 'macOS'
      : w.os === 'windows'
        ? 'Windows'
        : w.os === 'linux'
          ? 'Linux'
          : w.os)
  );
}

/** "macOS 27.2 · arm64", plus WSL / container / VM. */
export function osText(w: Worker): string {
  const f = w.facts;
  const virt = f?.is_wsl ? 'WSL' : f?.is_container ? 'container' : f?.is_vm ? 'VM' : '';
  return [[osName(w), f?.os_version].filter(Boolean).join(' '), w.arch, virt]
    .filter(Boolean)
    .join(' · ');
}

/** "10 cores · Apple M4"; live CPU use is cpuLiveText. */
export function cpuText(w: Worker): string {
  const f = w.facts;
  if (!f?.cpu_cores && !f?.cpu_model) return '';
  return [f.cpu_cores ? `${f.cpu_cores} cores` : '', f.cpu_model ?? ''].filter(Boolean).join(' · ');
}

/** Live CPU use ("12%"), or '' while offline or unknown. */
export function cpuLiveText(w: Worker): string {
  const m = w.metrics;
  return w.status === 'online' && m
    ? `${Math.round(Math.min(100, Math.max(0, m.cpu_percent)))}%`
    : '';
}

/** "6.2 GB / 16.0 GB" while online, else the total. */
export function ramText(w: Worker): string {
  const m = w.metrics;
  if (w.status === 'online' && m?.mem_total_mb)
    return `${formatMB(m.mem_used_mb)} / ${formatMB(m.mem_total_mb)}`;
  return w.facts?.ram_total_mb ? formatMB(w.facts.ram_total_mb) : '';
}

/** "120 GB free / 512 GB · ssd". */
export function diskText(w: Worker): string {
  const f = w.facts;
  if (!f?.disk_total_gb) return '';
  return `${f.disk_free_gb} GB free / ${f.disk_total_gb} GB${f.storage_type ? ` · ${f.storage_type}` : ''}`;
}

/** "cpu 12% · mem 40% · gpu 3% · load 1.2" while online. */
export function loadText(w: Worker): string {
  const m = w.metrics;
  if (w.status !== 'online' || !m) return '';
  return [
    `cpu ${Math.round(Math.min(100, m.cpu_percent))}%`,
    m.mem_total_mb ? `mem ${Math.round((m.mem_used_mb / m.mem_total_mb) * 100)}%` : '',
    m.gpu_percent != null ? `gpu ${Math.round(m.gpu_percent)}%` : '',
    m.load_avg ? `load ${m.load_avg.toFixed(1)}` : '',
  ]
    .filter(Boolean)
    .join(' · ');
}

/** One line: "macOS · arm64 · 10c / 16G · WSL". */
export function machineSpec(w: Worker): string {
  const os = osName(w);
  const cores = w.facts?.cpu_cores ? `${w.facts.cpu_cores}c` : '';
  const ram = w.facts?.ram_total_mb ? `${Math.round(w.facts.ram_total_mb / 1024)}G` : '';
  const size = [cores, ram].filter(Boolean).join(' / ');
  const virt = w.facts?.is_wsl
    ? 'WSL'
    : w.facts?.is_container
      ? 'container'
      : w.facts?.is_vm
        ? 'VM'
        : '';
  return [os, w.arch, size, virt].filter(Boolean).join(' · ');
}

/** Sort keys for the machine table's columns. */
export const machineSortKey = {
  name: (w: Worker) => w.name.toLowerCase(),
  status: (w: Worker) => (w.status === 'online' ? 0 : 1),
  os: (w: Worker) => osText(w).toLowerCase(),
  cpu: (w: Worker) => w.facts?.cpu_cores ?? 0,
  ram: (w: Worker) => w.facts?.ram_total_mb ?? w.metrics?.mem_total_mb ?? 0,
  disk: (w: Worker) => w.facts?.disk_free_gb ?? 0,
  load: (w: Worker) => (w.status === 'online' ? (w.metrics?.cpu_percent ?? 0) : -1),
  tasks: (w: Worker) => w.active_tasks ?? 0,
} as const;
export type MachineSortColumn = keyof typeof machineSortKey;

/** Whether a machine matches the filter box (name, OS, arch, labels, host group). */
export function machineMatches(w: Worker, labels: readonly string[], query: string): boolean {
  const q = query.trim().toLowerCase();
  if (!q) return true;
  const hay = [w.name, w.sid, w.host_group, osText(w), cpuText(w), ...labels]
    .join(' ')
    .toLowerCase();
  return q.split(/\s+/).every((part) => hay.includes(part));
}

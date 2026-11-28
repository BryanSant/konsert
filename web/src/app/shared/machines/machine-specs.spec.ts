import { describe, expect, it } from 'vitest';
import { Worker } from '../../core/models/syre.models';
import {
  cpuLiveText,
  cpuText,
  diskText,
  formatMB,
  loadText,
  machineMatches,
  machineSpec,
  osText,
  ramText,
} from './machine-specs';

function worker(over: Partial<Worker> = {}): Worker {
  return {
    id: 'w-1',
    sid: 'm-1',
    name: 'box-1',
    host_group: 'lab',
    os: 'darwin',
    arch: 'arm64',
    isolation: 'host',
    permission_mode: 'full_access',
    agents: ['claude'],
    tags: {},
    max_concurrent: 4,
    status: 'online',
    last_seen_at: '',
    ...over,
  } as Worker;
}

const facts = {
  os_name: 'macOS',
  os_version: '27.2',
  cpu_model: 'Apple M4',
  cpu_cores: 10,
  ram_total_mb: 16384,
  disk_total_gb: 228,
  disk_free_gb: 28,
  storage_type: 'ssd',
  is_wsl: false,
  is_container: false,
  is_vm: false,
} as unknown as Worker['facts'];

const metrics = {
  cpu_percent: 12.4,
  mem_used_mb: 6349,
  mem_total_mb: 16384,
  load_avg: 1.23,
  gpu_percent: 3,
} as Worker['metrics'];

describe('machine specs', () => {
  it('formats megabytes', () => {
    expect(formatMB(0)).toBe('0 MB');
    expect(formatMB(512)).toBe('512 MB');
    expect(formatMB(16384)).toBe('16.0 GB');
  });

  it('describes OS, CPU, RAM and disk from facts and metrics', () => {
    const w = worker({ facts, metrics });
    expect(osText(w)).toBe('macOS 27.2 · arm64');
    expect(
      osText(
        worker({
          os: 'linux',
          arch: 'amd64',
          facts: { ...facts!, os_name: 'Ubuntu', os_version: '26.10', is_wsl: true },
        }),
      ),
    ).toBe('Ubuntu 26.10 · amd64 · WSL');
    expect(osText(worker({ os: 'windows', arch: 'amd64' }))).toBe('Windows · amd64');
    expect(cpuText(w)).toBe('10 cores · Apple M4');
    expect(cpuLiveText(w)).toBe('12%');
    expect(ramText(w)).toBe('6.2 GB / 16.0 GB');
    expect(diskText(w)).toBe('28 GB free / 228 GB · ssd');
    expect(loadText(w)).toBe('cpu 12% · mem 39% · gpu 3% · load 1.2');
    expect(machineSpec(w)).toBe('macOS · arm64 · 10c / 16G');
  });

  it('shows totals, not live use, while offline', () => {
    const w = worker({ facts, metrics, status: 'offline' });
    expect(cpuLiveText(w)).toBe('');
    expect(ramText(w)).toBe('16.0 GB');
    expect(loadText(w)).toBe('');
  });

  it('is empty without facts', () => {
    const w = worker();
    expect(cpuText(w)).toBe('');
    expect(diskText(w)).toBe('');
    expect(ramText(w)).toBe('');
  });

  it('filters by name, OS and label, every word', () => {
    const w = worker({ facts });
    expect(machineMatches(w, ['Gaming hardware'], '')).toBe(true);
    expect(machineMatches(w, ['Gaming hardware'], 'gaming')).toBe(true);
    expect(machineMatches(w, ['Gaming hardware'], 'box macos')).toBe(true);
    expect(machineMatches(w, ['Gaming hardware'], 'box windows')).toBe(false);
  });
});

import { TestBed } from '@angular/core/testing';
import { signal } from '@angular/core';
import { ApiClientService } from '../core/services/api-client.service';
import { FleetStore } from '../core/state/fleet.store';
import { Worker } from '../core/models/syre.models';
import { MachineResourcesComponent, formatIdle, joinSeries, niceMax } from './machine-resources.component';

function worker(): Worker {
  return {
    id: 'w1',
    name: 'box',
    facts: { cpu_cores: 8 },
    metrics: {
      cpu_percent: 50, mem_used_mb: 8000, mem_total_mb: 16384, load_avg: 2,
      load: {
        at: '2026-11-27T12:00:00Z',
        agent_cpu_percent: 200, other_cpu_percent: 100, agent_rss_mb: 4096, other_rss_mb: 2048,
        sessions: [{ task_id: 't1', cpu_percent: 180, rss_mb: 3000, peak_rss_mb: 3500, procs: 5, orphans: 1 }],
        top_other: [{ pid: 4242, name: 'nginx', cpu_percent: 12, rss_mb: 50, started_at: '2026-11-27T10:00:00Z' }],
        user_idle_s: 600, user_idle_source: 'hid',
      },
    },
  } as unknown as Worker;
}

describe('MachineResourcesComponent', () => {
  const series = [
    { subject: 'agent:w1', period: 60, points: [
      { subject: 'agent:w1', t: '2026-11-27T11:00:00Z', n: 6, cpu_avg: 100, cpu_max: 120, rss_avg_mb: 1000, rss_max_mb: 1100, procs_max: 2 },
      { subject: 'agent:w1', t: '2026-11-27T11:01:00Z', n: 6, cpu_avg: 300, cpu_max: 320, rss_avg_mb: 3000, rss_max_mb: 3100, procs_max: 4 },
    ] },
    { subject: 'other:w1', period: 60, points: [
      { subject: 'other:w1', t: '2026-11-27T11:01:00Z', n: 6, cpu_avg: 50, cpu_max: 60, rss_avg_mb: 500, rss_max_mb: 600, procs_max: 0 },
    ] },
  ];

  async function render(kill = vi.fn().mockResolvedValue({ name: 'nginx', killed: true })) {
    TestBed.configureTestingModule({
      imports: [MachineResourcesComponent],
      providers: [
        { provide: ApiClientService, useValue: { getMetrics: vi.fn().mockResolvedValue({ range: '12h', series }), killProcess: kill } },
        { provide: FleetStore, useValue: { tasksById: signal({ t1: { title: 'Build release' } }) } },
      ],
    });
    const f = TestBed.createComponent(MachineResourcesComponent);
    f.componentRef.setInput('worker', worker());
    f.detectChanges();
    await f.whenStable();
    f.detectChanges();
    return { f, el: f.nativeElement as HTMLElement, kill };
  }

  it('shows the split, sessions and the busiest other processes', async () => {
    const { el } = await render();
    const split = el.querySelector('[data-testid="machine-split"]')!.textContent!;
    expect(split).toContain('Agents 2.0 cores');
    expect(split).toContain('Other 1.0 cores');
    expect(split).toContain('Agents 4.0 GB');
    expect(el.querySelector('[data-testid="machine-idle"]')!.textContent).toContain('Idle 10 min');
    const row = el.querySelector('[data-testid="machine-sessions"] tbody tr')!.textContent!;
    expect(row).toContain('Build release');
    expect(row).toContain('180%');
    expect(row).toContain('1 left over');
    expect(el.querySelector('[data-testid="machine-top-other"]')!.textContent).toContain('nginx');
  });

  it('draws both series from history, one chart per measure', async () => {
    const { el } = await render();
    for (const i of [0, 1]) {
      expect(el.querySelectorAll(`[data-testid="load-chart-${i}"] path`).length).toBe(2);
    }
    expect(el.querySelectorAll('[data-testid="load-table"] tbody tr').length).toBe(2);
  });

  it('stops a process after a confirmation, naming it and its start time', async () => {
    const { f, el, kill } = await render();
    (el.querySelector('[data-testid="process-kill"]') as HTMLButtonElement).click();
    f.detectChanges();
    (el.querySelector('[data-testid="process-kill-confirm"]') as HTMLButtonElement).click();
    await f.whenStable();
    f.detectChanges();
    expect(kill).toHaveBeenCalledWith('w1', 4242, '2026-11-27T10:00:00Z');
    expect(el.querySelector('[data-testid="process-kill-note"]')!.textContent).toContain('nginx (pid 4242) stopped');
  });

  it('joins series by time and picks nice axis maxima', () => {
    const pts = joinSeries(series, 'w1', 'cpu_avg');
    expect(pts).toEqual([
      { t: Date.parse('2026-11-27T11:00:00Z'), agent: 100, other: 0 },
      { t: Date.parse('2026-11-27T11:01:00Z'), agent: 300, other: 50 },
    ]);
    expect(niceMax(310)).toBe(500);
    expect(niceMax(0)).toBe(1);
    expect(niceMax(2400)).toBe(2500);
    expect(formatIdle(undefined)).toContain('No one');
    expect(formatIdle(3)).toBe('In use right now');
  });
});

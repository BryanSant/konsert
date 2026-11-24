import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ChatSession, StorageResponse, Task } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { FleetStore } from '../core/state/fleet.store';
import { OrchestratorStore } from '../core/state/orchestrator.store';
import { KeepToggleComponent } from '../features/control/keep-toggle.component';
import { compactedNote } from '../features/chat/transcript/chat-transcript.component';
import { StorageSettingsComponent, formatBytes } from './storage-settings.component';

function storage(over: Partial<StorageResponse> = {}): StorageResponse {
  const days = { detail_days: 30, fleet_days: 90, fleet_summary_days: 365, usage_days: 30, usage_daily_days: 365 };
  const b = { Min: 7, Max: 365 };
  return {
    stats: {
      path: '/var/lib/konsert/konsert.db',
      file_bytes: 50 * 1024 * 1024,
      wal_bytes: 4 * 1024 * 1024,
      free_bytes: 1024 * 1024,
      page_size: 4096,
      method: 'dbstat',
      auto_vacuum: 'incremental',
      measured_at: new Date().toISOString(),
      breakdown: [
        { kind: 'transcripts', label: 'Transcripts', bytes: 1024 * 1024, rows: 10 },
        { kind: 'session_events', label: 'Session events', bytes: 40 * 1024 * 1024, rows: 1000 },
        { kind: 'other', label: 'Other', bytes: 9 * 1024 * 1024, rows: 0 },
      ],
    },
    retention: days,
    defaults: days,
    bounds: { detail_days: b, fleet_days: b, fleet_summary_days: { Min: 30, Max: 3650 }, usage_days: b, usage_daily_days: { Min: 30, Max: 3650 } },
    retention_text: ['Running work and work finished in the last 30 days keeps every detail.'],
    compaction: { running: false },
    ...over,
  };
}

describe('formatBytes', () => {
  it('formats sizes', () => {
    expect(formatBytes(512)).toBe('512 B');
    expect(formatBytes(1536)).toBe('1.5 KB');
    expect(formatBytes(54 * 1024 * 1024)).toBe('54.0 MB');
  });
});

describe('compactedNote', () => {
  it('is empty unless compacted and names both dates', () => {
    expect(compactedNote(null, null)).toBe('');
    const note = compactedNote('2026-11-24T12:00:00Z', '2026-10-25T12:00:00Z');
    expect(note).toContain('were compacted on');
    expect(note).toContain('from before');
    expect(note).toContain('conversation itself is kept');
  });
});

describe('StorageSettingsComponent', () => {
  let api: Record<string, ReturnType<typeof vi.fn>>;

  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    api = {
      getStorage: vi.fn(async () => storage()),
      compactNow: vi.fn(async () => ({ compaction: { running: true, step: 'chats', rows: 12, started_at: new Date().toISOString() } })),
      getCompaction: vi.fn(async () => {
        const done = { running: false, trigger: 'manual', rows: 2000, freed_bytes: 30 * 1024 * 1024, started_at: new Date().toISOString(), finished_at: new Date().toISOString(), duration_ms: 1500 };
        api['getStorage'].mockImplementation(async () => storage({ compaction: done }));
        return { compaction: done };
      }),
      setRetention: vi.fn(async (r) => storage({ retention: r })),
    };
    TestBed.configureTestingModule({ providers: [{ provide: ApiClientService, useValue: api }] });
  });

  afterEach(() => {
    vi.useRealTimers();
    TestBed.resetTestingModule();
  });

  async function setup() {
    const fixture = TestBed.createComponent(StorageSettingsComponent);
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();
    return { fixture, cmp: fixture.componentInstance, root: fixture.nativeElement as HTMLElement };
  }

  it('shows size, breakdown and retention in plain words', async () => {
    const { root } = await setup();
    expect(root.querySelector('[data-testid="storage-total"]')?.textContent).toContain('54.0 MB');
    const kinds = Array.from(root.querySelectorAll('[data-testid="storage-breakdown"] li')).map((li) => li.getAttribute('data-kind'));
    expect(kinds).toEqual(['transcripts', 'session_events', 'other']);
    expect(root.querySelector('[data-testid="storage-retention-text"]')?.textContent).toContain('30 days');
  });

  it('runs Compact now, polls progress and re-measures when done', async () => {
    const { fixture, cmp, root } = await setup();
    await cmp.compactNow();
    fixture.detectChanges();
    expect(root.querySelector('[data-testid="compact-status"]')?.textContent).toContain('12 rows');
    expect((root.querySelector('[data-testid="compact-now"]') as HTMLButtonElement).disabled).toBe(true);
    await vi.advanceTimersByTimeAsync(1100);
    await fixture.whenStable();
    fixture.detectChanges();
    expect(api['getCompaction']).toHaveBeenCalled();
    expect(api['getStorage']).toHaveBeenLastCalledWith(true);
    expect(root.querySelector('[data-testid="compact-status"]')?.textContent).toContain('2000 rows compacted');
  });

  it('saves edited retention periods', async () => {
    const { cmp } = await setup();
    cmp.setDraft('detail_days', '60');
    await cmp.saveRetention();
    expect(api['setRetention']).toHaveBeenCalledWith(expect.objectContaining({ detail_days: 60, fleet_days: 90 }));
  });
});

describe('KeepToggleComponent', () => {
  it('pins a chat and applies the result', async () => {
    const setKeep = vi.fn(async (_k: string, id: string, keep: boolean) => ({ chat: { id, keep } as ChatSession }));
    TestBed.configureTestingModule({ providers: [{ provide: ApiClientService, useValue: { setKeep } }] });
    const orch = TestBed.inject(OrchestratorStore);
    orch.sessions.set([{ id: 'c1', sid: 'c-1', title: 'x', provider: 'p', model: 'm', created_at: '', updated_at: '' }]);
    const fixture = TestBed.createComponent(KeepToggleComponent);
    fixture.componentRef.setInput('kind', 'chat');
    fixture.componentRef.setInput('id', 'c1');
    fixture.detectChanges();
    const btn = (fixture.nativeElement as HTMLElement).querySelector('[data-testid="toggle-keep"]') as HTMLButtonElement;
    expect(btn.getAttribute('aria-pressed')).toBe('false');
    await fixture.componentInstance.toggle();
    expect(setKeep).toHaveBeenCalledWith('chat', 'c1', true);
    expect(orch.sessions()[0].keep).toBe(true);
    TestBed.resetTestingModule();
  });

  it('pins a task through the fleet store', async () => {
    const setKeep = vi.fn(async (_k: string, id: string, keep: boolean) => ({ task: { id, keep, status: 'completed' } as Task }));
    TestBed.configureTestingModule({ providers: [{ provide: ApiClientService, useValue: { setKeep } }] });
    const fleet = TestBed.inject(FleetStore);
    fleet.upsertTask({ id: 't1', status: 'completed' } as Task);
    const fixture = TestBed.createComponent(KeepToggleComponent);
    fixture.componentRef.setInput('kind', 'task');
    fixture.componentRef.setInput('id', 't1');
    fixture.componentRef.setInput('keep', false);
    fixture.detectChanges();
    await fixture.componentInstance.toggle();
    expect(fleet.tasksById()['t1'].keep).toBe(true);
    TestBed.resetTestingModule();
  });
});

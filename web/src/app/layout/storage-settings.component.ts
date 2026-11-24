import { ChangeDetectionStrategy, Component, DestroyRef, OnInit, computed, inject, signal } from '@angular/core';
import { CompactionRun, RetentionDays, StorageResponse } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';

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

/** Bar colors of the breakdown kinds (others fall back to grey). */
const KIND_COLORS: Record<string, string> = {
  transcripts: '#7aa2f7',
  session_events: '#bb9af7',
  task_events: '#e0af68',
  fleet_timeline: '#9ece6a',
  usage: '#7dcfff',
  machine_history: '#f7768e',
  other: '#6b6f7a',
};

const STEP_LABELS: Record<string, string> = {
  tasks: 'Tasks',
  chats: 'Chats',
  fleet: 'Fleet timeline',
  usage: 'Usage',
  vacuum: 'Reclaiming space',
};

interface PeriodField {
  key: keyof RetentionDays;
  label: string;
}

/**
 * Settings → Storage (KON-CSCWP): the database file size, what each kind of history
 * uses, the retention policy in plain words and as editable periods, and Compact now
 * (a full pass in the background, with progress).
 */
@Component({
  selector: 'ks-storage-settings',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section class="rounded-[var(--ks-radius-lg)] bg-black/20 p-4 space-y-3 text-[12px]" data-testid="settings-storage" aria-labelledby="ks-storage-title">
      <div class="flex items-baseline justify-between gap-2">
        <div id="ks-storage-title" class="text-[11px] font-semibold uppercase tracking-wider text-[#8e9098]">Storage</div>
        @if (data(); as d) {
          <span class="text-[10.5px] text-[#8e9098]" [title]="d.stats.path">{{ d.stats.method === 'dbstat' ? 'Measured' : 'Estimated' }} {{ measuredAgo() }}</span>
        }
      </div>

      @if (data(); as d) {
        <div class="flex flex-wrap items-baseline gap-x-5 gap-y-1" data-testid="storage-size">
          <div>
            <span class="text-[18px] font-semibold text-[var(--ks-text)]" data-testid="storage-total">{{ fmt(d.stats.file_bytes + d.stats.wal_bytes) }}</span>
            <span class="ml-1 text-[#8e9098]">database</span>
          </div>
          <span class="text-[#a1a3ab]">file {{ fmt(d.stats.file_bytes) }} · WAL {{ fmt(d.stats.wal_bytes) }} · free inside {{ fmt(d.stats.free_bytes) }}</span>
        </div>

        <div class="flex h-2.5 w-full overflow-hidden rounded-full bg-[var(--ks-surface-2)]" role="img" [attr.aria-label]="barLabel()">
          @for (k of bar(); track k.kind) {
            <span class="h-full" [style.width.%]="k.pct" [style.background]="k.color"></span>
          }
        </div>
        <ul class="grid grid-cols-1 sm:grid-cols-2 gap-x-6 gap-y-1 m-0 p-0 list-none" data-testid="storage-breakdown">
          @for (k of d.stats.breakdown; track k.kind) {
            <li class="flex items-center gap-2" [attr.data-kind]="k.kind">
              <span class="inline-block h-2 w-2 shrink-0 rounded-full" [style.background]="color(k.kind)"></span>
              <span class="flex-1 text-[#d4d4d8]">{{ k.label }}</span>
              <span class="font-mono-ide text-[11px] text-[var(--ks-text)]">{{ fmt(k.bytes) }}</span>
            </li>
          }
        </ul>

        <div class="pt-1" data-testid="storage-retention-text">
          <div class="mb-1 text-[11px] text-[#8e9098]">What is kept</div>
          <ul class="m-0 space-y-1 pl-4 list-disc marker:text-[#5b5e66]">
            @for (line of d.retention_text; track $index) {
              <li class="text-[12px] leading-relaxed text-[#d4d4d8]">{{ line }}</li>
            }
          </ul>
          <p class="m-0 mt-1 text-[11px] text-[#8e9098]">Deleting a chat, task, project or machine deletes its history.</p>
        </div>

        <details class="rounded-[var(--ks-radius-md)] bg-black/20" data-testid="storage-retention-edit">
          <summary class="cursor-pointer list-none px-3 py-2 text-[12px] text-[#a1a3ab] hover:text-[#ececed]">Change retention periods</summary>
          <div class="px-3 pb-3 space-y-2">
            @for (f of fields; track f.key) {
              <label class="flex items-center gap-2">
                <span class="flex-1 text-[#d4d4d8]">{{ f.label }}</span>
                <input
                  type="number"
                  class="ks-field ks-field-sm w-20 font-mono-ide"
                  [attr.data-testid]="'retention-' + f.key"
                  [min]="d.bounds[f.key].Min"
                  [max]="d.bounds[f.key].Max"
                  [value]="draft()[f.key]"
                  (input)="setDraft(f.key, $any($event.target).value)"
                />
                <span class="w-24 text-[10.5px] text-[#8e9098]">days ({{ d.bounds[f.key].Min }}–{{ d.bounds[f.key].Max }})</span>
              </label>
            }
            <div class="flex items-center gap-2 pt-1">
              <button type="button" class="ks-btn ks-btn-sm ks-btn-primary" data-testid="retention-save" [disabled]="saving()" (click)="saveRetention()">Save retention</button>
              <button type="button" class="ks-btn ks-btn-sm" (click)="resetDraft(d.defaults)">Defaults</button>
              @if (savedNote()) {
                <span class="text-[11px] text-[#9ece6a]" role="status">{{ savedNote() }}</span>
              }
            </div>
          </div>
        </details>
      } @else if (!error()) {
        <p class="m-0 text-[#8e9098]">Measuring…</p>
      }

      <div class="flex flex-wrap items-center gap-2 pt-1">
        <button type="button" class="ks-btn ks-btn-sm" data-testid="compact-now" [disabled]="running()" (click)="compactNow()">
          {{ running() ? 'Compacting…' : 'Compact now' }}
        </button>
        <span class="text-[11px] text-[#a1a3ab]" aria-live="polite" data-testid="compact-status">{{ runText() }}</span>
      </div>
      @if (error()) {
        <p class="m-0 text-[11.5px] text-[#f3a5a5]" role="alert">{{ error() }}</p>
      }
    </section>
  `,
})
export class StorageSettingsComponent implements OnInit {
  private readonly api = inject(ApiClientService);
  private readonly destroyRef = inject(DestroyRef);

  readonly data = signal<StorageResponse | null>(null);
  readonly run = signal<CompactionRun | null>(null);
  readonly draft = signal<RetentionDays>({ detail_days: 30, fleet_days: 90, fleet_summary_days: 365, usage_days: 30, usage_daily_days: 365 });
  readonly error = signal('');
  readonly saving = signal(false);
  readonly savedNote = signal('');
  private poll: ReturnType<typeof setTimeout> | null = null;

  readonly fields: PeriodField[] = [
    { key: 'detail_days', label: 'Finished work keeps every detail for' },
    { key: 'fleet_days', label: 'Fleet timeline kept in full for' },
    { key: 'fleet_summary_days', label: 'Your actions and failures kept for' },
    { key: 'usage_days', label: 'Per-call token usage kept for' },
    { key: 'usage_daily_days', label: 'Daily usage totals kept for' },
  ];

  readonly running = computed(() => !!this.run()?.running);

  readonly bar = computed(() => {
    const d = this.data();
    if (!d) return [];
    const total = d.stats.breakdown.reduce((a, k) => a + k.bytes, 0) || 1;
    return d.stats.breakdown.filter((k) => k.bytes > 0).map((k) => ({ kind: k.kind, pct: (100 * k.bytes) / total, color: this.color(k.kind) }));
  });

  readonly barLabel = computed(() =>
    (this.data()?.stats.breakdown ?? []).map((k) => `${k.label} ${formatBytes(k.bytes)}`).join(', '),
  );

  readonly measuredAgo = computed(() => {
    const at = this.data()?.stats.measured_at;
    if (!at) return '';
    const s = Math.max(0, Math.round((Date.now() - new Date(at).getTime()) / 1000));
    return s < 60 ? 'just now' : `${Math.round(s / 60)} min ago`;
  });

  readonly runText = computed(() => {
    const r = this.run();
    if (!r || !r.started_at) return 'Compaction runs in the background every hour.';
    if (r.running) {
      const step = STEP_LABELS[r.step ?? ''] ?? (r.step || 'Starting');
      return `${step}: ${r.rows ?? 0} rows compacted so far${r.freed_bytes ? `, ${formatBytes(r.freed_bytes)} returned to disk` : ''}…`;
    }
    if (r.error) return `Last pass failed: ${r.error}`;
    const when = r.finished_at ? new Date(r.finished_at).toLocaleString() : '';
    const secs = ((r.duration_ms ?? 0) / 1000).toFixed(1);
    return `Last pass (${r.trigger === 'manual' ? 'Compact now' : 'scheduled'}) ${when}: ${r.rows ?? 0} rows compacted, ${formatBytes(r.freed_bytes ?? 0)} returned to disk in ${secs}s.`;
  });

  constructor() {
    this.destroyRef.onDestroy(() => this.stopPolling());
  }

  async ngOnInit(): Promise<void> {
    await this.load(false);
    if (this.running()) this.schedulePoll();
  }

  fmt(n: number): string {
    return formatBytes(n);
  }

  color(kind: string): string {
    return KIND_COLORS[kind] ?? '#6b6f7a';
  }

  setDraft(key: keyof RetentionDays, value: string): void {
    this.draft.update((d) => ({ ...d, [key]: Number(value) }));
  }

  resetDraft(defaults: RetentionDays): void {
    this.draft.set({ ...defaults });
  }

  async saveRetention(): Promise<void> {
    this.saving.set(true);
    this.error.set('');
    this.savedNote.set('');
    try {
      const res = await this.api.setRetention(this.draft());
      this.apply(res);
      this.savedNote.set('Saved');
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : 'Could not save retention');
    } finally {
      this.saving.set(false);
    }
  }

  async compactNow(): Promise<void> {
    this.error.set('');
    try {
      const res = await this.api.compactNow();
      this.run.set(res.compaction);
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : 'Could not start compaction');
    }
    this.schedulePoll();
  }

  private async load(fresh: boolean): Promise<void> {
    try {
      this.apply(await this.api.getStorage(fresh));
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : 'Could not load storage');
    }
  }

  private apply(res: StorageResponse): void {
    this.data.set(res);
    this.run.set(res.compaction);
    this.draft.set({ ...res.retention });
  }

  private schedulePoll(): void {
    this.stopPolling();
    this.poll = setTimeout(async () => {
      try {
        const res = await this.api.getCompaction();
        this.run.set(res.compaction);
        if (res.compaction.running) {
          this.schedulePoll();
        } else {
          await this.load(true);
        }
      } catch {
        this.schedulePoll();
      }
    }, 1000);
  }

  private stopPolling(): void {
    if (this.poll) clearTimeout(this.poll);
    this.poll = null;
  }
}

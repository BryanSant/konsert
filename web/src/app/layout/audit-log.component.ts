import { TooltipDirective } from '../shared/tooltip.directive';
import { Component, DestroyRef, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AuditEvent, AuditQuery } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { AuditStore } from '../core/state/audit.store';
import { FleetStore, basenamePath } from '../core/state/fleet.store';
import { TabsStore } from '../core/state/tabs.store';

/** Page size for GET /api/audit. */
export const AUDIT_PAGE_SIZE = 50;

/** Audit actions the hub records, with human labels (unknown actions show raw). */
export const AUDIT_ACTIONS: ReadonlyArray<{ id: string; label: string }> = [
  { id: 'task.take_control', label: 'Take control' },
  { id: 'task.release_control', label: 'Release control' },
  { id: 'task.steer', label: 'Steer' },
  { id: 'task.cancel', label: 'Cancel' },
  { id: 'git.commit', label: 'Git commit' },
  { id: 'worker.remove', label: 'Remove host' },
  { id: 'settings.update', label: 'Settings change' },
  { id: 'wakelock.host', label: 'Keep Awake (host)' },
  { id: 'wakelock.fleet', label: 'Keep Awake (fleet)' },
];

const ACTION_LABELS: Record<string, string> = Object.fromEntries(AUDIT_ACTIONS.map((a) => [a.id, a.label]));

/** Identities that are not people but appear as audit users. */
const BUILTIN_USERS = ['token', 'mcp', 'orchestrator'];

/**
 * Fleet-wide operator audit log (KON-WP2A5): who took control, steered, canceled,
 * committed, removed a host or changed settings, when, and on which host/task.
 * Lives in the right Inspector so task links open tabs beside it.
 */
@Component({
  selector: 'app-audit-log',
  standalone: true,
  imports: [TooltipDirective, FormsModule],
  host: { class: 'flex flex-col min-h-0 h-full' },
  template: `
    <form
      role="search"
      aria-label="Filter audit log"
      (ngSubmit)="reload()"
      class="shrink-0 grid grid-cols-2 gap-2.5 px-4 pt-1.5 pb-3 text-[11.5px]"
    >
      <div>
        <label for="audit-filter-host" class="block text-[11px] text-[var(--ks-text-muted)] mb-1">Host</label>
        <select
          id="audit-filter-host"
          name="host"
          [ngModel]="hostFilter()"
          (ngModelChange)="hostFilter.set($event); reload()"
          class="ks-field ks-field-sm w-full"
        >
          <option value="">All hosts</option>
          @for (w of hosts(); track w.id) {
            <option [value]="w.id">{{ w.name }}</option>
          }
        </select>
      </div>
      <div>
        <label for="audit-filter-action" class="block text-[11px] text-[var(--ks-text-muted)] mb-1">Action</label>
        <select
          id="audit-filter-action"
          name="action"
          [ngModel]="actionFilter()"
          (ngModelChange)="actionFilter.set($event); reload()"
          class="ks-field ks-field-sm w-full"
        >
          <option value="">All actions</option>
          @for (a of actions; track a.id) {
            <option [value]="a.id">{{ a.label }}</option>
          }
        </select>
      </div>
      <div>
        <label for="audit-filter-user" class="block text-[11px] text-[var(--ks-text-muted)] mb-1">User</label>
        <input
          id="audit-filter-user"
          name="user"
          type="text"
          list="audit-user-options"
          autocomplete="off"
          placeholder="Any user"
          [ngModel]="userFilter()"
          (ngModelChange)="userFilter.set($event)"
          (change)="reload()"
          class="ks-field ks-field-sm w-full"
        />
        <datalist id="audit-user-options">
          @for (u of knownUsers(); track u) {
            <option [value]="u"></option>
          }
        </datalist>
      </div>
      <div>
        <label for="audit-filter-task" class="block text-[11px] text-[var(--ks-text-muted)] mb-1">Task ID</label>
        <input
          id="audit-filter-task"
          name="task"
          type="text"
          autocomplete="off"
          placeholder="Any task"
          [ngModel]="taskFilter()"
          (ngModelChange)="taskFilter.set($event)"
          (change)="reload()"
          class="ks-field ks-field-sm w-full font-mono-ide"
        />
      </div>
      <div class="col-span-2 flex items-center justify-between pt-0.5">
        <span class="text-[10px] text-[#86888f]" role="status" aria-live="polite">{{ statusText() }}</span>
        @if (hasFilters()) {
          <button
            type="button"
            (click)="clearFilters()"
            class="ks-btn ks-btn-sm"
          >
            Clear filters
          </button>
        }
        <button type="submit" class="ks-sr-only">Apply filters</button>
      </div>
    </form>

    <div class="flex-1 min-h-0 overflow-y-auto">
      @if (error()) {
        <div role="alert" class="m-3 rounded-md bg-[#e05d5d]/10 px-3 py-2.5 text-[11px] text-[#f09595]">
          {{ error() }}
        </div>
      }
      @if (!loading() && events().length === 0 && !error()) {
        <div class="px-4 py-3 text-[11.5px] text-[#86888f]">
          {{ hasFilters() ? 'No audit events match these filters.' : 'No operator actions recorded yet.' }}
        </div>
      }
      @if (events().length > 0) {
        <table class="w-full table-fixed border-collapse text-[11px]">
          <caption class="ks-sr-only">Operator audit log, newest first</caption>
          <colgroup>
            <col class="w-[60px]" />
            <col class="w-[64px]" />
            <col />
            <col class="w-[90px]" />
          </colgroup>
          <thead class="sticky top-0 z-10 bg-[#16171a]">
            <tr class="text-left text-[10px] uppercase tracking-wider text-[#8a8c94]">
              <th scope="col" class="px-3 py-2 font-medium">Time</th>
              <th scope="col" class="px-1.5 py-2 font-medium">User</th>
              <th scope="col" class="px-1.5 py-2 font-medium">Action</th>
              <th scope="col" class="px-3 py-2 font-medium">Host / Task</th>
            </tr>
          </thead>
          <tbody>
            @for (ev of events(); track ev.id) {
              <tr class="align-top border-t border-white/[0.04] hover:bg-white/[0.03]" [attr.data-audit-id]="ev.id">
                <td class="px-3 py-2 font-mono-ide text-[10px] text-[#86888f]">
                  <time [attr.datetime]="ev.created_at" [ksTooltip]="fullTime(ev.created_at)">{{ shortTime(ev.created_at) }}</time>
                </td>
                <td class="px-1.5 py-2 text-[#d4d4d8] truncate" [ksTooltip]="ev.user + (ev.source ? ' via ' + ev.source : '')">
                  {{ ev.user }}
                </td>
                <td class="px-1.5 py-2">
                  <div class="font-medium text-[#d9a043] truncate">{{ actionLabel(ev.action) }}</div>
                  @if (ev.message) {
                    <div class="text-[10px] text-[#8e9098] break-words line-clamp-2" [ksTooltip]="ev.message">{{ ev.message }}</div>
                  }
                </td>
                <td class="px-3 py-2 min-w-0">
                  @if (ev.worker_id) {
                    <div class="truncate text-[#d4d4d8]" [ksTooltip]="ev.worker_id">{{ hostName(ev.worker_id) }}</div>
                  }
                  @if (ev.task_id) {
                    <button
                      type="button"
                      (click)="openTask(ev.task_id)"
                      [attr.aria-label]="'Open task ' + taskLabel(ev.task_id)"
                      [ksTooltip]="ev.task_id"
                      class="max-w-full truncate font-mono-ide text-[10px] text-[#7aa2d6] hover:text-[#a8c6ee] underline underline-offset-2 cursor-pointer focus-visible:outline focus-visible:outline-1 focus-visible:outline-[#4a668a] rounded-sm"
                    >
                      {{ taskLabel(ev.task_id) }}
                    </button>
                  }
                  @if (!ev.worker_id && !ev.task_id) {
                    <span class="text-[10px] text-[#5e6068]">fleet</span>
                  }
                </td>
              </tr>
            }
          </tbody>
        </table>
      }
      @if (nextCursor()) {
        <div class="px-4 pb-4 pt-2">
          <button
            type="button"
            (click)="loadMore()"
            [disabled]="loadingMore()"
            class="ks-btn ks-btn-sm w-full"
          >
            {{ loadingMore() ? 'Loading…' : 'Load older events' }}
          </button>
        </div>
      }
    </div>
  `,
})
export class AuditLogComponent implements OnInit {
  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);
  private readonly tabs = inject(TabsStore);
  private readonly auditStore = inject(AuditStore);

  readonly actions = AUDIT_ACTIONS;

  readonly hostFilter = signal('');
  readonly userFilter = signal('');
  readonly actionFilter = signal('');
  readonly taskFilter = signal('');

  readonly events = signal<AuditEvent[]>([]);
  readonly nextCursor = signal<number | undefined>(undefined);
  readonly loading = signal(false);
  readonly loadingMore = signal(false);
  readonly error = signal('');

  /** Increments per reload so a slow, superseded response is dropped. */
  private seq = 0;

  readonly hosts = computed(() =>
    [...this.fleet.workers()].sort((a, b) => (a.name || a.id).localeCompare(b.name || b.id)),
  );
  readonly knownUsers = computed(() => {
    const set = new Set<string>(BUILTIN_USERS);
    for (const ev of this.events()) set.add(ev.user);
    return [...set].sort();
  });
  readonly hasFilters = computed(
    () => !!(this.hostFilter() || this.userFilter().trim() || this.actionFilter() || this.taskFilter().trim()),
  );
  readonly statusText = computed(() => {
    if (this.loading()) return 'Loading audit log…';
    const n = this.events().length;
    const more = this.nextCursor() ? '+' : '';
    return `${n}${more} event${n === 1 && !more ? '' : 's'}`;
  });

  constructor() {
    const unsubscribe = this.auditStore.subscribe((ev) => this.onLive(ev));
    inject(DestroyRef).onDestroy(unsubscribe);
  }

  ngOnInit(): void {
    void this.reload();
  }

  /** The active filters as an API query. */
  query(): AuditQuery {
    return {
      worker_id: this.hostFilter() || undefined,
      user: this.userFilter().trim() || undefined,
      action: this.actionFilter() || undefined,
      task_id: this.taskFilter().trim() || undefined,
      limit: AUDIT_PAGE_SIZE,
    };
  }

  async reload(): Promise<void> {
    const seq = ++this.seq;
    this.loading.set(true);
    this.error.set('');
    try {
      const res = await this.api.listAudit(this.query());
      if (seq !== this.seq) return;
      this.events.set(res.events ?? []);
      this.nextCursor.set(res.next_cursor || undefined);
    } catch (err) {
      if (seq !== this.seq) return;
      this.events.set([]);
      this.nextCursor.set(undefined);
      this.error.set(err instanceof Error ? err.message : 'Failed to load the audit log');
    } finally {
      if (seq === this.seq) this.loading.set(false);
    }
  }

  async loadMore(): Promise<void> {
    const cursor = this.nextCursor();
    if (!cursor || this.loadingMore()) return;
    const seq = this.seq;
    this.loadingMore.set(true);
    try {
      const res = await this.api.listAudit({ ...this.query(), before_id: cursor });
      if (seq !== this.seq) return;
      this.events.update((cur) => mergeAudit(cur, res.events ?? []));
      this.nextCursor.set(res.next_cursor || undefined);
    } catch (err) {
      if (seq === this.seq) this.error.set(err instanceof Error ? err.message : 'Failed to load more events');
    } finally {
      this.loadingMore.set(false);
    }
  }

  clearFilters(): void {
    this.hostFilter.set('');
    this.userFilter.set('');
    this.actionFilter.set('');
    this.taskFilter.set('');
    void this.reload();
  }

  /** Whether a live entry belongs in the current filtered view. */
  matches(ev: AuditEvent): boolean {
    const user = this.userFilter().trim();
    const task = this.taskFilter().trim();
    return (
      (!this.hostFilter() || ev.worker_id === this.hostFilter()) &&
      (!user || ev.user === user) &&
      (!this.actionFilter() || ev.action === this.actionFilter()) &&
      (!task || ev.task_id === task)
    );
  }

  private onLive(ev: AuditEvent): void {
    if (!ev?.id || !this.matches(ev)) return;
    this.events.update((cur) => mergeAudit(cur, [ev]));
  }

  openTask(taskId: string): void {
    const task = this.fleet.tasksById()[taskId];
    if (task) {
      this.tabs.openTaskTab(task, this.fleet.workersById()[task.worker_id]?.name);
      return;
    }
    // Not in the loaded fleet snapshot (e.g. an old task): narrow the log to it instead.
    this.taskFilter.set(taskId);
    void this.reload();
  }

  hostName(workerId: string): string {
    return this.fleet.workersById()[workerId]?.name || workerId.slice(0, 12);
  }

  taskLabel(taskId: string): string {
    const t = this.fleet.tasksById()[taskId];
    const name = t ? basenamePath(t.workdir) || t.agent : '';
    return name ? `${name} · ${taskId.slice(0, 6)}` : taskId.slice(0, 8);
  }

  actionLabel(action: string): string {
    return ACTION_LABELS[action] ?? action;
  }

  shortTime(iso: string): string {
    const d = new Date(iso);
    if (isNaN(d.getTime())) return '';
    const sameDay = d.toDateString() === new Date().toDateString();
    const time = d.toLocaleTimeString([], { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' });
    return sameDay ? time : `${d.toLocaleDateString([], { month: 'short', day: 'numeric' })} ${time.slice(0, 5)}`;
  }

  fullTime(iso: string): string {
    const d = new Date(iso);
    return isNaN(d.getTime()) ? iso : d.toLocaleString();
  }
}

/** Merges audit entries by id, newest (highest id) first. */
export function mergeAudit(existing: AuditEvent[], incoming: AuditEvent[]): AuditEvent[] {
  const byId = new Map<number, AuditEvent>();
  for (const ev of existing) byId.set(ev.id, ev);
  for (const ev of incoming) byId.set(ev.id, ev);
  return [...byId.values()].sort((a, b) => b.id - a.id);
}

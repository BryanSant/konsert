import { ChangeDetectionStrategy, Component, computed, inject, input } from '@angular/core';
import { FleetEvent, actorLabel, directiveUndoId, sourceLabel } from '../../core/models/control.models';
import { ControlStore } from '../../core/state/control.store';
import { FleetStore } from '../../core/state/fleet.store';
import { CommanderStore } from '../../core/state/commander.store';
import { SessionNavService } from '../../core/services/session-nav.service';
import { TooltipDirective } from '../../shared/tooltip.directive';
import { shortWhen } from './pause-everything.component';

const KIND_LABELS: Record<string, string> = {
  action: 'Action',
  hold: 'Hold',
  request: 'Request',
  veto: 'Veto',
  notice: 'Told agent',
  status: 'Status',
  directive: 'Directive',
};

/**
 * "Your actions": what the user did (UI, CLI, Syre terminals) and how agents and Syre
 * responded (holds placed, requests resolved, vetoes remembered, agents notified).
 */
@Component({
  selector: 'sy-your-actions',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [TooltipDirective],
  host: { class: 'block' },
  template: `
    <section aria-labelledby="sy-your-actions-heading" data-testid="your-actions">
      <h3
        id="sy-your-actions-heading"
        class="text-[11px] font-semibold uppercase tracking-wider text-[var(--sy-text-muted)] mb-1.5"
      >
        Your actions
      </h3>
      @if (events().length === 0) {
        <p class="text-[11.5px] text-[var(--sy-text-muted)]">
          Nothing yet. Interventions and how agents responded show up here.
        </p>
      }
      <ol class="space-y-1">
        @for (ev of events(); track ev.id) {
          <li
            class="flex gap-2 rounded-[var(--sy-radius-sm)] px-1.5 py-1.5 hover:bg-white/[0.03] text-[11.5px]"
            [attr.data-event]="ev.id"
            [attr.data-kind]="ev.kind"
          >
            <time
              class="font-mono-ide text-[10px] text-[var(--sy-text-muted)] w-[38px] shrink-0 pt-0.5"
              [attr.datetime]="ev.created_at"
              [syTooltip]="fullTime(ev.created_at)"
            >
              {{ when(ev.created_at) }}
            </time>
            <div class="flex-1 min-w-0">
              <div
                class="break-words"
                [class.text-[var(--sy-text)]]="ev.cause.actor === 'user'"
                [class.text-[var(--sy-text-secondary)]]="ev.cause.actor !== 'user'"
              >
                {{ ev.summary || ev.action || ev.kind }}
              </div>
              <div
                class="flex flex-wrap items-center gap-x-1.5 text-[10px] text-[var(--sy-text-muted)] mt-0.5"
              >
                <span class="rounded px-1 bg-[var(--sy-raised)]">{{ kindLabel(ev) }}</span>
                <span>{{ actorText(ev) }}</span>
                @if (ev.cause.confidence && ev.cause.confidence !== 'certain') {
                  <span [syTooltip]="'Attribution confidence: ' + ev.cause.confidence"
                    >· {{ ev.cause.confidence }}</span
                  >
                }
                @if (machine(ev); as m) {
                  <span>· {{ m }}</span>
                }
                @if (taskSid(ev); as sid) {
                  <button
                    type="button"
                    class="font-mono-ide text-[#7aa2d6] hover:text-[#a8c6ee] underline underline-offset-2 cursor-pointer"
                    (click)="openTask(ev)"
                    [attr.aria-label]="'Open task ' + sid"
                  >
                    {{ sid }}
                  </button>
                }
                @if (chatSid(ev); as sid) {
                  <button
                    type="button"
                    class="font-mono-ide text-[#7aa2d6] hover:text-[#a8c6ee] underline underline-offset-2 cursor-pointer"
                    (click)="openChat(ev)"
                    [attr.aria-label]="'Open chat ' + sid"
                  >
                    {{ sid }}
                  </button>
                }
                @if (undoId(ev); as id) {
                  <button type="button" class="sy-btn sy-btn-sm !h-5 !px-1.5" data-testid="directive-undo" (click)="undo(id)">Undo</button>
                }
              </div>
            </div>
          </li>
        }
      </ol>
      @if (control.eventsCursor() && !workerId()) {
        <button
          type="button"
          class="sy-btn sy-btn-sm w-full mt-2"
          (click)="control.loadOlderEvents()"
        >
          Load older
        </button>
      }
    </section>
  `,
})
export class YourActionsComponent {
  /** Restrict to one machine; empty shows the whole fleet. */
  readonly workerId = input('');
  readonly limit = input(0);

  readonly control = inject(ControlStore);
  private readonly fleet = inject(FleetStore);
  private readonly nav = inject(SessionNavService);
  private readonly orch = inject(CommanderStore);

  readonly events = computed(() => {
    const wid = this.workerId();
    const list = wid
      ? this.control.events().filter((e) => e.worker_id === wid)
      : this.control.events();
    const n = this.limit();
    return n > 0 ? list.slice(0, n) : list;
  });

  kindLabel(ev: FleetEvent): string {
    return KIND_LABELS[ev.kind] ?? ev.kind;
  }

  undoId(ev: FleetEvent): string {
    return directiveUndoId(ev, this.fleet.directives());
  }

  async undo(id: string): Promise<void> {
    try {
      await this.fleet.endDirective(id);
    } catch {
      // The directive stays; the user can end it from the machine view.
    }
  }

  actorText(ev: FleetEvent): string {
    return [actorLabel(ev.cause), sourceLabel(ev.cause)].filter(Boolean).join(' ');
  }

  machine(ev: FleetEvent): string {
    if (!ev.worker_id || this.workerId()) return '';
    const w = this.fleet.workersById()[ev.worker_id];
    return w?.name || w?.sid || '';
  }

  taskSid(ev: FleetEvent): string {
    return ev.task_id ? this.fleet.tasksById()[ev.task_id]?.sid || '' : '';
  }

  chatSid(ev: FleetEvent): string {
    return ev.chat_id ? this.orch.sessions().find((s) => s.id === ev.chat_id)?.sid || '' : '';
  }

  openTask(ev: FleetEvent): void {
    const t = ev.task_id ? this.fleet.tasksById()[ev.task_id] : undefined;
    if (t) this.nav.openTask(t);
  }

  openChat(ev: FleetEvent): void {
    if (!ev.chat_id) return;
    this.nav.openChat(ev.chat_id);
  }

  when(iso: string): string {
    return shortWhen(iso);
  }

  fullTime(iso: string): string {
    const d = new Date(iso);
    return isNaN(d.getTime()) ? iso : d.toLocaleString();
  }
}

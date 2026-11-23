import { ChangeDetectionStrategy, Component, computed, inject, input } from '@angular/core';
import {
  Hold,
  UserRequest,
  actorLabel,
  resourceLabel,
  sourceLabel,
} from '../../core/models/control.models';
import { ControlStore } from '../../core/state/control.store';
import { FleetStore } from '../../core/state/fleet.store';
import { IconComponent } from '../../shared/icon.component';
import { TooltipDirective } from '../../shared/tooltip.directive';
import { shortWhen } from './pause-everything.component';

/**
 * Active holds and open agent requests, fleet-wide or for one machine. Holds block
 * automatic actions on their scope until released; requests are things an agent asked
 * the user to do (mark done or decline).
 */
@Component({
  selector: 'ks-holds-requests',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective],
  host: { class: 'block space-y-3' },
  template: `
    <section aria-labelledby="ks-holds-heading" data-testid="holds-list">
      <h3
        id="ks-holds-heading"
        class="flex items-center gap-1.5 text-[11px] font-semibold uppercase tracking-wider text-[var(--ks-text-muted)] mb-1.5"
      >
        <app-icon name="lock" [size]="11" />
        Holds
        <span class="font-mono-ide normal-case tracking-normal">{{ holds().length }}</span>
      </h3>
      @if (holds().length === 0) {
        <p class="text-[11.5px] text-[var(--ks-text-muted)]">
          No holds. Automatic retries and restarts run normally.
        </p>
      }
      <ul class="space-y-1.5">
        @for (h of holds(); track h.id) {
          <li
            class="rounded-[var(--ks-radius-md)] bg-[var(--ks-raised)] px-2.5 py-2 text-[11.5px]"
            [attr.data-hold]="h.sid"
          >
            <div class="flex items-start gap-2">
              <div class="flex-1 min-w-0">
                <div class="text-[var(--ks-text)] break-words">{{ h.summary || scopeText(h) }}</div>
                <div class="text-[10.5px] text-[var(--ks-text-muted)] mt-0.5">
                  {{ scopeText(h) }} · {{ actor(h) }} ·
                  <span class="font-mono-ide">{{ h.sid }}</span> · {{ when(h.created_at) }}
                </div>
              </div>
              <button
                type="button"
                class="ks-btn ks-btn-sm shrink-0"
                [attr.data-testid]="'hold-release-' + h.sid"
                [disabled]="control.isBusy(h.id)"
                (click)="control.releaseHold(h)"
                ksTooltip="Allow automatic actions here again"
              >
                Release
              </button>
            </div>
          </li>
        }
      </ul>
    </section>

    <section aria-labelledby="ks-requests-heading" data-testid="requests-list">
      <h3
        id="ks-requests-heading"
        class="flex items-center gap-1.5 text-[11px] font-semibold uppercase tracking-wider text-[var(--ks-text-muted)] mb-1.5"
      >
        <app-icon name="hand" [size]="11" />
        Open requests
        <span class="font-mono-ide normal-case tracking-normal">{{ requests().length }}</span>
      </h3>
      @if (requests().length === 0) {
        <p class="text-[11.5px] text-[var(--ks-text-muted)]">
          No agent is waiting for you to do something.
        </p>
      }
      <ul class="space-y-1.5">
        @for (r of requests(); track r.id) {
          <li
            class="rounded-[var(--ks-radius-md)] bg-[var(--ks-amber-soft)] px-2.5 py-2 text-[11.5px]"
            [attr.data-request]="r.sid"
          >
            <div class="text-[var(--ks-text)] break-words">{{ r.description }}</div>
            <div class="text-[10.5px] text-[var(--ks-text-muted)] mt-0.5">
              {{ machineName(r.worker_id) }}
              @if (resource(r); as res) {
                · {{ res }}
              }
              · <span class="font-mono-ide">{{ r.sid }}</span> · {{ when(r.created_at) }}
            </div>
            <div class="flex justify-end gap-1.5 mt-1.5">
              <button
                type="button"
                class="ks-btn ks-btn-sm"
                [attr.data-testid]="'request-decline-' + r.sid"
                [disabled]="control.isBusy(r.id)"
                (click)="control.resolveRequest(r, 'declined')"
              >
                Decline
              </button>
              <button
                type="button"
                class="ks-btn ks-btn-sm ks-btn-soft"
                [attr.data-testid]="'request-done-' + r.sid"
                [disabled]="control.isBusy(r.id)"
                (click)="control.resolveRequest(r, 'done')"
                ksTooltip="Tell the agent it is done so it continues"
              >
                <app-icon name="check" [size]="11" />
                Mark done
              </button>
            </div>
          </li>
        }
      </ul>
    </section>
  `,
})
export class HoldsRequestsComponent {
  /** Restrict to one machine; empty shows the whole fleet. */
  readonly workerId = input('');

  readonly control = inject(ControlStore);
  private readonly fleet = inject(FleetStore);

  readonly holds = computed(() => {
    const wid = this.workerId();
    return wid ? this.control.holds().filter((h) => h.worker_id === wid) : this.control.holds();
  });
  readonly requests = computed(() => {
    const wid = this.workerId();
    return wid
      ? this.control.requests().filter((r) => r.worker_id === wid)
      : this.control.requests();
  });

  machineName(id: string | undefined): string {
    if (!id) return 'fleet';
    const w = this.fleet.workersById()[id];
    return w?.name || w?.sid || id;
  }

  scopeText(h: Hold): string {
    const host = this.machineName(h.worker_id);
    switch (h.scope_kind) {
      case 'resource':
        return `${resourceLabel(h.resource) || 'resource'} on ${host}`;
      case 'machine':
        return `machine ${host}`;
      default: {
        const t = h.task_id ? this.fleet.tasksById()[h.task_id] : undefined;
        return `session ${t?.sid || h.task_id || h.chat_id || ''} on ${host}`.trim();
      }
    }
  }

  actor(h: Hold): string {
    return [actorLabel(h.cause), sourceLabel(h.cause)].filter(Boolean).join(' ');
  }

  resource(r: UserRequest): string {
    return resourceLabel(r.resource);
  }

  when(iso: string): string {
    return shortWhen(iso);
  }
}

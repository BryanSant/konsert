import { ChangeDetectionStrategy, Component, computed, inject, input, signal } from '@angular/core';
import { PlacementLogEntry } from '../../core/models/syre.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { FleetStore } from '../../core/state/fleet.store';
import { TooltipDirective } from '../../shared/tooltip.directive';

/** "Why here?": where a task was placed, who decided, and how the machines compared (SYR-QY2TG). */
@Component({
  selector: 'sy-placement-why',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [TooltipDirective],
  host: { class: 'contents' },
  template: `
    <button type="button" class="sy-btn sy-btn-sm" data-testid="placement-why" [attr.aria-expanded]="open()" (click)="toggle()" syTooltip="Why this machine">
      Why here?
    </button>
    @if (open()) {
      <div class="mt-1 w-full rounded-[var(--sy-radius-md)] border border-[var(--sy-hairline)] bg-[var(--sy-surface-1)] px-3 py-2 text-[12px] space-y-1" data-testid="placement-explanation" role="region" aria-label="Placement">
        @if (error()) {
          <p class="m-0 text-[#f09595]" role="alert">{{ error() }}</p>
        } @else if (!entry()) {
          <p class="m-0 text-[var(--sy-text-muted)]">{{ loading() ? 'Loading…' : 'No placement recorded for this session.' }}</p>
        } @else {
          @let e = entry()!;
          <p class="m-0" data-testid="placement-summary">{{ summary() }}</p>
          <ul class="m-0 p-0 list-none space-y-0.5">
            @for (r of e.detail.ranked; track r.ID) {
              <li class="font-mono-ide text-[11px]" [class.text-[var(--sy-text)]]="r.ID === e.worker_id" [class.text-[var(--sy-text-muted)]]="r.ID !== e.worker_id" data-testid="placement-candidate">
                {{ name(r.ID) }}: {{ r.fits ? 'fits' : "doesn't fit" }} · fit {{ r.fit.toFixed(2) }} · locality {{ r.locality.toFixed(2) }} · free slots {{ r.spread.toFixed(2) }}{{ r.rule ? ' · rule ' + (r.rule > 0 ? '+' : '') + r.rule : '' }}{{ r.weight < 1 ? ' · cooling down after a shed' : '' }}
              </li>
            }
            @for (r of e.detail.rejected ?? []; track r.worker_id) {
              <li class="font-mono-ide text-[11px] text-[var(--sy-text-muted)]" data-testid="placement-rejected">{{ r.worker_name }}: ruled out ({{ r.reason }})</li>
            }
          </ul>
        }
      </div>
    }
  `,
})
export class PlacementWhyComponent {
  readonly taskId = input.required<string>();
  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);
  readonly open = signal(false);
  readonly loading = signal(false);
  readonly error = signal('');
  readonly entry = signal<PlacementLogEntry | null>(null);

  readonly summary = computed(() => {
    const e = this.entry();
    if (!e) return '';
    const who = e.decider === 'user' ? 'you' : e.decider === 'commander' ? 'the Commander' : 'the scheduler';
    const need = e.detail.known && e.detail.peak_mb ? `expected to peak at ${Math.round(e.detail.peak_mb / 1024)} GB` : 'no profile yet: rotated by headroom';
    const where = e.worker_id ? `Placed on ${this.name(e.worker_id)} by ${who}` : `Queued by ${who}: no machine could take it`;
    const rules = e.detail.rules?.length ? ` · ${e.detail.rules.length} placement rule${e.detail.rules.length > 1 ? 's' : ''}` : '';
    return `${where} (${need})${rules}.`;
  });

  name(id: string): string {
    return this.fleet.workers().find((w) => w.id === id)?.name ?? id;
  }

  async toggle(): Promise<void> {
    this.open.update((v) => !v);
    if (!this.open()) return;
    this.loading.set(true);
    this.error.set('');
    try {
      const { entries } = await this.api.getPlacementLog(`task:${this.taskId()}`, 1);
      this.entry.set(entries[0] ?? null);
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : String(e));
    } finally {
      this.loading.set(false);
    }
  }
}

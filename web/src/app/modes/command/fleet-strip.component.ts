import { ChangeDetectionStrategy, Component, computed, inject } from '@angular/core';
import { RouterLink } from '@angular/router';
import { FleetStore } from '../../core/state/fleet.store';
import { machineLoadLabel, occupiesSlot } from '../../core/state/work-logic';
import { providerDirectiveLines } from '../fleet/fleet-logic';
import { FooterUsageComponent } from '../../layout/footer-usage.component';
import { HeatAvatarComponent } from '../../shared/activity-spinner.component';
import { heatTextColor, spinnerMachine } from '../../shared/spinner-logic';

/**
 * Command's bottom strip: each machine's heat and load (its heat avatar, slots, heat %),
 * and each subscription's usage (the usage popover has the windows and sign-in).
 */
@Component({
  selector: 'sy-fleet-strip',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RouterLink, HeatAvatarComponent, FooterUsageComponent],
  host: { class: 'hidden md:block shrink-0' },
  template: `
    <footer class="flex flex-wrap items-center gap-x-4 gap-y-1 px-4 py-1 text-[12px] text-[var(--sy-text-secondary)] bg-[var(--sy-bg-chrome)] border-t border-[var(--sy-hairline)]" aria-label="Fleet and quota" data-testid="fleet-strip">
      @if (machines().length === 0) {
        <span class="text-[var(--sy-text-muted)]">No machines yet</span>
      }
      @for (m of machines(); track m.id) {
        <a
          [routerLink]="['/fleet', m.sid]"
          class="inline-flex items-center gap-1.5 no-underline hover:underline"
          [class.text-[var(--sy-amber)]]="m.tone === 'warn'"
          [class.text-[var(--sy-text-muted)]]="m.tone === 'stale'"
          [class.text-[var(--sy-text-secondary)]]="m.tone === 'muted'"
          ><span aria-hidden="true" class="inline-flex"><sy-heat-avatar [machine]="m.machine" [size]="14" /></span>{{ m.text }}@if (m.heatPct !== null) {<span class="font-mono-ide text-[11px]" [style.color]="m.heatColor">{{ m.heatPct }}%<span class="sy-sr-only"> heat</span></span>}</a
        >
      }
      @for (line of providerLines(); track line) {
        <span class="text-[#e9bd76]" data-testid="fleet-provider-line">{{ line }}</span>
      }
      <span class="flex-1"></span>
      <sy-footer-usage />
    </footer>
  `,
})
export class FleetStripComponent {
  private readonly fleet = inject(FleetStore);
  readonly providerLines = computed(() => providerDirectiveLines(this.fleet.directives()));

  readonly machines = computed(() => {
    const running = new Map<string, number>();
    for (const t of this.fleet.tasks()) {
      if (occupiesSlot(t)) running.set(t.worker_id, (running.get(t.worker_id) ?? 0) + 1);
    }
    const heat = this.fleet.heatByWorker();
    return this.fleet.workers().map((w) => {
      const h = heat[w.id];
      return {
        id: w.id,
        sid: w.sid,
        ...machineLoadLabel(w, running.get(w.id) ?? 0),
        machine: spinnerMachine(w.name, h),
        heatPct: h ? Math.round(h.score * 100) : null,
        heatColor: heatTextColor(h ? h.score : null),
      };
    });
  });
}

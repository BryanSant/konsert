import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { Worker } from '../../core/models/syre.models';
import { LabelChipsComponent } from './label-chips.component';
import { machineSpec } from './machine-specs';

/**
 * Status dot, name, labels and a one-line spec: a machine in the machine table's name cell
 * and in the target picker's popover (SYR-FJA2T).
 */
@Component({
  selector: 'sy-machine-summary',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [LabelChipsComponent],
  host: { class: 'flex items-start gap-2 min-w-0' },
  template: `
    <span
      class="mt-[5px] h-1.5 w-1.5 rounded-full shrink-0"
      [class.bg-[var(--sy-green)]]="online()"
      [class.bg-[#5e6068]]="!online()"
      role="img"
      [attr.aria-label]="online() ? 'Online' : 'Offline'"
    ></span>
    <span class="flex-1 min-w-0 flex flex-col gap-0.5">
      <span class="flex items-center gap-1.5 min-w-0 flex-wrap">
        <span
          class="sy-menu-label truncate text-[var(--sy-text)]"
          data-testid="machine-summary-name"
          >{{ worker().name }}</span
        >
        @if (showLabels() && labels().length) {
          <sy-label-chips [labels]="labels()" />
        }
      </span>
      <span class="sy-menu-desc block truncate">{{ line() }}</span>
    </span>
  `,
})
export class MachineSummaryComponent {
  readonly worker = input.required<Worker>();
  readonly showLabels = input(true);

  readonly online = computed(() => this.worker().status === 'online');
  readonly labels = computed(() => this.worker().labels ?? []);
  readonly line = computed(() => {
    const spec = machineSpec(this.worker());
    return this.online() ? spec : ['Offline', spec].filter(Boolean).join(' · ');
  });
}

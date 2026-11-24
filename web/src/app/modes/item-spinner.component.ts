import { ChangeDetectionStrategy, Component, computed, inject, input } from '@angular/core';
import { WorkItem } from '../core/state/work-logic';
import { WorkStore } from '../core/state/work.store';
import { ActivitySpinnerComponent } from '../shared/activity-spinner.component';
import { SPINNER_LABEL_COLORS } from '../shared/spinner-logic';

/**
 * A work item's live-work spinner with its activity word next to it (never colour alone):
 * Work cards, Wall tile headers. `wordless` drops the word where the line already says it.
 */
@Component({
  selector: 'ks-item-spinner',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [ActivitySpinnerComponent],
  host: { class: 'inline-flex items-center gap-1.5 min-w-0' },
  template: `
    @if (sp(); as s) {
      <ks-spinner [state]="s.state" [machines]="s.machines" [coarse]="s.coarse" [tps]="s.tps" [size]="size()" [detail]="s.detail ?? null" />
      @if (!wordless()) {
        <span class="truncate text-[11.5px]" [style.color]="color()" data-testid="activity-word">{{ s.word }}</span>
      }
    }
  `,
})
export class ItemSpinnerComponent {
  readonly item = input.required<Pick<WorkItem, 'id'>>();
  readonly size = input(22);
  readonly wordless = input(false);
  private readonly work = inject(WorkStore);
  readonly sp = computed(() => this.work.spinnerById()[this.item().id]);
  readonly color = computed(() => SPINNER_LABEL_COLORS[this.sp()?.state ?? 'done']);
}

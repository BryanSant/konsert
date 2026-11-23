import { ChangeDetectionStrategy, Component, computed, inject } from '@angular/core';
import { QuestionsStore } from '../../core/state/questions.store';
import { IconComponent } from '../../shared/icon.component';
import { TooltipDirective } from '../../shared/tooltip.directive';

/** Top-bar "Questions N": opens the fleet-wide question queue (amber while any wait). */
@Component({
  selector: 'ks-questions-button',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective],
  template: `
    <button
      type="button"
      class="ks-chip"
      [class.ks-chip-amber]="count() > 0"
      data-testid="questions-button"
      [attr.aria-label]="label()"
      ksTooltip="Questions from agents across the fleet"
      (click)="store.open()"
    >
      <app-icon name="question" [size]="14" />
      <span class="hidden sm:inline">Questions</span>
      @if (count() > 0) {
        <span class="font-mono-ide font-semibold" data-testid="questions-button-count">{{ count() }}</span>
      }
    </button>
  `,
})
export class QuestionsButtonComponent {
  readonly store = inject(QuestionsStore);
  readonly count = computed(() => this.store.pendingCount());
  readonly label = computed(() => (this.count() ? `Questions: ${this.count()} open` : 'Questions'));
}

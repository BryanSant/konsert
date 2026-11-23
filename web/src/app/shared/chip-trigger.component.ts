import { ChangeDetectionStrategy, Component, input } from '@angular/core';

/**
 * `<button ksChip>`: a flat pill trigger with a trailing chevron that lifts on hover.
 * Tone `amber` / `red` colors it (full access, logged out). Use with `ksPopoverTrigger`.
 */
@Component({
  selector: 'button[ksChip]',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: {
    type: 'button',
    class: 'ks-chip',
    '[class.ks-chip-amber]': "tone() === 'amber'",
    '[class.ks-chip-red]': "tone() === 'red'",
  },
  template: `
    <ng-content />
    @if (chevron()) {
      <svg aria-hidden="true" width="10" height="10" viewBox="0 0 10 10" fill="none" stroke="currentColor" stroke-width="1.5" class="shrink-0 opacity-70">
        <path d="M2.5 4 5 6.5 7.5 4" stroke-linecap="round" stroke-linejoin="round" />
      </svg>
    }
  `,
})
export class ChipTriggerComponent {
  readonly tone = input<'default' | 'amber' | 'red'>('default');
  readonly chevron = input(true);
}

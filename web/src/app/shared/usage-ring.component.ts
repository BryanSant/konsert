import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';

/** A tiny circular usage gauge: blue, amber near the limit, red when exhausted. Decorative. */
@Component({
  selector: 'ks-usage-ring',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { 'aria-hidden': 'true', class: 'inline-flex shrink-0' },
  template: `
    <svg [attr.width]="size()" [attr.height]="size()" viewBox="0 0 20 20">
      <circle cx="10" cy="10" r="7.5" fill="none" stroke="rgba(255,255,255,0.12)" stroke-width="2.5" />
      <circle
        cx="10"
        cy="10"
        r="7.5"
        fill="none"
        [attr.stroke]="color()"
        stroke-width="2.5"
        stroke-linecap="round"
        [attr.stroke-dasharray]="dash()"
        transform="rotate(-90 10 10)"
      />
    </svg>
  `,
})
export class UsageRingComponent {
  readonly percent = input<number>(0);
  readonly tone = input<'ok' | 'warning' | 'exhausted' | string>('ok');
  readonly size = input(14);

  private static readonly C = 2 * Math.PI * 7.5;
  readonly dash = computed(() => {
    const p = Math.max(0, Math.min(100, this.percent())) / 100;
    return `${(p * UsageRingComponent.C).toFixed(2)} ${UsageRingComponent.C.toFixed(2)}`;
  });
  readonly color = computed(() => {
    switch (this.tone()) {
      case 'exhausted':
        return 'var(--ks-red)';
      case 'warning':
        return 'var(--ks-amber)';
      default:
        return 'var(--ks-blue)';
    }
  });
}

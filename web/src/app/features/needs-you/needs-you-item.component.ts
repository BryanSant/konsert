import { ChangeDetectionStrategy, Component, inject, input } from '@angular/core';
import { NeedsYouEntry, NeedsYouStore } from '../../core/state/needs-you.store';
import { IconComponent } from '../../shared/icon.component';

/**
 * One needs-you item, identical wherever needs-you items are listed (Command's rail, the
 * phone inbox, Fleet, Work): priority tag, what is asked, who and how long. Activating it
 * opens the Needs-you dialog on it.
 */
@Component({
  selector: 'sy-needs-you-item',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent],
  host: { class: 'block' },
  template: `
    @let e = entry();
    <button
      type="button"
      class="sy-ny-item"
      [class.sy-ny-dim]="!e.pending"
      [attr.data-needs-you]="e.key"
      [attr.data-kind]="e.kind"
      [attr.data-impact]="e.impact ?? null"
      [attr.data-question]="e.kind === 'question' ? e.sid : null"
      [attr.data-request]="e.kind === 'request' ? e.sid : null"
      [attr.data-override]="e.kind === 'override' ? e.sid : null"
      [attr.data-policy]="e.kind === 'policy' ? e.sid : null"
      [attr.data-veto]="e.kind === 'veto' ? e.sid : null"
      [attr.data-shells]="e.kind === 'shells' ? e.sid : null"
      [attr.aria-label]="label()"
      (click)="store.open(e.key)"
    >
      <span class="sy-ny-tag" [attr.data-tag]="e.tag" aria-hidden="true">{{ e.tag }}</span>
      <span class="flex-1 min-w-0">
        <span class="block line-clamp-2 text-[var(--sy-text)]">{{ e.title }}</span>
        <span class="block truncate text-[11.5px] text-[var(--sy-text-muted)] mt-0.5">{{ e.meta }}</span>
      </span>
      <span class="shrink-0 text-[var(--sy-text-muted)]" aria-hidden="true"><app-icon name="chevron-right" [size]="13" /></span>
    </button>
  `,
  styles: `
    .sy-ny-item {
      display: flex;
      align-items: flex-start;
      gap: 10px;
      width: 100%;
      min-height: 44px;
      padding: 9px 10px;
      border: 0;
      border-radius: var(--sy-radius-md);
      background: var(--sy-surface-2);
      color: inherit;
      font: inherit;
      font-size: 12.5px;
      line-height: 1.4;
      text-align: left;
      cursor: pointer;
      transition: background-color var(--sy-dur-fast) var(--sy-ease-out);
    }
    .sy-ny-item:hover {
      background: var(--sy-surface-3);
    }
    .sy-ny-item:focus-visible {
      outline: none;
      box-shadow: var(--sy-focus-ring);
    }
    .sy-ny-dim {
      opacity: 0.6;
    }
    .sy-ny-tag {
      flex-shrink: 0;
      margin-top: 1px;
      padding: 1px 6px;
      border-radius: 4px;
      font-family: 'JetBrains Mono', ui-monospace, monospace;
      font-size: 10px;
      font-weight: 500;
      background: rgba(224, 168, 76, 0.18);
      color: #e9c27f;
    }
    .sy-ny-tag[data-tag='P1'] {
      background: var(--sy-red);
      color: #1e0b0b;
    }
    .sy-ny-tag[data-tag='P3'] {
      background: var(--sy-raised-strong);
      color: var(--sy-text-secondary);
    }
    @media (prefers-reduced-motion: reduce) {
      .sy-ny-item {
        transition: none;
      }
    }
  `,
})
export class NeedsYouItemComponent {
  readonly entry = input.required<NeedsYouEntry>();
  readonly store = inject(NeedsYouStore);

  label(): string {
    const e = this.entry();
    const veto = e.tag === 'P1' ? 'Veto not acknowledged' : 'Veto awaiting acknowledgement';
    const kind = { question: 'Question', request: 'Request', veto, review: 'Ready for review', override: 'Override request', policy: 'Policy change', shells: 'Shells left running' }[e.kind];
    return `${kind}: ${e.title}. ${e.meta}`;
  }
}

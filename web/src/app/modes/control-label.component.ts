import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { ControlLabel } from '../core/state/work-logic';

const DESCRIPTIONS: Record<ControlLabel, string> = {
  AUTO: 'Working on its own within policy',
  ORCH: 'Directed by the commander',
  ASKING: 'Waiting on you',
  HELD: 'Held by you: nothing automatic happens to it',
  SUSPENDED: 'Suspended: its processes are frozen',
  YOU: 'You have control',
  STALE: 'No data from its machine: last known state',
  QUEUED: 'Queued: waiting for a free slot',
  DONE: 'Finished',
  FAILED: 'Failed',
  STOPPED: 'Stopped (canceled or vetoed)',
};

/**
 * The control annunciator (ui-paradigms §4.9): grey when the agent works on its own,
 * colour only where it means something (asking, held, suspended, you, stale). The text
 * always carries the meaning, never the colour alone.
 */
@Component({
  selector: 'sy-control-label',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'inline-flex shrink-0' },
  template: `
    <span class="sy-annunciator" [attr.data-control]="label()" [class]="'sy-annunciator sy-ann-' + label().toLowerCase()" [class.w-[64px]]="fixed()">
      {{ label() }}<span class="sy-sr-only">: {{ description() }}</span>
    </span>
  `,
  styles: `
    .sy-annunciator {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      height: 18px;
      padding: 0 6px;
      border-radius: 4px;
      font-family: 'JetBrainsMono Nerd Font Mono', 'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, monospace;
      font-size: 9.5px;
      font-weight: 500;
      letter-spacing: 0.02em;
      color: var(--sy-text-secondary);
      background: var(--sy-raised);
      white-space: nowrap;
    }
    .sy-ann-orch {
      color: #c9cbd1;
      background: var(--sy-raised-strong);
    }
    .sy-ann-asking {
      color: #1b1406;
      background: var(--sy-amber);
    }
    .sy-ann-held {
      color: #9ec5ff;
      background: transparent;
      box-shadow: inset 0 0 0 1px var(--sy-blue);
    }
    .sy-ann-suspended {
      color: #16122e;
      background: var(--sy-violet);
    }
    .sy-ann-you {
      color: #ffffff;
      background: #2f6fd6;
    }
    .sy-ann-stale {
      color: var(--sy-text-muted);
      background: repeating-linear-gradient(135deg, rgba(255, 255, 255, 0.09) 0 4px, transparent 4px 8px);
    }
    .sy-ann-queued {
      color: var(--sy-text-muted);
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.12);
    }
    .sy-ann-done {
      color: #a8dbb3;
      background: var(--sy-green-soft);
    }
    .sy-ann-stopped {
      color: var(--sy-text-muted);
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.12);
    }
    .sy-ann-failed {
      color: #f3a3a3;
      background: var(--sy-red-soft);
    }
  `,
})
export class ControlLabelComponent {
  readonly label = input.required<ControlLabel>();
  /** Fixed width so a column of labels lines up (lists, live work). */
  readonly fixed = input(false);
  readonly description = computed(() => DESCRIPTIONS[this.label()]);
}

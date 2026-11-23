import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { ControlLabel } from '../core/state/work-logic';

const DESCRIPTIONS: Record<ControlLabel, string> = {
  AUTO: 'Working on its own within policy',
  ORCH: 'Directed by the orchestrator',
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
  selector: 'ks-control-label',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'inline-flex shrink-0' },
  template: `
    <span class="ks-annunciator" [attr.data-control]="label()" [class]="'ks-annunciator ks-ann-' + label().toLowerCase()" [class.w-[64px]]="fixed()">
      {{ label() }}<span class="ks-sr-only">: {{ description() }}</span>
    </span>
  `,
  styles: `
    .ks-annunciator {
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
      color: var(--ks-text-secondary);
      background: var(--ks-raised);
      white-space: nowrap;
    }
    .ks-ann-orch {
      color: #c9cbd1;
      background: var(--ks-raised-strong);
    }
    .ks-ann-asking {
      color: #1b1406;
      background: var(--ks-amber);
    }
    .ks-ann-held {
      color: #9ec5ff;
      background: transparent;
      box-shadow: inset 0 0 0 1px var(--ks-blue);
    }
    .ks-ann-suspended {
      color: #16122e;
      background: var(--ks-violet);
    }
    .ks-ann-you {
      color: #ffffff;
      background: #2f6fd6;
    }
    .ks-ann-stale {
      color: var(--ks-text-muted);
      background: repeating-linear-gradient(135deg, rgba(255, 255, 255, 0.09) 0 4px, transparent 4px 8px);
    }
    .ks-ann-queued {
      color: var(--ks-text-muted);
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.12);
    }
    .ks-ann-done {
      color: #a8dbb3;
      background: var(--ks-green-soft);
    }
    .ks-ann-stopped {
      color: var(--ks-text-muted);
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.12);
    }
    .ks-ann-failed {
      color: #f3a3a3;
      background: var(--ks-red-soft);
    }
  `,
})
export class ControlLabelComponent {
  readonly label = input.required<ControlLabel>();
  /** Fixed width so a column of labels lines up (lists, live work). */
  readonly fixed = input(false);
  readonly description = computed(() => DESCRIPTIONS[this.label()]);
}

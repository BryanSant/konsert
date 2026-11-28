import { ChangeDetectionStrategy, Component } from '@angular/core';
import { ActivitySpinnerComponent } from './activity-spinner.component';
import { PopoverComponent, PopoverTriggerDirective } from './popover.component';
import { SPINNER_LABEL_COLORS, SpinnerMachine, SpinnerState } from './spinner-logic';

const SAMPLE: SpinnerMachine = { name: 'desk', heat: 0.35, cause: 'cpu' };

/** "What the spinners mean": a small popover legend of the ring states and the heat fill. */
@Component({
  selector: 'sy-spinner-legend',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [ActivitySpinnerComponent, PopoverComponent, PopoverTriggerDirective],
  template: `
    <sy-popover label="What the spinners mean" role="dialog" placement="bottom-end" [closeOnSelect]="false" panelClass="w-[320px] p-3 !rounded-[var(--sy-radius-lg)]">
      <button type="button" syPopoverTrigger class="sy-focusable rounded-sm text-[11.5px] text-[#9ec5ff] hover:text-[#cfe2ff] hover:underline" data-testid="spinner-legend">what the spinners mean</button>
      <p class="m-0 mb-2.5 text-[12px] leading-snug text-[var(--sy-text-secondary)]">The fill is the machine and how hot it runs (cool blue → amber → red). The ring is what the agent is doing.</p>
      <ul class="m-0 p-0 list-none grid grid-cols-2 gap-x-3 gap-y-2">
        @for (s of states; track s.word) {
          <li class="flex items-center gap-2 text-[12px]">
            <sy-spinner [state]="s.state" [machines]="s.state === 'queued' ? [] : [sample]" [coarse]="!!s.coarse" [size]="24" [label]="s.word" />
            <span [style.color]="colors[s.state]">{{ s.word }}</span>
          </li>
        }
      </ul>
      <div class="mt-3 flex items-center gap-2 text-[11.5px] text-[var(--sy-text-secondary)]">
        @for (h of heats; track h) {
          <sy-spinner state="working" [machines]="[{ name: 'machine', heat: h, cause: 'cpu' }]" [size]="22" />
        }
        <span>cool → hot</span>
      </div>
    </sy-popover>
  `,
})
export class SpinnerLegendComponent {
  readonly sample = SAMPLE;
  readonly colors = SPINNER_LABEL_COLORS;
  readonly heats = [0.1, 0.5, 0.7, 0.95];
  readonly states: ReadonlyArray<{ state: SpinnerState; word: string; coarse?: boolean }> = [
    { state: 'thinking', word: 'thinking' },
    { state: 'working', word: 'working (a tool runs)' },
    { state: 'speaking', word: 'writing' },
    { state: 'asking', word: 'asks you' },
    { state: 'held', word: 'held / suspended' },
    { state: 'you', word: 'you have control' },
    { state: 'queued', word: 'queued' },
    { state: 'idle', word: 'idle (open, between turns)' },
    { state: 'done', word: 'done' },
    { state: 'error', word: 'error' },
    { state: 'working', word: 'observed (coarse)', coarse: true },
  ];
}

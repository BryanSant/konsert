import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { ChatStatus } from '../../core/models/agent-chat.models';

interface PillLook {
  label: string;
  cls: string;
  dot: 'spin' | 'pulse' | 'solid' | 'hollow';
}

export function statusLook(status: ChatStatus | string): PillLook {
  switch (status) {
    case 'starting':
      return { label: 'Starting', cls: 'text-[#a9c9f7] bg-[var(--ks-blue-soft)]', dot: 'pulse' };
    case 'working':
      return { label: 'Working', cls: 'text-[#a9c9f7] bg-[var(--ks-blue-soft)]', dot: 'spin' };
    case 'needs_you':
      return { label: 'Needs you', cls: 'text-[#f0c27a] bg-[var(--ks-amber-soft)]', dot: 'pulse' };
    case 'done':
      return { label: 'Done', cls: 'text-[#9fd8ab] bg-[var(--ks-green-soft)]', dot: 'solid' };
    case 'failed':
      return { label: 'Failed', cls: 'text-[#f3a5a5] bg-[var(--ks-red-soft)]', dot: 'solid' };
    case 'cancelled':
      return { label: 'Cancelled', cls: 'text-[#b4b6be] bg-[var(--ks-raised)]', dot: 'hollow' };
    default:
      return { label: 'Ready', cls: 'text-[#b4b6be] bg-[var(--ks-raised)]', dot: 'hollow' };
  }
}

/** Chat status pill: starting, working (+ latest tool), needs you, done, failed, cancelled. */
@Component({
  selector: 'ks-status-pill',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'inline-flex min-w-0' },
  template: `
    <span
      class="inline-flex items-center gap-1.5 min-w-0 rounded-full font-medium"
      [class]="look().cls"
      [class.px-2.5]="!compact()"
      [class.py-1]="!compact()"
      [class.text-[11.5px]]="!compact()"
      [class.px-2]="compact()"
      [class.py-0.5]="compact()"
      [class.text-[10.5px]]="compact()"
      [attr.data-status]="status()"
    >
      @switch (look().dot) {
        @case ('spin') {
          <span class="ks-spinner shrink-0" style="width: 9px; height: 9px" aria-hidden="true"></span>
        }
        @case ('pulse') {
          <span class="h-1.5 w-1.5 rounded-full bg-current ks-pulse shrink-0" aria-hidden="true"></span>
        }
        @case ('solid') {
          <span class="h-1.5 w-1.5 rounded-full bg-current shrink-0" aria-hidden="true"></span>
        }
        @default {
          <span class="h-1.5 w-1.5 rounded-full border border-current shrink-0" aria-hidden="true"></span>
        }
      }
      <span class="shrink-0">{{ look().label }}</span>
      @if (status() === 'working' && subtitle()) {
        <span class="truncate font-normal opacity-80 max-w-[260px]">· {{ subtitle() }}</span>
      }
    </span>
  `,
})
export class StatusPillComponent {
  readonly status = input<ChatStatus | string>('idle');
  readonly subtitle = input('');
  readonly compact = input(false);
  readonly look = computed(() => statusLook(this.status()));
}

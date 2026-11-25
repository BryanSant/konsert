import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { ChatStatus } from '../../core/models/agent-chat.models';

interface PillLook {
  label: string;
  cls: string;
  dot: 'spin' | 'pulse' | 'solid' | 'hollow' | 'pause';
}

export function statusLook(status: ChatStatus | string): PillLook {
  switch (status) {
    case 'starting':
      return { label: 'Starting', cls: 'text-[#a9c9f7] bg-[var(--sy-blue-soft)]', dot: 'pulse' };
    case 'working':
      return { label: 'Working', cls: 'text-[#a9c9f7] bg-[var(--sy-blue-soft)]', dot: 'spin' };
    case 'needs_you':
      return { label: 'Needs you', cls: 'text-[#f0c27a] bg-[var(--sy-amber-soft)]', dot: 'pulse' };
    case 'done':
      return { label: 'Done', cls: 'text-[#9fd8ab] bg-[var(--sy-green-soft)]', dot: 'solid' };
    case 'failed':
      return { label: 'Failed', cls: 'text-[#f3a5a5] bg-[var(--sy-red-soft)]', dot: 'solid' };
    case 'cancelled':
      return { label: 'Cancelled', cls: 'text-[#b4b6be] bg-[var(--sy-raised)]', dot: 'hollow' };
    case 'suspended':
      return { label: 'Suspended', cls: 'text-[var(--sy-violet)] bg-[var(--sy-violet-soft)]', dot: 'pause' };
    default:
      return { label: 'Ready', cls: 'text-[#b4b6be] bg-[var(--sy-raised)]', dot: 'hollow' };
  }
}

/** Chat status pill: starting, working (+ latest tool), needs you, done, failed, cancelled. */
@Component({
  selector: 'sy-status-pill',
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
          <span class="sy-spinner shrink-0" style="width: 9px; height: 9px" aria-hidden="true"></span>
        }
        @case ('pulse') {
          <span class="h-1.5 w-1.5 rounded-full bg-current sy-pulse shrink-0" aria-hidden="true"></span>
        }
        @case ('solid') {
          <span class="h-1.5 w-1.5 rounded-full bg-current shrink-0" aria-hidden="true"></span>
        }
        @case ('pause') {
          <svg class="shrink-0" width="8" height="8" viewBox="0 0 8 8" aria-hidden="true">
            <rect x="1" y="1" width="2" height="6" rx="0.5" fill="currentColor" />
            <rect x="5" y="1" width="2" height="6" rx="0.5" fill="currentColor" />
          </svg>
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

import { ChangeDetectionStrategy, Component, computed, input, output, signal } from '@angular/core';
import { AgentApprovalMode } from '../../../core/models/agent-chat.models';
import { ChipTriggerComponent } from '../../../shared/chip-trigger.component';
import { IconComponent } from '../../../shared/icon.component';
import { PopoverComponent, PopoverTriggerDirective } from '../../../shared/popover.component';
import { TooltipDirective } from '../../../shared/tooltip.directive';
import { approvalIcon, approvalTone } from './composer-logic';

/** Shield chip + menu of the account's own approval modes (icon, label, description). */
@Component({
  selector: 'ks-approval-picker',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [PopoverComponent, PopoverTriggerDirective, ChipTriggerComponent, IconComponent, TooltipDirective],
  template: `
    @if (modes().length > 0) {
      <ks-popover label="Approval mode" placement="top-start" panelClass="w-[330px]" [(open)]="open">
        <button
          ksChip
          ksPopoverTrigger
          data-testid="approval-chip"
          [tone]="tone()"
          [disabled]="disabled()"
          [attr.aria-label]="'Approval mode: ' + (current()?.label ?? 'default')"
          [ksTooltip]="current()?.label ?? 'Approval mode'"
        >
          <app-icon [name]="icon()" [size]="14" />
          <span class="hidden sm:inline max-w-[130px] truncate">{{ current()?.label }}</span>
        </button>

        @for (m of modes(); track m.id) {
          @let amber = isAmber(m);
          <button
            type="button"
            role="menuitemradio"
            class="ks-menu-item"
            [attr.aria-checked]="m.id === selectedId()"
            [attr.data-mode]="m.id"
            (click)="selected.emit(m)"
          >
            <span class="mt-0.5 shrink-0" [class.text-[var(--ks-amber)]]="amber" [class.text-[var(--ks-text-secondary)]]="!amber">
              <app-icon [name]="iconOf(m)" [size]="15" />
            </span>
            <span class="flex-1 min-w-0">
              <span class="ks-menu-label block" [class.text-[#f0c27a]]="amber">{{ m.label }}</span>
              @if (m.description) {
                <span class="ks-menu-desc block">{{ m.description }}</span>
              }
            </span>
            <span class="w-3 shrink-0 self-center flex justify-center">
              @if (m.id === selectedId()) {
                <span class="h-1.5 w-1.5 rounded-full bg-[var(--ks-text)]" aria-hidden="true"></span>
              }
            </span>
          </button>
        }
        @if (note()) {
          <div class="px-2.5 pt-1.5 pb-1 text-[11px] text-[var(--ks-text-muted)]">{{ note() }}</div>
        }
      </ks-popover>
    }
  `,
})
export class ApprovalPickerComponent {
  readonly modes = input<AgentApprovalMode[]>([]);
  readonly selectedId = input('');
  readonly disabled = input(false);
  /** e.g. "Applies from the next turn". */
  readonly note = input('');
  readonly selected = output<AgentApprovalMode>();

  readonly open = signal(false);
  readonly current = computed(() => this.modes().find((m) => m.id === this.selectedId()) ?? this.modes()[0] ?? null);
  readonly tone = computed(() => approvalTone(this.current()));
  readonly icon = computed(() => approvalIcon(this.current()));

  isAmber(m: AgentApprovalMode): boolean {
    return approvalTone(m) === 'amber';
  }
  iconOf(m: AgentApprovalMode): string {
    return approvalIcon(m);
  }
}

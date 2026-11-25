import { ChangeDetectionStrategy, Component, computed, input, output, signal } from '@angular/core';
import { AgentApprovalMode } from '../../../core/models/agent-chat.models';
import { ChipTriggerComponent } from '../../../shared/chip-trigger.component';
import { IconComponent } from '../../../shared/icon.component';
import { PopoverComponent, PopoverTriggerDirective } from '../../../shared/popover.component';
import { TooltipDirective } from '../../../shared/tooltip.directive';
import { approvalIcon, approvalTone } from './composer-logic';

let pickerId = 0;

/** Shield chip + menu of the account's own approval modes (icon, label, description). */
@Component({
  selector: 'sy-approval-picker',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [PopoverComponent, PopoverTriggerDirective, ChipTriggerComponent, IconComponent, TooltipDirective],
  template: `
    @if (modes().length > 0) {
      <sy-popover label="Approval mode" placement="top-start" panelClass="w-[330px]" [(open)]="open">
        <button
          syChip
          syPopoverTrigger
          data-testid="approval-chip"
          [tone]="tone()"
          [disabled]="disabled()"
          [attr.aria-label]="'Approval mode: ' + (current()?.label ?? 'default')"
          [syTooltip]="current()?.label ?? 'Approval mode'"
        >
          <app-icon [name]="icon()" [size]="14" />
          <span class="hidden sm:inline max-w-[130px] truncate">{{ current()?.label }}</span>
        </button>

        @for (m of modes(); track m.id) {
          @let off = !!m.disabled;
          @let amber = isAmber(m) && !off;
          <!-- Disabled modes stay in the menu (greyed, aria-disabled, skipped by the arrow
               keys) so the reason is visible and announced instead of the mode vanishing. -->
          <button
            type="button"
            role="menuitemradio"
            class="sy-menu-item"
            [attr.aria-checked]="m.id === selectedId()"
            [attr.aria-disabled]="off ? 'true' : null"
            [attr.aria-describedby]="off ? reasonId(m) : null"
            [attr.data-mode]="m.id"
            [attr.data-disabled]="off ? 'true' : null"
            (click)="choose(m)"
          >
            <span class="mt-0.5 shrink-0" [class.text-[var(--sy-amber)]]="amber" [class.text-[var(--sy-text-secondary)]]="!amber">
              <app-icon [name]="iconOf(m)" [size]="15" />
            </span>
            <span class="flex-1 min-w-0">
              <span class="sy-menu-label block" [class.text-[#f0c27a]]="amber">{{ m.label }}</span>
              @if (m.description) {
                <span class="sy-menu-desc block">{{ m.description }}</span>
              }
              @if (off) {
                <span class="sy-menu-reason" [id]="reasonId(m)" data-testid="approval-disabled-reason">
                  <app-icon name="lock" [size]="11" />
                  <span>{{ m.disabled }}</span>
                </span>
              }
            </span>
            <span class="w-3 shrink-0 self-center flex justify-center">
              @if (m.id === selectedId()) {
                <span class="h-1.5 w-1.5 rounded-full bg-[var(--sy-text)]" aria-hidden="true"></span>
              }
            </span>
          </button>
        }
        @if (note()) {
          <div class="px-2.5 pt-1.5 pb-1 text-[11px] text-[var(--sy-text-muted)]">{{ note() }}</div>
        }
      </sy-popover>
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
  readonly current = computed(
    () => this.modes().find((m) => m.id === this.selectedId()) ?? this.modes().find((m) => !m.disabled) ?? null,
  );
  private readonly uid = ++pickerId;

  /** Picks a mode; disabled modes are never selectable (the popover stays open). */
  choose(m: AgentApprovalMode): void {
    if (m.disabled) return;
    this.selected.emit(m);
  }

  reasonId(m: AgentApprovalMode): string {
    return `sy-approval-${this.uid}-${m.id}-reason`;
  }
  readonly tone = computed(() => approvalTone(this.current()));
  readonly icon = computed(() => approvalIcon(this.current()));

  isAmber(m: AgentApprovalMode): boolean {
    return approvalTone(m) === 'amber';
  }
  iconOf(m: AgentApprovalMode): string {
    return approvalIcon(m);
  }
}

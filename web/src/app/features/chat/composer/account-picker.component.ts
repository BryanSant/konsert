import { ChangeDetectionStrategy, Component, computed, input, output, signal } from '@angular/core';
import { AgentAccount } from '../../../core/models/agent-chat.models';
import {
  accountHealth,
  bindingWindow,
  formatResetTime,
  outOfUsageText,
  usedPercent,
  windowName,
} from '../../../core/models/agent-usage';
import { ChipTriggerComponent } from '../../../shared/chip-trigger.component';
import { IconComponent } from '../../../shared/icon.component';
import { PopoverComponent, PopoverTriggerDirective } from '../../../shared/popover.component';
import { TooltipDirective } from '../../../shared/tooltip.directive';
import { UsageRingComponent } from '../../../shared/usage-ring.component';

/** One line describing an account's state (usage, login, exhausted until X). */
export function accountStateLine(a: AgentAccount, now = new Date()): string {
  const health = accountHealth(a);
  if (health === 'logged_out') return 'Signed out · re-login needed';
  if (a.chat_supported === false) return 'Chat not supported by this CLI';
  const w = bindingWindow(a.usage);
  if (health === 'exhausted' && w) return outOfUsageText(w.window, w.resets_at, now);
  if (w) {
    const pct = usedPercent(a) ?? 0;
    const name = windowName(w.window);
    const reset = formatResetTime(w.resets_at, now);
    return `${pct}% of ${name || 'usage'} used${reset ? ` · resets ${reset}` : ''}`;
  }
  if (a.usage_note) return `Usage not available: ${a.usage_note}`;
  return a.plan ? `${capitalize(a.plan)} plan` : 'Signed in';
}

function capitalize(s: string): string {
  return s ? s[0].toUpperCase() + s.slice(1) : s;
}

/** Account chip ("Claude 2" + usage ring) and its picker menu. */
@Component({
  selector: 'ks-account-picker',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [PopoverComponent, PopoverTriggerDirective, ChipTriggerComponent, IconComponent, TooltipDirective, UsageRingComponent],
  template: `
    <ks-popover label="Agent account" placement="top-start" panelClass="w-[320px] max-h-[360px] overflow-y-auto" [(open)]="open">
      <button
        ksChip
        ksPopoverTrigger
        data-testid="account-chip"
        [tone]="chipTone()"
        [disabled]="disabled()"
        [attr.aria-label]="'Agent account: ' + chipLabel()"
        [ksTooltip]="chipTooltip()"
      >
        @if (current(); as a) {
          <app-icon [name]="a.agent" [size]="13" />
          <span class="max-w-[120px] truncate">{{ a.label }}</span>
          @if (health() === 'logged_out') {
            <span class="font-semibold">Re-login</span>
          } @else if (percent() !== null) {
            <ks-usage-ring [percent]="percent()!" [tone]="health()" [size]="13" />
            @if (health() === 'exhausted') {
              <span class="text-[#f3a5a5] font-medium">Out</span>
            } @else if (health() === 'warning') {
              <span class="font-mono-ide text-[11px]">{{ percent() }}%</span>
            }
          }
        } @else {
          <app-icon name="orchestrator" [size]="13" />
          <span>No agent</span>
        }
      </button>

      @if (accounts().length === 0) {
        <div class="px-3 py-3 text-[12px] text-[var(--ks-text-muted)]">
          No agent accounts reported by this machine yet. Install and sign in to an agent CLI, then refresh.
        </div>
      }
      @for (a of accounts(); track a.id) {
        @let h = healthOf(a);
        <button
          type="button"
          role="menuitemradio"
          class="ks-menu-item items-center"
          [attr.aria-checked]="a.id === selectedId()"
          [attr.aria-disabled]="a.chat_supported === false ? 'true' : null"
          [attr.data-account]="a.id"
          (click)="choose(a)"
        >
          <app-icon [name]="a.agent" [size]="15" />
          <span class="flex-1 min-w-0">
            <span class="ks-menu-label flex items-center gap-1.5">
              <span class="truncate">{{ a.label }}</span>
              @if (a.plan) {
                <span class="text-[10.5px] font-normal text-[var(--ks-text-muted)]">{{ a.plan }}</span>
              }
            </span>
            <span
              class="ks-menu-desc block truncate"
              [class.!text-[#f3a5a5]]="h === 'logged_out' || h === 'exhausted'"
              [class.!text-[#f0c27a]]="h === 'warning'"
            >
              {{ stateLine(a) }}
            </span>
          </span>
          @if (pctOf(a) !== null && h !== 'logged_out') {
            <ks-usage-ring [percent]="pctOf(a)!" [tone]="h" [size]="15" />
          }
          <span class="w-3.5 shrink-0 text-[var(--ks-text)]">
            @if (a.id === selectedId()) {
              <app-icon name="check" [size]="14" />
            }
          </span>
        </button>
      }
      <div class="ks-menu-sep"></div>
      <button type="button" role="menuitem" class="ks-menu-item items-center" (click)="refresh.emit()">
        <app-icon name="refresh" [size]="14" />
        <span class="ks-menu-desc !text-[var(--ks-text-secondary)]">Refresh login and usage</span>
      </button>
    </ks-popover>
  `,
})
export class AccountPickerComponent {
  readonly accounts = input<AgentAccount[]>([]);
  readonly selectedId = input('');
  readonly disabled = input(false);
  readonly selected = output<AgentAccount>();
  readonly refresh = output<void>();

  readonly open = signal(false);
  readonly current = computed(() => this.accounts().find((a) => a.id === this.selectedId()) ?? null);
  readonly health = computed(() => (this.current() ? accountHealth(this.current()!) : 'unknown'));
  readonly percent = computed(() => (this.current() ? usedPercent(this.current()!) : null));
  readonly chipTone = computed(() => {
    const h = this.health();
    if (h === 'logged_out') return 'red' as const;
    if (h === 'warning') return 'amber' as const;
    return 'default' as const;
  });
  readonly chipLabel = computed(() => {
    const a = this.current();
    return a ? `${a.label}, ${accountStateLine(a)}` : 'none';
  });
  readonly chipTooltip = computed(() => {
    const a = this.current();
    return a ? `${a.label} · ${accountStateLine(a)}` : 'Choose an agent account';
  });

  healthOf(a: AgentAccount) {
    return accountHealth(a);
  }
  pctOf(a: AgentAccount) {
    return usedPercent(a);
  }
  stateLine(a: AgentAccount) {
    return accountStateLine(a);
  }

  choose(a: AgentAccount): void {
    if (a.chat_supported === false) return;
    this.selected.emit(a);
  }
}

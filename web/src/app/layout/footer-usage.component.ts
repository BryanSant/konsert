import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { AgentAccount, AgentRateLimit } from '../core/models/agent-chat.models';
import {
  accountHealth,
  bindingWindow,
  formatCountdown,
  formatResetTime,
  isExhausted,
  usedPercent,
  windowName,
} from '../core/models/agent-usage';
import { AgentAccountsStore } from '../core/state/agent-accounts.store';
import { FleetStore } from '../core/state/fleet.store';
import { ReloginService } from '../features/chat/relogin.service';
import { IconComponent } from '../shared/icon.component';
import { PopoverComponent, PopoverTriggerDirective } from '../shared/popover.component';
import { TooltipDirective } from '../shared/tooltip.directive';
import { UsageRingComponent } from '../shared/usage-ring.component';

export interface FooterAccountRow {
  workerId: string;
  host: string;
  account: AgentAccount;
  health: ReturnType<typeof accountHealth>;
  percent: number | null;
  /** One line: "42% of 5-hour · resets 3:40 PM", "Exhausted until Tue 3:40 PM", ... */
  state: string;
  windows: AgentRateLimit[];
}

/** The account state line for the footer (pure, for tests). */
export function footerStateLine(a: AgentAccount, now = new Date()): string {
  const health = accountHealth(a);
  if (health === 'logged_out') return 'Signed out';
  const w = bindingWindow(a.usage);
  if (!w) return a.usage_note ? `Not available: ${a.usage_note}` : 'No usage reported';
  const name = windowName(w.window);
  const reset = formatResetTime(w.resets_at, now);
  if (isExhausted(w)) return `Exhausted until ${reset || 'reset'}${name ? ` (${name})` : ''}`;
  return `${usedPercent(a)}% of ${name || 'usage'}${reset ? ` · resets ${reset}` : ''}`;
}

/** Footer: per-account subscription usage, plus an alert for signed-out accounts. */
@Component({
  selector: 'ks-footer-usage',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [PopoverComponent, PopoverTriggerDirective, IconComponent, UsageRingComponent, TooltipDirective],
  host: { class: 'flex items-center gap-1 min-w-0' },
  template: `
    <ks-popover label="Subscription usage" role="dialog" placement="top-start" panelClass="w-[360px] max-w-[92vw] !p-0" [closeOnSelect]="false" [(open)]="open">
      <button
        type="button"
        ksPopoverTrigger
        class="ks-btn ks-btn-sm !h-6 !px-2 gap-2 min-w-0"
        data-testid="footer-usage"
        [attr.aria-label]="'Subscription usage: ' + summaryLabel()"
      >
        @if (summary().length > 0) {
          @for (r of summary(); track r.workerId + r.account.id) {
            <span class="inline-flex items-center gap-1 shrink-0" [attr.data-account]="r.account.id">
              <app-icon [name]="r.account.agent" [size]="11" />
              <span class="text-[#b4b6be]">{{ r.account.label }}</span>
              <ks-usage-ring [percent]="r.percent ?? 0" [tone]="r.health" [size]="11" />
              <span
                class="font-mono-ide"
                [class.text-[#f3a5a5]]="r.health === 'exhausted'"
                [class.text-[#f0c27a]]="r.health === 'warning'"
              >
                {{ r.health === 'exhausted' ? 'out' : r.percent + '%' }}
              </span>
            </span>
          }
        } @else {
          <span class="text-[var(--ks-text-muted)]">Usage</span>
        }
      </button>

      <div class="flex items-center justify-between px-3.5 pt-3 pb-2">
        <span class="text-[12.5px] font-semibold text-[var(--ks-text)]">Subscription usage</span>
        <button type="button" class="ks-btn ks-btn-sm" [disabled]="accounts.refreshing()" (click)="accounts.refresh()">
          <app-icon name="refresh" [size]="12" />
          {{ accounts.refreshing() ? 'Checking…' : 'Refresh' }}
        </button>
      </div>
      <div class="max-h-[340px] overflow-y-auto px-2 pb-2 space-y-0.5" data-testid="usage-list">
        @for (r of rows(); track r.workerId + r.account.id) {
          <div class="rounded-[var(--ks-radius-md)] px-2 py-2 hover:bg-[var(--ks-raised)]" [attr.data-account-row]="r.account.id" [attr.data-health]="r.health">
            <div class="flex items-center gap-2">
              <app-icon [name]="r.account.agent" [size]="14" />
              <span class="flex-1 min-w-0">
                <span class="block truncate text-[12.5px] font-medium text-[var(--ks-text)]">
                  {{ r.account.label }}
                  @if (multiHost()) {
                    <span class="font-normal text-[var(--ks-text-muted)]">· {{ r.host }}</span>
                  }
                </span>
                <span
                  class="block truncate text-[11.5px]"
                  [class.text-[#f3a5a5]]="r.health === 'exhausted' || r.health === 'logged_out'"
                  [class.text-[#f0c27a]]="r.health === 'warning'"
                  [class.text-[var(--ks-text-muted)]]="r.health === 'ok' || r.health === 'unknown'"
                  data-testid="usage-state"
                >
                  {{ r.state }}
                </span>
              </span>
              @if (r.health === 'logged_out') {
                <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" (click)="relogin(r)">Sign in</button>
              } @else if (r.percent !== null) {
                <ks-usage-ring [percent]="r.percent" [tone]="r.health" [size]="16" />
              }
            </div>
            @if (r.windows.length > 1) {
              <div class="mt-1.5 ml-[22px] space-y-1">
                @for (w of r.windows; track w.window) {
                  <div class="flex items-center gap-2 text-[11px] text-[var(--ks-text-muted)]">
                    <span class="w-[86px] shrink-0">{{ windowLabel(w.window) }}</span>
                    <span class="h-1 flex-1 rounded-full bg-white/10 overflow-hidden">
                      <span
                        class="block h-full rounded-full"
                        [style.width.%]="windowPct(w)"
                        [class.bg-[var(--ks-red)]]="windowExhausted(w)"
                        [class.bg-[var(--ks-amber)]]="!windowExhausted(w) && windowPct(w) >= 80"
                        [class.bg-[var(--ks-blue)]]="!windowExhausted(w) && windowPct(w) < 80"
                      ></span>
                    </span>
                    <span class="w-[92px] shrink-0 text-right font-mono-ide">{{ windowExhausted(w) ? 'out ' + countdown(w.resets_at) : windowPct(w) + '%' }}</span>
                  </div>
                }
              </div>
            }
          </div>
        } @empty {
          <div class="px-2 py-3 text-[12px] text-[var(--ks-text-muted)]">
            No agent accounts reported yet. Connected machines report their signed-in agent CLIs here.
          </div>
        }
      </div>
      @if (fleet.usage().total_requests > 0) {
        <div class="flex items-center justify-between border-t border-[var(--ks-hairline)] px-3.5 py-2 text-[11px] text-[var(--ks-text-muted)]">
          <span>API-key orchestrator</span>
          <span class="font-mono-ide">{{ apiTokens() }} tokens · {{ fleet.usage().total_requests }} calls</span>
        </div>
      }
    </ks-popover>

    @for (r of loggedOut(); track r.workerId + r.account.id) {
      <button
        type="button"
        class="ks-btn ks-btn-sm ks-chip-red !h-6 !px-2 gap-1.5 shrink-0"
        data-testid="footer-auth-alert"
        [attr.aria-label]="r.account.label + ' on ' + r.host + ' is signed out. Show how to sign in'"
        [ksTooltip]="r.account.label + ' needs you to sign in again'"
        (click)="relogin(r)"
      >
        <app-icon name="key" [size]="11" />
        <span class="hidden sm:inline">{{ r.account.label }} signed out</span>
      </button>
    }
  `,
})
export class FooterUsageComponent {
  readonly accounts = inject(AgentAccountsStore);
  readonly fleet = inject(FleetStore);
  private readonly reloginSvc = inject(ReloginService);
  readonly open = signal(false);

  readonly rows = computed<FooterAccountRow[]>(() =>
    this.accounts.all().map(({ workerId, account }) => ({
      workerId,
      host: this.fleet.workersById()[workerId]?.name || workerId,
      account,
      health: accountHealth(account),
      percent: usedPercent(account),
      state: footerStateLine(account),
      windows: account.usage ?? [],
    })),
  );
  readonly multiHost = computed(() => new Set(this.rows().map((r) => r.workerId)).size > 1);
  /** Accounts with a usage number, shown inline in the footer (max 3, most used first). */
  readonly summary = computed(() =>
    this.rows()
      .filter((r) => r.percent !== null && r.health !== 'logged_out')
      .sort((a, b) => (b.percent ?? 0) - (a.percent ?? 0))
      .slice(0, 3),
  );
  readonly summaryLabel = computed(() =>
    this.summary().length ? this.summary().map((r) => `${r.account.label} ${r.state}`).join('; ') : 'none reported',
  );
  readonly loggedOut = computed(() => this.rows().filter((r) => r.health === 'logged_out'));
  readonly apiTokens = computed(() => {
    const u = this.fleet.usage();
    return (u.total_input_tokens + u.total_output_tokens).toLocaleString();
  });

  relogin(r: FooterAccountRow): void {
    this.open.set(false);
    this.reloginSvc.show({ workerId: r.workerId, accountId: r.account.id, label: r.account.label, relogin: r.account.auth?.relogin });
  }

  windowLabel(w: string): string {
    const n = windowName(w);
    return n ? n[0].toUpperCase() + n.slice(1) : w;
  }
  windowPct(w: AgentRateLimit): number {
    return isExhausted(w) ? 100 : Math.round(Math.max(0, Math.min(100, w.used_percent ?? 0)));
  }
  windowExhausted(w: AgentRateLimit): boolean {
    return isExhausted(w);
  }
  countdown(iso?: string): string {
    return formatCountdown(iso);
  }
}

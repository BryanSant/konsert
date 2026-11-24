import { ChangeDetectionStrategy, Component, computed, inject } from '@angular/core';
import { RouterLink } from '@angular/router';
import { accountHealth, bindingWindow, formatResetTime, usedPercent } from '../../core/models/agent-usage';
import { AgentAccountsStore } from '../../core/state/agent-accounts.store';
import { FleetStore } from '../../core/state/fleet.store';
import { machineLoadLabel, occupiesSlot } from '../../core/state/work-logic';
import { IconComponent } from '../../shared/icon.component';
import { HeatAvatarComponent } from '../../shared/activity-spinner.component';
import { heatTextColor, spinnerMachine } from '../../shared/spinner-logic';
import { heatLevelOf } from '../../core/state/activity-logic';

/** Read-only fleet and quota summary for a phone. */
@Component({
  selector: 'ks-mobile-fleet',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RouterLink, IconComponent, HeatAvatarComponent],
  host: { class: 'flex flex-col h-full min-h-0 overflow-y-auto' },
  template: `
    <div class="px-4 pt-4 pb-1">
      <a routerLink="/inbox" class="ks-btn ks-btn-sm h-[44px] -ml-2 no-underline">
        <span class="inline-flex rotate-180"><app-icon name="chevron-right" [size]="12" /></span>
        Inbox
      </a>
    </div>
    <h1 class="px-4 m-0 pb-2 text-[15px] font-semibold">Fleet (read-only)</h1>
    <ul class="m-0 p-0 list-none" data-testid="mobile-fleet">
      @for (m of machines(); track m.id) {
        <li class="grid grid-cols-[28px_minmax(0,1fr)_auto_96px] items-center gap-2.5 px-4 py-2.5 border-b border-[var(--ks-hairline)] text-[14px]" [attr.data-machine]="m.name">
          <ks-heat-avatar [machine]="m.machine" [size]="28" />
          <span class="truncate">{{ m.name }}</span>
          <span class="text-[12.5px]" [style.color]="m.heatColor">{{ m.heatWord }}</span>
          <span class="text-right text-[12.5px]" [class.text-[var(--ks-amber)]]="m.tone === 'warn'" [class.text-[var(--ks-text-muted)]]="m.tone !== 'warn'">{{ m.note }}</span>
        </li>
      } @empty {
        <li class="px-4 py-3 text-[13px] text-[var(--ks-text-muted)]">No machines yet.</li>
      }
    </ul>
    <h2 class="px-4 pt-4 mb-1 text-[12px] font-semibold uppercase tracking-wider text-[var(--ks-text-secondary)]">Quota</h2>
    <ul class="m-0 p-0 list-none" data-testid="mobile-quota">
      @for (q of quotas(); track q.key) {
        <li class="flex justify-between px-4 py-1.5 text-[14px]">
          <span>{{ q.label }}</span>
          <span [class.text-[var(--ks-amber)]]="q.warn" [class.text-[var(--ks-text-secondary)]]="!q.warn">{{ q.text }}</span>
        </li>
      } @empty {
        <li class="px-4 py-1.5 text-[13px] text-[var(--ks-text-muted)]">No usage reported yet.</li>
      }
    </ul>
    <p class="px-4 py-4 text-[12.5px] leading-relaxed text-[var(--ks-text-muted)]">
      No board, wall or placement editing on a phone. Terminals only as an emergency take-control, behind a confirmation.
    </p>
  `,
})
export class MobileFleetComponent {
  private readonly fleet = inject(FleetStore);
  private readonly accounts = inject(AgentAccountsStore);

  readonly machines = computed(() => {
    const running = new Map<string, number>();
    for (const t of this.fleet.tasks()) {
      if (occupiesSlot(t)) running.set(t.worker_id, (running.get(t.worker_id) ?? 0) + 1);
    }
    const heat = this.fleet.heatByWorker();
    return this.fleet.workers().map((w) => {
      const n = running.get(w.id) ?? 0;
      const label = machineLoadLabel(w, n);
      const h = heat[w.id];
      return {
        id: w.id,
        name: w.name,
        machine: spinnerMachine(w.name, h),
        heatWord: h ? `${heatLevelOf(h.score)} ${Math.round(h.score * 100)}%` : w.status === 'online' ? '' : 'offline',
        heatColor: heatTextColor(h ? h.score : null),
        tone: label.tone,
        note: label.text.slice(w.name.length).trim() || `${n}/${w.max_concurrent || '∞'}`,
      };
    });
  });

  readonly quotas = computed(() =>
    this.accounts
      .all()
      .map((r) => {
        const a = r.account;
        const key = `${r.workerId}:${a.id}`;
        if (accountHealth(a) === 'logged_out') return { key, label: a.label || a.id, text: 'signed out', warn: true };
        const pct = usedPercent(a);
        if (pct == null) return null;
        const w = bindingWindow(a.usage);
        const reset = pct >= 75 && w?.resets_at ? ` → ${formatResetTime(w.resets_at)}` : '';
        return { key, label: a.label || a.id, text: `${pct}%${reset}`, warn: pct >= 75 };
      })
      .filter((x): x is { key: string; label: string; text: string; warn: boolean } => !!x),
  );
}

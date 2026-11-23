import { ChangeDetectionStrategy, Component, computed, inject } from '@angular/core';
import { RouterLink } from '@angular/router';
import { accountHealth, bindingWindow, formatResetTime, usedPercent } from '../../core/models/agent-usage';
import { AgentAccountsStore } from '../../core/state/agent-accounts.store';
import { FleetStore } from '../../core/state/fleet.store';
import { machineLoadLabel, occupiesSlot } from '../../core/state/work-logic';

/** Command's bottom strip: each machine's load and each subscription's quota, at a glance. */
@Component({
  selector: 'ks-fleet-strip',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RouterLink],
  host: { class: 'hidden md:block shrink-0' },
  template: `
    <footer class="flex flex-wrap items-center gap-x-4 gap-y-1 px-4 py-1.5 text-[12px] text-[var(--ks-text-secondary)] bg-[var(--ks-bg-chrome)] border-t border-[var(--ks-hairline)]" aria-label="Fleet and quota" data-testid="fleet-strip">
      @if (machines().length === 0) {
        <span class="text-[var(--ks-text-muted)]">No machines yet</span>
      }
      @for (m of machines(); track m.id) {
        <a
          [routerLink]="['/fleet', m.sid]"
          class="no-underline hover:underline"
          [class.text-[var(--ks-amber)]]="m.tone === 'warn'"
          [class.text-[var(--ks-text-muted)]]="m.tone === 'stale'"
          [class.text-[var(--ks-text-secondary)]]="m.tone === 'muted'"
          >{{ m.text }}</a
        >
      }
      <span class="flex-1"></span>
      @for (q of quotas(); track q.key) {
        <span [class.text-[var(--ks-amber)]]="q.warn" [class.text-[var(--ks-text-muted)]]="q.out" data-testid="quota">{{ q.text }}</span>
      }
    </footer>
  `,
})
export class FleetStripComponent {
  private readonly fleet = inject(FleetStore);
  private readonly accounts = inject(AgentAccountsStore);

  readonly machines = computed(() => {
    const running = new Map<string, number>();
    for (const t of this.fleet.tasks()) {
      if (occupiesSlot(t)) running.set(t.worker_id, (running.get(t.worker_id) ?? 0) + 1);
    }
    return this.fleet.workers().map((w) => ({ id: w.id, sid: w.sid, ...machineLoadLabel(w, running.get(w.id) ?? 0) }));
  });

  readonly quotas = computed(() => {
    const seen = new Set<string>();
    const out: Array<{ key: string; text: string; warn: boolean; out: boolean }> = [];
    for (const r of this.accounts.all()) {
      const a = r.account;
      const key = `${a.agent}:${a.label || a.id}`;
      if (seen.has(key)) continue;
      seen.add(key);
      const health = accountHealth(a);
      const label = a.label || a.id;
      if (health === 'logged_out') {
        out.push({ key, text: `${label} signed out`, warn: true, out: false });
        continue;
      }
      const pct = usedPercent(a);
      if (pct == null) continue;
      const w = bindingWindow(a.usage);
      const reset = pct >= 75 && w?.resets_at ? ` → ${formatResetTime(w.resets_at)}` : '';
      out.push({ key, text: `${label} ${pct}%${reset}`, warn: pct >= 75, out: false });
    }
    return out.slice(0, 8);
  });
}

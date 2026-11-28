import { ChangeDetectionStrategy, Component, OnInit, computed, inject, signal } from '@angular/core';
import { PolicyReport } from '../core/models/syre.models';
import { ApiClientService } from '../core/services/api-client.service';

/** Plain names of the rules and directives a report row can be about. */
const RULE_LABELS: Record<string, string> = {
  role: 'Control-only role',
  allowed_work: 'Allowed work',
  quiet_hours: 'Quiet hours',
  max_sessions: 'Max sessions',
  max_load: 'Max load',
  reserve_cpu: 'CPU kept for you',
  min_free_ram: 'RAM kept free',
  'machine.pause_or_drain': 'Paused or draining',
  'machine.throttle': 'Throttle',
  hold: 'Holds',
};

export function ruleLabel(rule: string): string {
  return RULE_LABELS[rule] ?? rule;
}

/**
 * Settings → Policy report (SYR-ADQZX): over the last week, how often each machine rule
 * kept work off a machine, and whether the Commander broke the glass (asked for an
 * override), with what came of it.
 */
@Component({
  selector: 'sy-policy-report',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section class="rounded-[var(--sy-radius-lg)] bg-black/20 p-4 space-y-3 text-[12px]" data-testid="policy-report" aria-labelledby="sy-policy-report-title">
      <div id="sy-policy-report-title" class="text-[11px] font-semibold uppercase tracking-wider text-[#8e9098]">Policy report · last 7 days</div>
      @if (error()) {
        <p class="m-0 text-[11.5px] text-[#f0a3a3]" role="alert">{{ error() }}</p>
      } @else if (report(); as r) {
        <p class="m-0 text-[11.5px] text-[#a1a3ab]" data-testid="policy-report-summary">{{ summary() }}</p>
        @if (rows().length) {
          <table class="w-full text-left text-[11.5px]" data-testid="policy-report-rows">
            <thead class="text-[10.5px] uppercase tracking-wider text-[#8e9098]">
              <tr><th class="py-1 font-medium">Machine</th><th class="py-1 font-medium">Rule</th><th class="py-1 font-medium text-right">Blocked</th><th class="py-1 font-medium">Overrides</th></tr>
            </thead>
            <tbody>
              @for (row of rows(); track row.worker_id + row.rule) {
                <tr class="border-t border-white/5 align-top">
                  <td class="py-1.5 pr-2 text-[#d4d4d8]">{{ row.worker_name || row.worker_id }}</td>
                  <td class="py-1.5 pr-2 text-[#d4d4d8]" [attr.title]="row.last_message || null">{{ label(row.rule) }}</td>
                  <td class="py-1.5 pr-2 text-right font-mono-ide text-[#ececed]">{{ row.blocks }}</td>
                  <td class="py-1.5 text-[#a1a3ab]">{{ overrideText(row) }}</td>
                </tr>
              }
            </tbody>
          </table>
        }
        @if (overrides().length) {
          <details class="rounded-[var(--sy-radius-md)] bg-black/20" data-testid="policy-report-overrides">
            <summary class="cursor-pointer list-none px-3 py-2 text-[11.5px] text-[#a1a3ab]">Override requests ({{ overrides().length }})</summary>
            <ul class="m-0 list-none space-y-1.5 px-3 pb-3">
              @for (o of overrides(); track o.id) {
                <li class="text-[11.5px] text-[#d4d4d8]">
                  <span class="font-mono-ide text-[#8e9098]">{{ o.sid }}</span> {{ o.status }}{{ o.use_count ? ', used ' + o.use_count + '×' : '' }}:
                  <span class="text-[#a1a3ab]">{{ o.reason }}</span>
                </li>
              }
            </ul>
          </details>
        }
      } @else {
        <p class="m-0 text-[11.5px] text-[#8e9098]">Loading…</p>
      }
    </section>
  `,
})
export class PolicyReportComponent implements OnInit {
  private readonly api = inject(ApiClientService);

  readonly report = signal<PolicyReport | null>(null);
  readonly error = signal('');
  readonly rows = computed(() => this.report()?.rows ?? []);
  readonly overrides = computed(() => this.report()?.overrides ?? []);

  readonly summary = computed(() => {
    const t = this.report()?.totals;
    if (!t) return '';
    if (!t.Blocks && !t.Requested) return 'No rule kept work off a machine, and no override was requested.';
    const blocks = `${t.Blocks} piece${t.Blocks === 1 ? '' : 's'} of work held back by machine rules`;
    if (!t.Requested) return `${blocks}; no override was requested.`;
    return `${blocks}; ${t.Requested} override${t.Requested === 1 ? '' : 's'} requested: ${t.Approved} approved, ${t.Denied} denied, ${t.Used} used.`;
  });

  async ngOnInit(): Promise<void> {
    try {
      this.report.set(await this.api.getPolicyReport(7));
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not load the policy report');
    }
  }

  label(rule: string): string {
    return ruleLabel(rule);
  }

  overrideText(row: { overrides_requested: number; overrides_approved: number; overrides_denied: number; overrides_used: number; followed: boolean }): string {
    if (!row.overrides_requested) return 'none';
    const parts = [`${row.overrides_requested} asked`];
    if (row.overrides_approved) parts.push(`${row.overrides_approved} approved`);
    if (row.overrides_denied) parts.push(`${row.overrides_denied} denied`);
    if (row.overrides_used) parts.push(`${row.overrides_used} used`);
    return parts.join(', ') + (row.followed ? '' : ' (before any block)');
  }
}

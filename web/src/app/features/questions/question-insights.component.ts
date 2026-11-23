import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { DELEGATABLE_CATEGORIES, QuestionMetrics, QuestionSettings } from '../../core/models/question.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { formatDuration } from '../../core/state/questions-logic';

/**
 * Settings → Questions (PRD §5.8b): question quality over the last week (asked,
 * answered with/without a side conversation, time blocked, delegations, which agents ask
 * unclear questions), and the per-category "let the orchestrator decide" rules (off by
 * default). Rules save as they change.
 */
@Component({
  selector: 'ks-question-insights',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section class="rounded-[var(--ks-radius-lg)] bg-black/20 p-4 space-y-3 text-[12px]" data-testid="question-insights" aria-labelledby="ks-q-insights">
      <div id="ks-q-insights" class="text-[11px] font-semibold uppercase tracking-wider text-[#8e9098]">Questions · last 7 days</div>
      @if (metrics(); as m) {
        <dl class="grid grid-cols-3 gap-2" data-testid="question-metrics">
          <div><dt class="text-[10.5px] text-[var(--ks-text-muted)]">Asked</dt><dd class="text-[15px] font-semibold text-[var(--ks-text)]" data-metric="asked">{{ m.asked }}</dd></div>
          <div><dt class="text-[10.5px] text-[var(--ks-text-muted)]">Answered</dt><dd class="text-[15px] font-semibold text-[var(--ks-text)]" data-metric="answered">{{ m.answered }}</dd></div>
          <div><dt class="text-[10.5px] text-[var(--ks-text-muted)]">Open now</dt><dd class="text-[15px] font-semibold text-[var(--ks-text)]">{{ m.open }}</dd></div>
          <div><dt class="text-[10.5px] text-[var(--ks-text-muted)]">Needed a side conversation</dt><dd class="text-[13px] text-[var(--ks-text)]" data-metric="clarified">{{ m.answered_with_clarification }} of {{ m.answered }}</dd></div>
          <div><dt class="text-[10.5px] text-[var(--ks-text-muted)]">Blocked (median / p90)</dt><dd class="text-[13px] text-[var(--ks-text)]">{{ dur(m.median_blocked_seconds) }} / {{ dur(m.p90_blocked_seconds) }}</dd></div>
          <div><dt class="text-[10.5px] text-[var(--ks-text-muted)]">Delegated (overridden)</dt><dd class="text-[13px] text-[var(--ks-text)]" data-metric="delegated">{{ m.delegated }} ({{ m.delegation_overrides }})</dd></div>
          <div class="col-span-3"><dt class="text-[10.5px] text-[var(--ks-text-muted)]">Sent back as under-specified</dt><dd class="text-[13px] text-[var(--ks-text)]" data-metric="rejected">{{ m.rejected }}</dd></div>
        </dl>
        @if (m.agents.length) {
          <table class="w-full text-left text-[11.5px]" data-testid="question-agents">
            <caption class="ks-sr-only">Question clarity per agent</caption>
            <thead class="text-[10.5px] text-[var(--ks-text-muted)]">
              <tr><th class="font-normal py-1">Agent</th><th class="font-normal">Asked</th><th class="font-normal">Unclear</th><th class="font-normal">Sent back</th><th class="font-normal">Delegated</th></tr>
            </thead>
            <tbody>
              @for (a of m.agents; track a.agent) {
                <tr class="text-[var(--ks-text-secondary)]" [attr.data-agent]="a.agent">
                  <td class="py-0.5 text-[var(--ks-text)]">{{ a.agent }}</td>
                  <td>{{ a.asked }}</td>
                  <td [class.text-[var(--ks-amber)]]="a.unclear_rate >= 50">{{ a.unclear_rate }}%</td>
                  <td>{{ a.rejected }}</td>
                  <td>{{ a.delegated }}</td>
                </tr>
              }
            </tbody>
          </table>
          <p class="text-[10.5px] text-[var(--ks-text-muted)]">Unclear: the question needed a side conversation or was sent back for missing context first.</p>
        }
      } @else if (error()) {
        <p class="text-[var(--ks-text-muted)]">{{ error() }}</p>
      }

      <fieldset class="space-y-1.5 pt-1">
        <legend class="text-[11px] text-[#8e9098] mb-1">Let the orchestrator decide automatically (off by default)</legend>
        @for (c of categories; track c.id) {
          <label class="flex items-start gap-2 cursor-pointer">
            <input type="checkbox" class="mt-0.5" [attr.data-testid]="'auto-delegate-' + c.id" [checked]="isAuto(c.id)" (change)="toggle(c.id, $any($event.target).checked)" />
            <span><span class="text-[var(--ks-text)]">{{ c.label }}</span> <span class="text-[var(--ks-text-muted)]">— {{ c.hint }}</span></span>
          </label>
        }
        <label class="flex items-center gap-2 pt-1">
          <span class="text-[var(--ks-text-secondary)]">Hold the orchestrator's decision for</span>
          <input type="number" min="0" max="3600" class="ks-field ks-field-sm w-20" data-testid="delegation-grace" [value]="settings().delegation_grace_seconds" (change)="setGrace($any($event.target).value)" />
          <span class="text-[var(--ks-text-secondary)]">seconds so you can override it</span>
        </label>
      </fieldset>
    </section>
  `,
})
export class QuestionInsightsComponent implements OnInit {
  private readonly api = inject(ApiClientService);
  readonly metrics = signal<QuestionMetrics | null>(null);
  readonly settings = signal<QuestionSettings>({ auto_delegate: [], delegation_grace_seconds: 30 });
  readonly error = signal('');
  readonly categories = DELEGATABLE_CATEGORIES;

  async ngOnInit(): Promise<void> {
    try {
      const [m, s] = await Promise.all([this.api.questionMetrics(7), this.api.getQuestionSettings()]);
      this.metrics.set(m);
      this.settings.set({ auto_delegate: s.auto_delegate ?? [], delegation_grace_seconds: s.delegation_grace_seconds ?? 30 });
    } catch {
      this.error.set('Question metrics are not available.');
    }
  }

  dur(s: number): string {
    return formatDuration(s);
  }

  isAuto(id: string): boolean {
    return this.settings().auto_delegate.includes(id);
  }

  toggle(id: string, on: boolean): void {
    const cur = this.settings().auto_delegate.filter((c) => c !== id);
    void this.save({ ...this.settings(), auto_delegate: on ? [...cur, id] : cur });
  }

  setGrace(v: string): void {
    const n = Math.max(0, Math.min(3600, Math.round(Number(v) || 0)));
    void this.save({ ...this.settings(), delegation_grace_seconds: n });
  }

  private async save(s: QuestionSettings): Promise<void> {
    this.settings.set(s);
    try {
      this.settings.set(await this.api.putQuestionSettings(s));
    } catch {
      this.error.set('Could not save the question settings.');
    }
  }
}

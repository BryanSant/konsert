import { ChangeDetectionStrategy, Component, computed, input, output, signal } from '@angular/core';
import { AgentAccount, AgentChatError, AgentRelogin } from '../../../core/models/agent-chat.models';
import { formatCountdown, formatResetTime, outOfUsageText } from '../../../core/models/agent-usage';
import { IconComponent } from '../../../shared/icon.component';
import { ReloginHelpComponent } from '../relogin-help.component';

export interface ErrorCardView {
  icon: string;
  title: string;
  detail: string;
  tone: 'red' | 'amber';
}

/** Headline per AgentChatError.kind (pure, for tests). */
export function errorCardView(err: AgentChatError, ctx: { agent?: string; accountLabel?: string; now?: Date } = {}): ErrorCardView {
  const who = ctx.accountLabel || ctx.agent || 'The agent';
  const now = ctx.now ?? new Date();
  switch (err.kind) {
    case 'usage_limit':
      return { icon: 'clock', title: outOfUsageText(err.window, err.resets_at, now), detail: err.message, tone: 'amber' };
    case 'auth':
      return { icon: 'key', title: `${who} needs you to sign in again`, detail: err.message, tone: 'red' };
    case 'exit':
      return {
        icon: 'alert',
        title: `${ctx.agent || 'The agent'} exited${err.exit_code !== undefined && err.exit_code !== null ? ` with code ${err.exit_code}` : ''}`,
        detail: err.message,
        tone: 'red',
      };
    case 'timeout':
      return { icon: 'clock', title: 'No progress for a while', detail: err.message || 'The agent has not reported anything recently. It may still be working.', tone: 'amber' };
    case 'rate_limit':
      return { icon: 'clock', title: 'The service is busy', detail: err.message, tone: 'amber' };
    case 'not_installed':
      return { icon: 'alert', title: `${ctx.agent || 'The agent'} is not installed on this machine`, detail: err.message, tone: 'red' };
    case 'model':
      return { icon: 'alert', title: 'That model is not available', detail: `${err.message} Pick another model below.`, tone: 'red' };
    case 'protocol':
      return { icon: 'alert', title: 'The agent sent something Konsert could not read', detail: err.message, tone: 'red' };
    default:
      return { icon: 'alert', title: 'Something went wrong', detail: err.message, tone: 'red' };
  }
}

/** A classified failure with the action that fixes it. */
@Component({
  selector: 'ks-error-card',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, ReloginHelpComponent],
  template: `
    @let err = error();
    <section
      role="alert"
      class="rounded-[var(--ks-radius-lg)] p-3.5 space-y-2.5"
      [class.bg-[var(--ks-red-soft)]]="view().tone === 'red'"
      [class.bg-[var(--ks-amber-soft)]]="view().tone === 'amber'"
      [attr.data-error-kind]="err.kind"
    >
      <div class="flex items-start gap-2.5">
        <span class="mt-0.5 shrink-0" [class.text-[var(--ks-red)]]="view().tone === 'red'" [class.text-[var(--ks-amber)]]="view().tone === 'amber'">
          <app-icon [name]="view().icon" [size]="15" />
        </span>
        <div class="flex-1 min-w-0">
          <h3 class="text-[13px] font-semibold text-[var(--ks-text)] leading-snug" data-testid="error-title">{{ view().title }}</h3>
          @if (err.kind === 'usage_limit' && err.resets_at) {
            <div class="text-[12px] text-[#f3d29b]" data-testid="error-reset">Resets {{ countdown() }} · {{ resetAt() }}</div>
          }
          @if (view().detail && view().detail !== view().title) {
            <p class="mt-0.5 text-[12px] text-[var(--ks-text-secondary)] break-words">{{ view().detail }}</p>
          }
        </div>
      </div>

      @if (err.kind === 'auth') {
        <ks-relogin-help [relogin]="err.relogin || relogin()" [workerId]="workerId()" [accountId]="accountId()" />
      }

      @if (err.kind === 'exit' && err.stderr) {
        <div>
          <button type="button" class="ks-btn ks-btn-sm" [attr.aria-expanded]="showStderr()" (click)="showStderr.set(!showStderr())">
            <span class="transition-transform" [class.rotate-90]="showStderr()"><app-icon name="chevron-right" [size]="11" /></span>
            {{ showStderr() ? 'Hide' : 'Show' }} error output
          </button>
          @if (showStderr()) {
            <pre class="mt-1.5 max-h-60 overflow-auto rounded-[var(--ks-radius-md)] bg-black/35 p-2.5 font-mono-ide text-[11px] text-[#e9c3c3] whitespace-pre-wrap break-words" data-testid="error-stderr">{{ err.stderr }}</pre>
          }
        </div>
      }

      @if (actions()) {
        <div class="flex flex-wrap items-center gap-1.5">
          @if (err.kind === 'usage_limit' || err.kind === 'rate_limit') {
            @for (alt of alternatives(); track alt.id) {
              <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" [attr.data-switch]="alt.id" (click)="switchAccount.emit(alt)">
                <app-icon [name]="alt.agent" [size]="12" />
                Switch to {{ alt.label }}
              </button>
            }
          }
          @if (err.kind === 'timeout') {
            <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" (click)="keepWaiting.emit()">Keep waiting</button>
            <button type="button" class="ks-btn ks-btn-sm ks-btn-danger" (click)="stop.emit()">Stop</button>
          }
          @if (err.retryable && err.kind !== 'timeout') {
            <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" (click)="retry.emit()">
              <app-icon name="refresh" [size]="12" />
              Retry
            </button>
          }
        </div>
      }
    </section>
  `,
})
export class ErrorCardComponent {
  readonly error = input.required<AgentChatError>();
  readonly agent = input('');
  readonly accountLabel = input('');
  readonly workerId = input('');
  readonly accountId = input('');
  /** Fallback relogin from capabilities when the error has none. */
  readonly relogin = input<AgentRelogin | undefined>(undefined);
  /** Other logged-in accounts of the same agent. */
  readonly alternatives = input<AgentAccount[]>([]);
  readonly retry = output<void>();
  readonly switchAccount = output<AgentAccount>();
  readonly keepWaiting = output<void>();
  readonly stop = output<void>();

  readonly showStderr = signal(false);
  readonly view = computed(() => errorCardView(this.error(), { agent: this.agent(), accountLabel: this.accountLabel() }));
  readonly countdown = computed(() => formatCountdown(this.error().resets_at));
  readonly resetAt = computed(() => formatResetTime(this.error().resets_at));
  readonly actions = computed(() => {
    const e = this.error();
    return (
      e.kind === 'timeout' ||
      !!e.retryable ||
      ((e.kind === 'usage_limit' || e.kind === 'rate_limit') && this.alternatives().length > 0)
    );
  });
}

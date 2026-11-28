/** Helpers for subscription usage windows, auth state and reset times (pure, testable). */
import { AgentAccount, AgentRateLimit } from './agent-chat.models';

/** Near-limit threshold for the amber state. */
export const USAGE_WARN_PERCENT = 80;

/** Human name for a window id: five_hour -> "5-hour", seven_day -> "weekly", ... */
export function windowName(window: string | undefined): string {
  switch ((window ?? '').toLowerCase()) {
    case 'five_hour':
      return '5-hour';
    case 'seven_day':
    case 'weekly':
      return 'weekly';
    case 'seven_day_opus':
      return 'weekly Opus';
    case 'seven_day_sonnet':
      return 'weekly Sonnet';
    case 'monthly':
      return 'monthly';
    case 'daily':
      return 'daily';
    case '':
      return '';
    default: {
      // Per-model windows the CLIs add over time: seven_day_fable -> "weekly Fable",
      // five_hour_opus -> "5-hour Opus".
      const w = window!.toLowerCase();
      for (const [prefix, base] of [['seven_day_', 'weekly'], ['five_hour_', '5-hour']] as const) {
        if (w.startsWith(prefix)) {
          const model = w.slice(prefix.length).replace(/_/g, ' ');
          return `${base} ${model.charAt(0).toUpperCase()}${model.slice(1)}`;
        }
      }
      return window!.replace(/_/g, ' ');
    }
  }
}

export function isExhausted(rl: AgentRateLimit): boolean {
  return rl.state === 'exhausted' || (rl.used_percent ?? 0) >= 100;
}

/**
 * The window that matters most: an exhausted one (latest reset first, since that is when
 * the account really comes back), else the highest used percent.
 */
export function bindingWindow(usage: AgentRateLimit[] | undefined): AgentRateLimit | null {
  if (!usage || usage.length === 0) return null;
  const exhausted = usage.filter(isExhausted);
  if (exhausted.length) {
    return exhausted.reduce((a, b) => (time(b.resets_at) > time(a.resets_at) ? b : a));
  }
  return usage.reduce((a, b) => ((b.used_percent ?? 0) > (a.used_percent ?? 0) ? b : a));
}

export type AccountHealth = 'ok' | 'warning' | 'exhausted' | 'logged_out' | 'unknown';

export function accountHealth(a: AgentAccount): AccountHealth {
  if (a.auth?.state === 'logged_out') return 'logged_out';
  const w = bindingWindow(a.usage);
  if (w && isExhausted(w)) return 'exhausted';
  if (w && (w.state === 'warning' || (w.used_percent ?? 0) >= USAGE_WARN_PERCENT)) return 'warning';
  if (a.auth?.state === 'unknown') return 'unknown';
  return 'ok';
}

export function usedPercent(a: AgentAccount): number | null {
  const w = bindingWindow(a.usage);
  if (!w) return null;
  if (isExhausted(w)) return 100;
  return Math.max(0, Math.min(100, Math.round(w.used_percent ?? 0)));
}

/** Other logged-in, not-exhausted accounts of the same agent (switch suggestions). */
export function alternativeAccounts(current: AgentAccount | null | undefined, all: AgentAccount[]): AgentAccount[] {
  if (!current) return [];
  return all
    .filter((a) => a.id !== current.id && a.agent === current.agent && a.chat_supported !== false)
    .filter((a) => {
      const h = accountHealth(a);
      return h !== 'logged_out' && h !== 'exhausted';
    })
    .sort((a, b) => (usedPercent(a) ?? 0) - (usedPercent(b) ?? 0));
}

/** "3:40 PM" today, "Tue 3:40 PM" within a week, else "Nov 30, 3:40 PM" (local time). */
export function formatResetTime(iso: string | undefined, now = new Date()): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '';
  const timeStr = d.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' });
  const sameDay = d.toDateString() === now.toDateString();
  if (sameDay) return timeStr;
  const days = (d.getTime() - now.getTime()) / 86_400_000;
  if (days > 0 && days < 6.5) {
    return `${d.toLocaleDateString(undefined, { weekday: 'short' })} ${timeStr}`;
  }
  return `${d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })}, ${timeStr}`;
}

/** "in 2h 5m", "in 3d 4h", "in 12m", "soon". */
export function formatCountdown(iso: string | undefined, now = new Date()): string {
  if (!iso) return '';
  const ms = new Date(iso).getTime() - now.getTime();
  if (isNaN(ms)) return '';
  if (ms <= 60_000) return 'soon';
  const mins = Math.round(ms / 60_000);
  if (mins < 60) return `in ${mins}m`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) {
    const m = mins % 60;
    return m ? `in ${hrs}h ${m}m` : `in ${hrs}h`;
  }
  const days = Math.floor(hrs / 24);
  const h = hrs % 24;
  return h ? `in ${days}d ${h}h` : `in ${days}d`;
}

/** "Out of weekly usage until Tue 3:40 PM" (window name optional). */
export function outOfUsageText(window: string | undefined, resetsAt: string | undefined, now = new Date()): string {
  const name = windowName(window);
  const until = formatResetTime(resetsAt, now);
  const what = name ? `Out of ${name} usage` : 'Out of usage';
  return until ? `${what} until ${until}` : what;
}

function time(iso: string | undefined): number {
  const t = iso ? new Date(iso).getTime() : 0;
  return isNaN(t) ? 0 : t;
}

/** A window's display name: the vendor's bucket label, else the window's own name (capitalized). */
export function windowDisplayName(w: AgentRateLimit): string {
  if (w.label) return w.label;
  const n = windowName(w.window);
  return n ? n[0].toUpperCase() + n.slice(1) : w.window;
}

/** Whether a window runs over a billing cycle (monthly): its reset reads as a date, not a countdown. */
export function isCycleWindow(w: AgentRateLimit): boolean {
  return w.window === 'monthly' || w.window.startsWith('monthly_') || (w.window_minutes ?? 0) >= 40_000;
}

/** "resets Dec 1" for a billing-cycle window, else "resets in 2h 10m"; '' without a reset. */
export function windowResetText(w: AgentRateLimit, now = new Date()): string {
  if (!w.resets_at) return '';
  const d = new Date(w.resets_at);
  if (isNaN(d.getTime())) return '';
  if (isCycleWindow(w)) return `resets ${d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })}`;
  const c = formatCountdown(w.resets_at, now);
  return c ? `resets ${c}` : '';
}

/** "as of 9:42 PM" when the account's numbers are older than 10 minutes, else ''. */
export function usageAsOf(a: Pick<AgentAccount, 'checked_at' | 'usage'>, now = new Date()): string {
  if (!a.usage?.length) return '';
  // The oldest reading decides: a window last seen in a chat an hour ago is an hour old,
  // however recently the account was probed (SYR-BA6HV).
  const seen = a.usage.map((w) => w.observed_at).filter((x): x is string => !!x).sort()[0] ?? a.checked_at;
  if (!seen) return '';
  const t = new Date(seen);
  if (isNaN(t.getTime()) || now.getTime() - t.getTime() < 10 * 60_000) return '';
  return `as of ${t.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })}`;
}

/**
 * How the account is billed (SYR-BA6HV): its plan when known, "Subscription
 * (long-lived token)" for a CLAUDE_CODE_OAUTH_TOKEN account, "API billing" for an API key.
 */
export function billingLabel(a: Pick<AgentAccount, 'plan' | 'auth'>): string {
  if (a.plan) return a.plan[0].toUpperCase() + a.plan.slice(1);
  switch (a.auth?.method) {
    case 'setup_token':
      return 'Subscription (long-lived token)';
    case 'api_key':
      return 'API billing';
  }
  return '';
}

/** "$0.75 today · $12.40 this month" for an API-key account. */
export function spendText(a: Pick<AgentAccount, 'spend'>): string {
  const s = a.spend;
  if (!s) return '';
  const usd = (n: number) => (n >= 100 ? `$${n.toFixed(0)}` : `$${n.toFixed(2)}`);
  return `${usd(s.today_usd)} today · ${usd(s.month_usd)} this month`;
}

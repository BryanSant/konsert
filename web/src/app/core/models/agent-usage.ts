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
    default:
      return window!.replace(/_/g, ' ');
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

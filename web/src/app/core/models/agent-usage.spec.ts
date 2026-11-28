import { describe, expect, it } from 'vitest';
import { claude2Account, claudeAccount, codexLoggedOut, cursorAccount, agyAccount, copilotAccount } from '../../../testing/agent-chat.fixtures';
import { accountHealth, alternativeAccounts, billingLabel, bindingWindow, formatCountdown, formatResetTime, isCycleWindow, outOfUsageText, spendText, usageAsOf, usedPercent, windowDisplayName, windowName, windowResetText } from './agent-usage';

describe('agent usage helpers', () => {
  it('names windows', () => {
    expect(windowName('five_hour')).toBe('5-hour');
    expect(windowName('seven_day')).toBe('weekly');
    expect(windowName('seven_day_fable')).toBe('weekly Fable');
    expect(windowName('five_hour_opus')).toBe('5-hour Opus');
    expect(windowName('seven_day_opus')).toBe('weekly Opus');
    expect(windowName('monthly')).toBe('monthly');
    expect(windowName('daily')).toBe('daily');
    expect(windowName('per_minute')).toBe('per minute');
  });

  it('picks the exhausted weekly window as binding for Claude 2', () => {
    const w = bindingWindow(claude2Account().usage);
    expect(w?.window).toBe('seven_day');
    expect(accountHealth(claude2Account())).toBe('exhausted');
    expect(usedPercent(claude2Account())).toBe(100);
  });

  it('treats 100% without a state as exhausted, >= 80% as warning', () => {
    expect(accountHealth(claudeAccount({ usage: [{ window: 'five_hour', used_percent: 100 }] }))).toBe('exhausted');
    expect(accountHealth(claudeAccount({ usage: [{ window: 'five_hour', used_percent: 85 }] }))).toBe('warning');
    expect(accountHealth(claudeAccount())).toBe('ok');
    expect(accountHealth(codexLoggedOut())).toBe('logged_out');
    expect(usedPercent(cursorAccount({ usage: undefined, usage_note: 'Cursor keeps its sign-in in the macOS Keychain' }))).toBeNull();
    expect(usedPercent(cursorAccount())).toBe(82); // its fullest pool
  });

  it('suggests other usable accounts of the same agent only', () => {
    const all = [claudeAccount(), claude2Account(), codexLoggedOut(), cursorAccount()];
    expect(alternativeAccounts(claude2Account(), all).map((a) => a.id)).toEqual(['claude']);
    expect(alternativeAccounts(claudeAccount(), all)).toEqual([]); // claude2 is exhausted
    expect(alternativeAccounts(codexLoggedOut(), all)).toEqual([]);
  });

  it('formats reset times relative to now', () => {
    const now = new Date(2026, 10, 23, 12, 0); // Mon Nov 23, local noon
    const sameDay = new Date(2026, 10, 23, 15, 40).toISOString();
    const tue = new Date(2026, 10, 24, 15, 40).toISOString();
    const far = new Date(2026, 11, 10, 9, 0).toISOString();
    expect(formatResetTime(sameDay, now)).toMatch(/3:40/);
    expect(formatResetTime(sameDay, now)).not.toMatch(/Tue/);
    expect(formatResetTime(tue, now)).toMatch(/^Tue 3:40/);
    expect(formatResetTime(far, now)).toMatch(/Dec 10/);
    expect(formatCountdown(sameDay, now)).toBe('in 3h 40m');
    expect(formatCountdown(tue, now)).toBe('in 1d 3h');
    expect(formatCountdown(new Date(2026, 10, 23, 12, 0, 30).toISOString(), now)).toBe('soon');
  });

  it('writes the out-of-usage sentence with the window name', () => {
    const now = new Date(2026, 10, 23, 12, 0);
    const tue = new Date(2026, 10, 24, 15, 40).toISOString();
    expect(outOfUsageText('seven_day', tue, now)).toMatch(/^Out of weekly usage until Tue 3:40/);
    expect(outOfUsageText('five_hour', undefined, now)).toBe('Out of 5-hour usage');
  });

  it('names vendor buckets, dates billing-cycle resets and marks stale numbers (SYR-AWKXV)', () => {
    const now = new Date('2026-11-28T06:00:00Z');
    const cursor = cursorAccount();
    expect(cursor.usage!.map(windowDisplayName)).toEqual(['Plan total', 'Cursor Models', 'Other Models']);
    expect(windowResetText(cursor.usage![0], now)).toMatch(/^resets (Dec 1|Nov 30)$/);
    const agy = agyAccount();
    expect(agy.usage!.map(windowDisplayName)).toEqual(['Gemini Models · weekly', 'Gemini Models · 5-hour', 'Claude and GPT models · weekly']);
    expect(windowResetText(agy.usage![1], now)).toBe('resets in 4h 17m');
    expect(windowResetText(agy.usage![2], now)).toBe(''); // untouched bucket: no reset
    const copilot = copilotAccount();
    expect(windowDisplayName(copilot.usage![0])).toBe('Copilot Chat');
    expect(isCycleWindow(copilot.usage![0])).toBe(true);
    // An unlabelled window falls back to its own name.
    expect(windowDisplayName({ window: 'five_hour', used_percent: 10 })).toBe('5-hour');
    // The binding window of Cursor's pools is the fullest one.
    expect(bindingWindow(cursor.usage)?.label).toBe('Other Models');
    // Numbers older than 10 minutes say when they are from.
    expect(usageAsOf(copilot, new Date('2026-11-28T05:05:00Z'))).toBe('');
    expect(usageAsOf(copilot, now)).toMatch(/^as of /);
    expect(usageAsOf({ checked_at: '2026-11-28T05:00:00Z', usage: [] }, now)).toBe('');
  });
});

describe('billing (SYR-BA6HV)', () => {
  it('labels the three Claude modes; a token account is never API billing', () => {
    expect(billingLabel({ plan: 'max', auth: { state: 'ok', method: 'oauth' } })).toBe('Max');
    expect(billingLabel({ plan: '', auth: { state: 'ok', method: 'setup_token' } })).toBe('Subscription (long-lived token)');
    expect(billingLabel({ auth: { state: 'ok', method: 'api_key' } })).toBe('API billing');
    expect(billingLabel({ auth: { state: 'ok', method: 'oauth' } })).toBe('');
  });

  it('shows spend for API-key accounts', () => {
    expect(spendText({ spend: { today_usd: 0.75, month_usd: 123.4 } })).toBe('$0.75 today · $123 this month');
    expect(spendText({})).toBe('');
  });

  it('dates usage by its oldest reading, not the last probe', () => {
    const now = new Date('2026-11-27T12:00:00Z');
    const a = {
      checked_at: '2026-11-27T11:59:00Z',
      usage: [
        { window: 'five_hour', used_percent: 21, observed_at: '2026-11-27T10:30:00Z' },
        { window: 'seven_day', used_percent: 23, observed_at: '2026-11-27T11:00:00Z' },
      ],
    };
    expect(usageAsOf(a, now)).toContain('as of');
    expect(usageAsOf({ ...a, usage: a.usage.map((w) => ({ ...w, observed_at: '2026-11-27T11:58:00Z' })) }, now)).toBe('');
  });
});

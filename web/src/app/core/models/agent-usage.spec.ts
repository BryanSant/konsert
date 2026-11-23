import { describe, expect, it } from 'vitest';
import { claude2Account, claudeAccount, codexLoggedOut, cursorAccount } from '../../../testing/agent-chat.fixtures';
import {
  accountHealth,
  alternativeAccounts,
  bindingWindow,
  formatCountdown,
  formatResetTime,
  outOfUsageText,
  usedPercent,
  windowName,
} from './agent-usage';

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
    expect(usedPercent(cursorAccount())).toBeNull();
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
});

import { describe, expect, it } from 'vitest';
import { draftFromPolicy, policyChangeLines, policyFromDraft } from './machine-policy';

describe('machine policy editor', () => {
  it('round-trips a policy and keeps the role', () => {
    const p = {
      role: 'interactive' as const,
      max_sessions: 2,
      min_free_ram_mb: 2048,
      quiet_hours: [{ start: '22:00', end: '07:00', days: ['mon', 'tue'] }],
      time_zone: 'Europe/Berlin',
      allowed_work: ['chat'],
    };
    const back = policyFromDraft(draftFromPolicy(p), p);
    expect(back.policy).toEqual(p);
  });

  it('clears empty and zero limits, all days, and every work class', () => {
    const d = draftFromPolicy({ max_sessions: 2, allowed_work: ['chat'] });
    d.maxSessions = '0';
    d.allowedWork = ['chat', 'task', 'ticket'];
    d.quietHours = [
      { start: '22:00', end: '06:00', days: ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'] },
    ];
    d.timeZone = 'UTC';
    expect(policyFromDraft(d, { max_sessions: 2 }).policy).toEqual({
      quiet_hours: [{ start: '22:00', end: '06:00' }],
      time_zone: 'UTC',
    });
  });

  it('refuses bad numbers and times before asking the hub', () => {
    const d = draftFromPolicy(null);
    d.maxSessions = '1.5';
    expect(policyFromDraft(d, null).error).toContain('whole number');
    d.maxSessions = '';
    d.quietHours = [{ start: '25:00', end: '07:00', days: [] }];
    expect(policyFromDraft(d, null).error).toContain('HH:MM');
    d.quietHours = [{ start: '07:00', end: '07:00', days: [] }];
    expect(policyFromDraft(d, null).error).toContain('different');
  });

  it('defaults the time zone to the browser', () => {
    expect(draftFromPolicy(null).timeZone).toBe(Intl.DateTimeFormat().resolvedOptions().timeZone);
  });

  it('lists what an audit entry changed', () => {
    const meta = JSON.stringify({
      before: { max_sessions: 4 },
      after: { max_sessions: 2, quiet_hours: [{ start: '22:00', end: '07:00' }], time_zone: 'UTC' },
    });
    expect(policyChangeLines(meta)).toEqual([
      'Max sessions: 4 → 2',
      'Quiet hours: none → 22:00–07:00 (UTC)',
    ]);
    expect(policyChangeLines('not json')).toEqual([]);
  });
});

import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FIXTURE_WORKER_ID, claude2Account, claudeAccount, codexLoggedOut, cursorAccount } from '../../testing/agent-chat.fixtures';
import { ApiClientService } from '../core/services/api-client.service';
import { AgentAccountsStore, normalizeAccountsPayload } from '../core/state/agent-accounts.store';
import { ReloginService } from '../features/chat/relogin.service';
import { FooterUsageComponent, footerStateLine } from './footer-usage.component';

describe('footer usage', () => {
  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [{ provide: ApiClientService, useValue: { setUnauthorizedHandler: vi.fn(), listAgentAccounts: vi.fn(), refreshAgentAccounts: vi.fn() } }],
    });
  });
  afterEach(() => TestBed.resetTestingModule());

  it('describes each account state', () => {
    const now = new Date(2026, 10, 23, 12, 0);
    expect(footerStateLine(claudeAccount({ usage: [{ window: 'five_hour', used_percent: 42, resets_at: new Date(2026, 10, 23, 15, 40).toISOString() }] }), now)).toMatch(
      /^42% of 5-hour · resets 3:40/,
    );
    expect(
      footerStateLine(
        claude2Account({ usage: [{ window: 'seven_day', used_percent: 100, state: 'exhausted', resets_at: new Date(2026, 10, 24, 15, 40).toISOString() }] }),
        now,
      ),
    ).toMatch(/^Exhausted until Tue 3:40.* \(weekly\)$/);
    expect(footerStateLine(cursorAccount(), now)).toBe('Not available: Cursor exposes usage only in its TUI');
    expect(footerStateLine(codexLoggedOut(), now)).toBe('Signed out');
  });

  async function setup() {
    const store = TestBed.inject(AgentAccountsStore);
    store.applyWorkerAccounts(FIXTURE_WORKER_ID, [claudeAccount(), claude2Account(), codexLoggedOut(), cursorAccount()]);
    const fixture = TestBed.createComponent(FooterUsageComponent);
    document.body.appendChild(fixture.nativeElement);
    fixture.detectChanges();
    await fixture.whenStable();
    const el = fixture.nativeElement as HTMLElement;
    return { fixture, el, store };
  }

  it('shows per-account usage inline, exhausted Claude 2 first, and a signed-out alert', async () => {
    const { el } = await setup();
    const trigger = el.querySelector('[data-testid="footer-usage"]')!;
    const inline = Array.from(trigger.querySelectorAll('[data-account]')).map((n) => n.getAttribute('data-account'));
    expect(inline).toEqual(['claude2', 'claude']);
    expect(trigger.querySelector('[data-account="claude2"]')!.textContent).toContain('out');
    expect(trigger.querySelector('[data-account="claude"]')!.textContent).toContain('42%');
    const alert = el.querySelector('[data-testid="footer-auth-alert"]')!;
    expect(alert.getAttribute('aria-label')).toContain('Codex');
  });

  it('lists every account with its state in the popover', async () => {
    const { el, fixture } = await setup();
    (el.querySelector('[data-testid="footer-usage"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    await fixture.whenStable();
    const rows = Array.from(el.querySelectorAll('[data-account-row]'));
    expect(rows.map((r) => r.getAttribute('data-account-row'))).toEqual(['claude', 'claude2', 'codex', 'cursor']);
    const c2 = el.querySelector('[data-account-row="claude2"]')!;
    expect(c2.getAttribute('data-health')).toBe('exhausted');
    expect(c2.querySelector('[data-testid="usage-state"]')!.textContent).toMatch(/Exhausted until .* \(weekly\)/);
    // Both windows are listed for Claude 2.
    expect(c2.textContent).toContain('5-hour');
    expect(c2.textContent).toContain('Weekly');
    expect(el.querySelector('[data-account-row="cursor"]')!.textContent).toContain('Not available: Cursor exposes usage only in its TUI');
    expect(el.querySelector('[data-account-row="codex"]')!.textContent).toContain('Signed out');
  });

  it('opens the sign-in flow from the alert', async () => {
    const { el } = await setup();
    (el.querySelector('[data-testid="footer-auth-alert"]') as HTMLButtonElement).click();
    const req = TestBed.inject(ReloginService).request();
    expect(req).toMatchObject({ workerId: FIXTURE_WORKER_ID, accountId: 'codex', label: 'Codex' });
    expect(req?.relogin?.command).toBe('codex login --device-auth');
  });

  it('normalizes the accounts payload shapes', () => {
    const a = { ...claudeAccount(), worker_id: 'w1' };
    expect(Object.keys(normalizeAccountsPayload({ accounts: [a] }))).toEqual(['w1']);
    expect(normalizeAccountsPayload({ workers: [{ worker_id: 'w2', accounts: [claudeAccount()] }, { worker_id: 'w3' }] })).toMatchObject({
      w2: [{ id: 'claude' }],
      w3: [],
    });
    expect(normalizeAccountsPayload([a])['w1']).toHaveLength(1);
    expect(normalizeAccountsPayload(null)).toEqual({});
  });
});

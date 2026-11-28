import { TestBed } from '@angular/core/testing';
import { ApiClientService } from '../core/services/api-client.service';
import { PolicyReport } from '../core/models/syre.models';
import { PolicyReportComponent, ruleLabel } from './policy-report.component';

describe('PolicyReportComponent', () => {
  async function render(report: PolicyReport) {
    TestBed.configureTestingModule({
      imports: [PolicyReportComponent],
      providers: [{ provide: ApiClientService, useValue: { getPolicyReport: () => Promise.resolve(report) } }],
    });
    const fixture = TestBed.createComponent(PolicyReportComponent);
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();
    return fixture.nativeElement as HTMLElement;
  }

  it('shows blocks per machine and rule with the overrides that followed', async () => {
    const el = await render({
      since: '2026-11-21T00:00:00Z',
      rows: [
        { worker_id: 'w1', worker_name: 'bocabox', rule: 'quiet_hours', blocks: 12, overrides_requested: 1, overrides_approved: 1, overrides_denied: 0, overrides_used: 1, followed: true },
        { worker_id: 'w2', worker_name: 'daybreak', rule: 'max_sessions', blocks: 3, overrides_requested: 0, overrides_approved: 0, overrides_denied: 0, overrides_used: 0, followed: false },
      ],
      overrides: [{ id: 'o1', sid: 'o-abc', scope_kind: 'policy', rules: ['quiet_hours'], reason: 'release build', status: 'expired', created_at: '', use_count: 1 }],
      totals: { Blocks: 15, Requested: 1, Approved: 1, Denied: 0, Used: 1 },
    });
    expect(el.querySelector('[data-testid="policy-report-summary"]')?.textContent).toContain('15 pieces of work held back');
    expect(el.querySelector('[data-testid="policy-report-summary"]')?.textContent).toContain('1 approved, 0 denied, 1 used');
    const rows = el.querySelectorAll('[data-testid="policy-report-rows"] tbody tr');
    expect(rows.length).toBe(2);
    expect(rows[0].textContent).toContain('bocabox');
    expect(rows[0].textContent).toContain('Quiet hours');
    expect(rows[0].textContent).toContain('1 asked, 1 approved, 1 used');
    expect(rows[1].textContent).toContain('none');
    expect(el.querySelector('[data-testid="policy-report-overrides"]')?.textContent).toContain('release build');
  });

  it('says so when nothing was blocked', async () => {
    const el = await render({ since: '', rows: null, overrides: null, totals: { Blocks: 0, Requested: 0, Approved: 0, Denied: 0, Used: 0 } });
    expect(el.querySelector('[data-testid="policy-report-summary"]')?.textContent).toContain('No rule kept work off a machine');
    expect(el.querySelector('[data-testid="policy-report-rows"]')).toBeNull();
  });

  it('names rules plainly', () => {
    expect(ruleLabel('min_free_ram')).toBe('RAM kept free');
    expect(ruleLabel('something_new')).toBe('something_new');
  });
});

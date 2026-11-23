import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AuditEvent, AuditListResponse, AuditQuery, Task, Worker } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { AuditStore } from '../core/state/audit.store';
import { FleetStore } from '../core/state/fleet.store';
import { TabsStore } from '../core/state/tabs.store';
import { AUDIT_PAGE_SIZE, AuditLogComponent, mergeAudit } from './audit-log.component';

function ev(id: number, over: Partial<AuditEvent> = {}): AuditEvent {
  return {
    id,
    created_at: new Date(Date.UTC(2026, 10, 22, 12, 0, id)).toISOString(),
    user: 'alice@example.com',
    source: 'rest',
    action: 'task.steer',
    worker_id: 'w1',
    task_id: 't1',
    message: `event ${id}`,
    ...over,
  };
}

describe('AuditLogComponent', () => {
  let listAudit: ReturnType<typeof vi.fn>;
  let responses: AuditListResponse[];

  beforeEach(() => {
    responses = [];
    listAudit = vi.fn(async (_q: AuditQuery) => responses.shift() ?? { events: [] });
    TestBed.configureTestingModule({
      providers: [{ provide: ApiClientService, useValue: { listAudit } }],
    });
    const fleet = TestBed.inject(FleetStore);
    fleet.workers.set([
      { id: 'w1', name: 'alpha', status: 'online' } as Worker,
      { id: 'w2', name: 'beta', status: 'online' } as Worker,
    ]);
    fleet.upsertTask({ id: 't1', worker_id: 'w1', agent: 'claude', status: 'running', workdir: '/src/konsert', created_at: '' } as Task);
  });

  afterEach(() => TestBed.resetTestingModule());

  async function setup() {
    const fixture = TestBed.createComponent(AuditLogComponent);
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();
    return { fixture, cmp: fixture.componentInstance, root: fixture.nativeElement as HTMLElement };
  }

  function rows(root: HTMLElement): HTMLTableRowElement[] {
    return Array.from(root.querySelectorAll('tbody tr'));
  }

  it('renders a real table with time, user, action and host/task columns', async () => {
    responses.push({ events: [ev(2, { action: 'task.take_control', message: 'Took interactive control' }), ev(1, { worker_id: '', task_id: '', action: 'settings.update', user: 'token' })] });
    const { root } = await setup();

    expect(listAudit).toHaveBeenCalledWith(expect.objectContaining({ limit: AUDIT_PAGE_SIZE }));
    const table = root.querySelector('table')!;
    expect(table.querySelector('caption')?.textContent).toContain('audit log');
    const headers = Array.from(table.querySelectorAll('thead th')).map((th) => [th.getAttribute('scope'), th.textContent?.trim()]);
    expect(headers).toEqual([
      ['col', 'Time'],
      ['col', 'User'],
      ['col', 'Action'],
      ['col', 'Host / Task'],
    ]);

    const [first, second] = rows(root);
    expect(first.textContent).toContain('alice@example.com');
    expect(first.textContent).toContain('Take control');
    expect(first.textContent).toContain('alpha');
    expect(first.querySelector('time')?.getAttribute('datetime')).toBe(ev(2).created_at);
    const taskBtn = first.querySelector('button') as HTMLButtonElement;
    expect(taskBtn.getAttribute('aria-label')).toBe('Open task konsert · t1');
    expect(second.textContent).toContain('Settings change');
    expect(second.textContent).toContain('fleet');
    expect(root.querySelector('[role="status"]')?.textContent).toContain('2 events');
  });

  it('labels every filter and sends them to the API', async () => {
    const { fixture, cmp, root } = await setup();
    for (const id of ['audit-filter-host', 'audit-filter-action', 'audit-filter-user', 'audit-filter-task']) {
      expect(root.querySelector(`label[for="${id}"]`), id).not.toBeNull();
      expect(root.querySelector(`#${id}`), id).not.toBeNull();
    }
    expect(root.querySelector('form[role="search"]')?.getAttribute('aria-label')).toBe('Filter audit log');
    const hostOptions = Array.from(root.querySelectorAll('#audit-filter-host option')).map((o) => o.textContent?.trim());
    expect(hostOptions).toEqual(['All hosts', 'alpha', 'beta']);

    cmp.hostFilter.set('w2');
    cmp.userFilter.set(' mcp ');
    cmp.actionFilter.set('task.cancel');
    cmp.taskFilter.set('t9');
    await cmp.reload();
    expect(listAudit).toHaveBeenLastCalledWith({ worker_id: 'w2', user: 'mcp', action: 'task.cancel', task_id: 't9', limit: AUDIT_PAGE_SIZE });

    // Changing a select applies immediately.
    const select = root.querySelector('#audit-filter-action') as HTMLSelectElement;
    select.value = 'git.commit';
    select.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    await fixture.whenStable();
    expect(listAudit).toHaveBeenLastCalledWith(expect.objectContaining({ action: 'git.commit' }));

    cmp.clearFilters();
    expect(listAudit).toHaveBeenLastCalledWith({ worker_id: undefined, user: undefined, action: undefined, task_id: undefined, limit: AUDIT_PAGE_SIZE });
  });

  it('pages with the cursor and stops when there are no older events', async () => {
    responses.push({ events: [ev(5), ev(4)], next_cursor: 4 });
    responses.push({ events: [ev(3), ev(2)], next_cursor: 2 });
    responses.push({ events: [ev(1)] });
    const { fixture, cmp, root } = await setup();

    const more = () => Array.from(root.querySelectorAll('button')).find((b) => b.textContent?.includes('Load older'));
    expect(more()).toBeTruthy();
    more()!.click();
    await fixture.whenStable();
    fixture.detectChanges();
    expect(listAudit).toHaveBeenLastCalledWith(expect.objectContaining({ before_id: 4 }));
    expect(cmp.events().map((e) => e.id)).toEqual([5, 4, 3, 2]);

    await cmp.loadMore();
    fixture.detectChanges();
    expect(listAudit).toHaveBeenLastCalledWith(expect.objectContaining({ before_id: 2 }));
    expect(rows(root).length).toBe(5);
    expect(more()).toBeUndefined();
  });

  it('live-appends matching events pushed by the hub and ignores others', async () => {
    responses.push({ events: [ev(1)] });
    const { fixture, cmp, root } = await setup();
    const store = TestBed.inject(AuditStore);

    store.publish(ev(2, { action: 'task.cancel' }));
    fixture.detectChanges();
    expect(cmp.events().map((e) => e.id)).toEqual([2, 1]);
    expect(rows(root)[0].textContent).toContain('Cancel');

    cmp.hostFilter.set('w2');
    responses.push({ events: [] });
    await cmp.reload();
    store.publish(ev(3, { worker_id: 'w1' }));
    store.publish(ev(4, { worker_id: 'w2' }));
    store.publish(ev(4, { worker_id: 'w2' })); // duplicate frame
    expect(cmp.events().map((e) => e.id)).toEqual([4]);

    fixture.destroy();
    store.publish(ev(5, { worker_id: 'w2' }));
    expect(cmp.events().map((e) => e.id)).toEqual([4]);
  });

  it('opens the task tab from a task link, or filters to tasks not loaded', async () => {
    const old = ev(1, { task_id: 'old-task-xyz', task_sid: 't-0ldtk' });
    responses.push({ events: [ev(2), old] });
    const { cmp, root } = await setup();
    const tabs = TestBed.inject(TabsStore);

    (rows(root)[0].querySelector('button') as HTMLButtonElement).click();
    expect(tabs.activeTabId()).toBe('t1');

    // The task link shows the sid; filtering to an unloaded task uses the sid too.
    expect(rows(root)[1].textContent).toContain('t-0ldtk');
    cmp.openTask(old);
    expect(cmp.taskFilter()).toBe('t-0ldtk');
    expect(listAudit).toHaveBeenLastCalledWith(expect.objectContaining({ task_id: 't-0ldtk' }));
  });

  it('drops a superseded response and shows API errors', async () => {
    let resolveSlow!: (r: AuditListResponse) => void;
    listAudit.mockImplementationOnce(async () => ({ events: [] }));
    const { fixture, cmp, root } = await setup();

    listAudit.mockImplementationOnce(() => new Promise<AuditListResponse>((r) => (resolveSlow = r)));
    const slow = cmp.reload();
    listAudit.mockImplementationOnce(async () => ({ events: [ev(9)] }));
    await cmp.reload();
    resolveSlow({ events: [ev(1)] });
    await slow;
    expect(cmp.events().map((e) => e.id)).toEqual([9]);

    listAudit.mockImplementationOnce(async () => {
      throw new Error('authentication required');
    });
    await cmp.reload();
    fixture.detectChanges();
    expect(root.querySelector('[role="alert"]')?.textContent).toContain('authentication required');
  });

  it('mergeAudit dedupes by id and sorts newest first', () => {
    expect(mergeAudit([ev(3), ev(1)], [ev(2), ev(3, { message: 'new' })]).map((e) => [e.id, e.message])).toEqual([
      [3, 'new'],
      [2, 'event 2'],
      [1, 'event 1'],
    ]);
  });
});

import { provideRouter } from '@angular/router';
import { TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { Project, Ticket, TicketKind, TicketStatus } from '../../core/models/syre.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { FleetStore } from '../../core/state/fleet.store';
import { TicketStore } from '../../core/state/ticket.store';
import { NewTicketDialogComponent } from './new-ticket-dialog.component';
import { TicketPanelComponent } from './ticket-panel.component';
import { WorkModeComponent } from './work-mode.component';

const proj = (id: string, name: string, extra: Partial<Project> = {}): Project => ({
  id,
  sid: `p-${id}`,
  name,
  is_default: false,
  created_at: '2026-11-25T00:00:00Z',
  ...extra,
});

function ticket(partial: Partial<Ticket> & Pick<Ticket, 'id' | 'title'> & { kind?: TicketKind; status?: TicketStatus }): Ticket {
  return {
    sid: partial.sid ?? partial.id,
    project_id: 'proj',
    kind: 'feature',
    body: '',
    acceptance: [],
    labels: [],
    priority: 'P2',
    status: 'ready',
    created_at: '2026-11-25T00:00:00Z',
    updated_at: '2026-11-25T00:00:00Z',
    ...partial,
  };
}

describe('WorkModeComponent board', () => {
  let api: Record<string, ReturnType<typeof vi.fn>>;

  beforeEach(() => {
    localStorage.removeItem('syre.work.view');
    localStorage.removeItem('syre.work.lanes');
    api = {
      getTicket: vi.fn(async (id: string) => ({ ticket: ticket({ id, title: id }), blocked_by: [], repo_bindings: [] })),
      listTicketComments: vi.fn(async () => ({ comments: [] })),
      getTicketPacket: vi.fn(async () => ({ packet: {}, text: '', tokens: 0 })),
      updateTicket: vi.fn(),
      createTicket: vi.fn(),
      addTicketEdge: vi.fn(),
      putTicketRepo: vi.fn(),
    };
    TestBed.configureTestingModule({
      imports: [WorkModeComponent],
      providers: [provideRouter([]), { provide: ApiClientService, useValue: api }],
    });
  });

  function boardOf(tickets: Ticket[], edges: { ticket_id: string; blocked_by_id: string }[] = []) {
    const fleet = TestBed.inject(FleetStore);
    fleet.projects.set([proj('proj', 'Billing', { is_default: true })]);
    const store = TestBed.inject(TicketStore);
    store.tickets.set(tickets);
    store.edges.set(edges.map((e) => ({ ...e, created_at: '' })));
    store.loaded.set(true);
    const fixture = TestBed.createComponent(WorkModeComponent);
    fixture.detectChanges();
    return { fixture, store, el: fixture.nativeElement as HTMLElement };
  }

  it('hides backlog until asked, dims a blocked queued card, and shows the lane rollup', () => {
    const epic = ticket({ id: 'epic', sid: 'e-1', title: 'Checkout', kind: 'epic', status: 'backlog' });
    const a = ticket({ id: 'a', sid: 't-a', title: 'Schema', parent_id: 'epic', status: 'ready' });
    const b = ticket({ id: 'b', sid: 't-b', title: 'Store', parent_id: 'epic', status: 'ready' });
    const later = ticket({ id: 'c', sid: 't-c', title: 'Later', parent_id: 'epic', status: 'backlog' });
    const { fixture, el } = boardOf([epic, a, b, later], [{ ticket_id: 'b', blocked_by_id: 'a' }]);

    const headers = [...el.querySelectorAll('[data-column]')].map((n) => n.getAttribute('data-column'));
    expect(headers).toEqual(['queued', 'running', 'needs_you', 'review', 'done']);
    expect(el.querySelector('[data-lane="Expedite"]')).toBeTruthy();
    expect(el.querySelector('[data-lane="Checkout"] [data-testid="lane-rollup"]')?.textContent).toBe('0/3 · 0 running · 1 blocked');
    expect(el.querySelector('[data-card="t-c"]')).toBeNull();

    const blocked = el.querySelector('[data-card="t-b"]') as HTMLElement;
    expect(blocked.getAttribute('data-blocked')).toBe('true');
    expect(blocked.classList.contains('sy-work-card-blocked')).toBe(true);
    expect(blocked.querySelector('[data-testid="blocker-chip"]')?.textContent).toContain('blocked by t-a');
    expect(blocked.closest('[data-cell]')?.getAttribute('data-cell')).toBe('queued');

    (el.querySelector('[data-testid="show-backlog"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(el.querySelector('[data-card="t-c"]')).toBeTruthy();
    expect([...el.querySelectorAll('[data-column]')].map((n) => n.getAttribute('data-column'))).toContain('backlog');
  });

  it('switches swimlanes and clears the blocked chip when the blocker is done', () => {
    const epic = ticket({ id: 'epic', sid: 'e-1', title: 'Checkout', kind: 'epic', status: 'backlog' });
    const a = ticket({ id: 'a', sid: 't-a', title: 'Schema', parent_id: 'epic' });
    const b = ticket({ id: 'b', sid: 't-b', title: 'Store', parent_id: 'epic' });
    const { fixture, store, el } = boardOf([epic, a, b], [{ ticket_id: 'b', blocked_by_id: 'a' }]);
    expect((el.querySelector('[data-testid="lane-by"]') as HTMLSelectElement).value).toBe('epic');

    const select = el.querySelector('[data-testid="lane-by"]') as HTMLSelectElement;
    select.value = 'project';
    select.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    expect(el.querySelector('[data-lane="Billing"]')).toBeTruthy();
    expect(el.querySelector('[data-lane="Checkout"]')).toBeNull();

    store.upsert({ ...a, status: 'done', updated_at: '2026-11-25T01:00:00Z' });
    fixture.detectChanges();
    const card = el.querySelector('[data-card="t-b"]') as HTMLElement;
    expect(card.hasAttribute('data-blocked')).toBe(false);
    expect(card.querySelector('[data-testid="blocker-chip"]')).toBeNull();
    expect(el.querySelector('[data-card="t-a"]')?.closest('[data-cell]')?.getAttribute('data-cell')).toBe('done');
  });
});

describe('NewTicketDialogComponent', () => {
  let api: { createTicket: ReturnType<typeof vi.fn>; putTicketRepo: ReturnType<typeof vi.fn>; addTicketEdge: ReturnType<typeof vi.fn>; updateTicket: ReturnType<typeof vi.fn> };

  beforeEach(() => {
    api = {
      createTicket: vi.fn(async (req: Partial<Ticket> & { title: string }) =>
        ticket({ id: 'n1', sid: 't-n1', title: req.title, kind: req.kind, status: req.status, project_id: req.project_id, parent_id: req.parent_id, priority: req.priority, acceptance: req.acceptance ?? [] }),
      ),
      putTicketRepo: vi.fn(async () => ({ ticket_id: 'n1', repo_id: 'r1' })),
      addTicketEdge: vi.fn(async () => ({ ticket_id: 'n1', blocked_by_id: 'a', created_at: '' })),
      updateTicket: vi.fn(async (_id: string, patch: Partial<Ticket>) => ticket({ id: 'n1', sid: 't-n1', title: 'bound', ...patch })),
    };
    TestBed.configureTestingModule({
      imports: [NewTicketDialogComponent],
      providers: [{ provide: ApiClientService, useValue: api }],
    });
    TestBed.inject(FleetStore).projects.set([proj('p1', 'Billing', { is_default: true })]);
  });

  async function open(): Promise<{ fixture: ReturnType<TestBed['createComponent']>; el: HTMLElement }> {
    const fixture = TestBed.createComponent(NewTicketDialogComponent);
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();
    return { fixture, el: fixture.nativeElement as HTMLElement };
  }

  function setInput(el: HTMLElement, testId: string, value: string): void {
    const input = el.querySelector(`[data-testid="${testId}"]`) as HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement;
    input.value = value;
    input.dispatchEvent(new Event('input'));
    input.dispatchEvent(new Event('change'));
  }

  it('creates a ready feature with acceptance lines', async () => {
    const { fixture, el } = await open();
    setInput(el, 'nt-title', 'Land the schema');
    setInput(el, 'nt-acceptance', 'columns exist\n\nblocked chip shows');
    fixture.detectChanges();
    (el.querySelector('[data-testid="nt-submit"]') as HTMLButtonElement).click();
    await fixture.whenStable();
    expect(api.createTicket).toHaveBeenCalledWith(
      expect.objectContaining({
        project_id: 'p1',
        kind: 'feature',
        title: 'Land the schema',
        status: 'ready',
        priority: 'P2',
        acceptance: [
          { text: 'columns exist', done: false },
          { text: 'blocked chip shows', done: false },
        ],
      }),
    );
  });

  it('creates an epic in backlog and links a blocker', async () => {
    TestBed.inject(TicketStore).tickets.set([
      ticket({ id: 'epic', sid: 'e-1', title: 'Checkout', kind: 'epic', project_id: 'p1', status: 'backlog' }),
      ticket({ id: 'a', sid: 't-a', title: 'Schema', project_id: 'p1' }),
    ]);
    const { fixture, el } = await open();
    setInput(el, 'nt-kind', 'feature');
    setInput(el, 'nt-parent', 'epic');
    setInput(el, 'nt-title', 'Store');
    fixture.detectChanges();
    const box = el.querySelector('[data-testid="nt-blockers"] input') as HTMLInputElement;
    box.checked = true;
    box.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    (el.querySelector('[data-testid="nt-submit"]') as HTMLButtonElement).click();
    await fixture.whenStable();
    expect(api.createTicket).toHaveBeenCalledWith(expect.objectContaining({ kind: 'feature', parent_id: 'epic', status: 'ready', title: 'Store' }));
    expect(api.addTicketEdge).toHaveBeenCalledWith('n1', 'a');
  });

  it('binds a repo before the ticket becomes ready', async () => {
    TestBed.inject(FleetStore).repos.set([
      { id: 'r1', sid: 'r-1', project_id: 'p1', name: 'api', remote_url: 'https://example.invalid/api.git', created_at: '' },
    ]);
    const { fixture, el } = await open();
    setInput(el, 'nt-title', 'Bound');
    fixture.detectChanges();
    const box = el.querySelector('[data-testid="nt-repos"] input') as HTMLInputElement;
    box.checked = true;
    box.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    (el.querySelector('[data-testid="nt-submit"]') as HTMLButtonElement).click();
    await fixture.whenStable();
    expect(api.createTicket).toHaveBeenCalledWith(expect.objectContaining({ title: 'Bound', status: 'backlog' }));
    expect(api.putTicketRepo).toHaveBeenCalledWith('n1', 'r1');
    expect(api.updateTicket).toHaveBeenCalledWith('n1', { status: 'ready' });
  });

  it('creates an epic in the backlog with no parent', async () => {
    const { fixture, el } = await open();
    setInput(el, 'nt-kind', 'epic');
    setInput(el, 'nt-title', 'Payments');
    fixture.detectChanges();
    (el.querySelector('[data-testid="nt-submit"]') as HTMLButtonElement).click();
    await fixture.whenStable();
    expect(api.createTicket).toHaveBeenCalledWith(expect.objectContaining({ kind: 'epic', status: 'backlog', title: 'Payments', parent_id: '' }));
  });
});

describe('TicketPanelComponent', () => {
  let api: Record<string, ReturnType<typeof vi.fn>>;
  const current = ticket({ id: 'b', sid: 't-b', title: 'Store', body: 'Write the store', acceptance: [{ text: 'edges reject cycles', done: false }] });

  beforeEach(() => {
    api = {
      getTicket: vi.fn(async () => ({ ticket: current, blocked_by: [], repo_bindings: [], digest: { status: 'completed', summary: 'landed the schema' } })),
      listTicketComments: vi.fn(async () => ({ comments: [] })),
      getTicketPacket: vi.fn(async () => ({ packet: {}, text: 'packet preview', tokens: 4 })),
      updateTicket: vi.fn(async (_id: string, patch: Partial<Ticket>) => ({ ...current, ...patch, updated_at: '2026-11-25T02:00:00Z' })),
      addTicketEdge: vi.fn(async () => {
        throw new Error('cycle');
      }),
    };
    TestBed.configureTestingModule({
      imports: [TicketPanelComponent],
      providers: [provideRouter([]), { provide: ApiClientService, useValue: api }],
    });
    TestBed.inject(TicketStore).tickets.set([current, ticket({ id: 'a', sid: 't-a', title: 'Schema' })]);
  });

  async function open() {
    const fixture = TestBed.createComponent(TicketPanelComponent);
    fixture.componentRef.setInput('ticket', current);
    fixture.detectChanges();
    await fixture.whenStable();
    await new Promise((r) => setTimeout(r, 0));
    fixture.detectChanges();
    return { fixture, el: fixture.nativeElement as HTMLElement };
  }

  it('shows the ticket and moves it when the status changes', async () => {
    const { fixture, el } = await open();
    expect(api['getTicket']).toHaveBeenCalledWith('b');
    expect(fixture.componentInstance.digest()?.summary).toBe('landed the schema');
    expect(el.querySelector('[data-testid="ticket-panel"]')?.getAttribute('data-ticket')).toBe('t-b');
    expect(el.querySelector('[data-testid="ticket-body"]')?.textContent).toContain('Write the store');
    expect(el.querySelector('[data-testid="ticket-acceptance"]')?.textContent).toContain('edges reject cycles');
    expect(el.querySelector('[data-testid="ticket-digest"]')?.textContent).toContain('landed the schema');
    expect(el.querySelector('[data-testid="ticket-packet"]')?.textContent).toContain('packet preview');

    const status = el.querySelector('[data-testid="ticket-status"]') as HTMLSelectElement;
    status.value = 'done';
    status.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    (el.querySelector('[data-testid="ticket-move"]') as HTMLButtonElement).click();
    await fixture.whenStable();
    expect(api['updateTicket']).toHaveBeenCalledWith('b', { status: 'done' });
  });

  it('fetches a capped diff only after View diff is clicked', async () => {
    api['inspectTicket'] = vi.fn(async () => ({
      text: 'diff body',
      truncated: true,
      next_offset: 200,
      note: 'truncated; call inspect again with offset=200',
    }));
    const { fixture, el } = await open();
    expect(api['inspectTicket']).not.toHaveBeenCalled();
    expect(el.querySelector('[data-testid="ticket-diff"]')).toBeNull();

    (el.querySelector('[data-testid="view-diff"]') as HTMLButtonElement).click();
    await fixture.whenStable();
    fixture.detectChanges();
    expect(api['inspectTicket']).toHaveBeenCalledWith('b', { kind: 'diff', repo: undefined, max_lines: 200, offset: 0 });
    expect(el.querySelector('[data-testid="ticket-diff"]')?.textContent).toContain('diff body');
    expect(el.querySelector('[data-testid="diff-note"]')?.textContent).toContain('offset=200');

    (el.querySelector('[data-testid="diff-more"]') as HTMLButtonElement).click();
    await fixture.whenStable();
    fixture.detectChanges();
    expect(api['inspectTicket']).toHaveBeenLastCalledWith('b', { kind: 'diff', repo: undefined, max_lines: 200, offset: 200 });
    expect(el.querySelector('[data-testid="ticket-diff"]')?.textContent).toContain('diff bodydiff body');
  });

  it('shows a cycle error when adding a blocker fails', async () => {
    const { fixture, el } = await open();
    const pick = el.querySelector('[data-testid="add-blocker"]') as HTMLSelectElement;
    pick.value = 'a';
    pick.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    (el.querySelector('[data-testid="add-blocker-btn"]') as HTMLButtonElement).click();
    await fixture.whenStable();
    fixture.detectChanges();
    expect(el.querySelector('[data-testid="blocker-error"]')?.textContent).toContain('cycle');
  });
});

import { Component, signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FIXTURE_WORKER_ID, claude2Account, claudeAccount, codexLoggedOut, cursorAccount } from '../../testing/agent-chat.fixtures';
import { Worker } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { AgentAccountsStore } from '../core/state/agent-accounts.store';
import { FleetStore } from '../core/state/fleet.store';
import { resetTooltipWarmState } from '../shared/tooltip.directive';
import { SessionDispatchModalComponent, SessionDispatchRequest } from './session-dispatch-modal.component';

const worker = (id: string, name: string): Worker => ({
  id,
  name,
  host_group: 'default',
  os: 'darwin',
  arch: 'arm64',
  isolation: 'host',
  permission_mode: 'full_access',
  agents: ['claude', 'shell'],
  tags: {},
  max_concurrent: 4,
  status: 'online',
  last_seen_at: '',
});

@Component({
  standalone: true,
  imports: [SessionDispatchModalComponent],
  template: `<app-session-dispatch-modal [request]="request()" (close)="closed = closed + 1" />`,
})
class Host {
  readonly request = signal<SessionDispatchRequest>({ kind: 'chat', worker: null });
  closed = 0;
}

describe('SessionDispatchModalComponent (shared composer)', () => {
  let createSession: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    resetTooltipWarmState();
    createSession = vi.fn(async () => ({ task_id: 'task-1', status: 'queued' }));
    TestBed.configureTestingModule({
      providers: [
        {
          provide: ApiClientService,
          useValue: { createSession, setUnauthorizedHandler: vi.fn(), uploadAttachment: vi.fn(), listDirectory: vi.fn(async () => ({ entries: [] })) },
        },
      ],
    });
    const fleet = TestBed.inject(FleetStore);
    fleet.upsertWorker(worker(FIXTURE_WORKER_ID, 'studio'));
    fleet.upsertWorker(worker('w-empty', 'oldbox'));
    vi.spyOn(fleet, 'refresh').mockResolvedValue();
    vi.spyOn(fleet, 'loadSettings').mockResolvedValue();
    const accounts = TestBed.inject(AgentAccountsStore);
    accounts.byWorker.set({ [FIXTURE_WORKER_ID]: [claudeAccount(), claude2Account(), codexLoggedOut(), cursorAccount()] });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    TestBed.resetTestingModule();
    document.querySelectorAll('.ks-tooltip').forEach((n) => n.remove());
  });

  async function setup(req: SessionDispatchRequest = { kind: 'chat', worker: worker(FIXTURE_WORKER_ID, 'studio') }) {
    const fixture = TestBed.createComponent(Host);
    fixture.componentInstance.request.set(req);
    document.body.appendChild(fixture.nativeElement);
    fixture.detectChanges();
    await fixture.whenStable();
    const el = fixture.nativeElement as HTMLElement;
    const render = async () => {
      fixture.detectChanges();
      await fixture.whenStable();
      // Let the async createSession / refresh chain settle.
      await new Promise((r) => setTimeout(r, 0));
      fixture.detectChanges();
    };
    const $ = <T extends Element = HTMLElement>(sel: string) => el.querySelector(sel) as T;
    return { fixture, el, render, $, host: fixture.componentInstance };
  }

  it('reuses the composer pickers with the machine’s real models, efforts and approval modes', async () => {
    const { $, el, render } = await setup();
    expect($('[data-testid="dispatch-composer"]')).toBeTruthy();
    expect($('[data-testid="account-chip"]').textContent).toContain('Claude');
    expect($('[data-testid="model-trigger"]').textContent?.trim()).toBe('Sonnet 5.5 Med');
    expect($('[data-testid="approval-chip"]').textContent).toContain('Ask for approval');
    expect($('[data-testid="context-machine"]').textContent).toContain('studio');
    // No attachments (the dispatch API has none) and no legacy selects.
    expect($('[aria-label="Attach file"]')).toBeNull();
    expect(el.querySelector('select')).toBeNull();

    $<HTMLButtonElement>('[data-testid="approval-chip"]').click();
    await render();
    (el.querySelector('[data-mode="acceptEdits"]') as HTMLButtonElement).click();
    await render();
    $<HTMLButtonElement>('[data-testid="model-trigger"]').click();
    await render();
    (el.querySelector('[data-effort="high"]') as HTMLButtonElement).click();
    await render();

    const ta = $<HTMLTextAreaElement>('textarea');
    ta.value = 'Fix the flaky test';
    ta.dispatchEvent(new Event('input'));
    $<HTMLInputElement>('[data-testid="dispatch-branch"]').value = 'feat/x';
    $<HTMLInputElement>('[data-testid="dispatch-branch"]').dispatchEvent(new Event('input'));
    await render();
    $<HTMLButtonElement>('[data-testid="dispatch-submit"]').click();
    await render();

    expect(createSession).toHaveBeenCalledTimes(1);
    expect(createSession.mock.calls[0][0]).toMatchObject({
      worker_selector: { worker_id: FIXTURE_WORKER_ID },
      session_kind: 'chat',
      agent: 'claude',
      account_id: 'claude',
      model: 'sonnet',
      effort: 'high',
      approval_mode: 'acceptEdits',
      prompt: 'Fix the flaky test',
      repo_path: '~',
      branch: 'feat/x',
      use_worktree: true,
      mode: 'interactive',
    });
  });

  it('starts an interactive agent session with an empty prompt from the send button', async () => {
    const { $, render, host } = await setup();
    const send = $<HTMLButtonElement>('[data-testid="send-button"]');
    expect(send.getAttribute('aria-label')).toBe('Start session');
    expect(send.disabled).toBe(false);
    send.click();
    await render();
    expect(createSession.mock.calls[0][0]).toMatchObject({ prompt: '', account_id: 'claude', use_worktree: false });
    expect(host.closed).toBe(1);
  });

  it('switching account sends that account’s agent and defaults', async () => {
    const { $, el, render } = await setup();
    $<HTMLButtonElement>('[data-testid="account-chip"]').click();
    await render();
    (el.querySelector('[data-account="cursor"]') as HTMLButtonElement).click();
    await render();
    $<HTMLButtonElement>('[data-testid="dispatch-submit"]').click();
    await render();
    expect(createSession.mock.calls[0][0]).toMatchObject({ agent: 'cursor', account_id: 'cursor', model: 'auto', approval_mode: 'ask' });
    expect(createSession.mock.calls[0][0].effort).toBeUndefined();
  });

  it('keeps terminal (shell) sessions working with the context bar', async () => {
    const { $, render } = await setup({ kind: 'terminal', worker: worker(FIXTURE_WORKER_ID, 'studio') });
    expect($('[data-testid="dispatch-composer"]')).toBeNull();
    expect($('[data-testid="context-machine"]').textContent).toContain('studio');
    expect($('[data-testid="dispatch-submit"]').textContent?.trim()).toBe('Open terminal');
    $<HTMLButtonElement>('[data-testid="dispatch-submit"]').click();
    await render();
    const req = createSession.mock.calls[0][0];
    expect(req).toMatchObject({ session_kind: 'terminal', agent: 'shell', prompt: '', repo_path: '~' });
    expect(req.account_id).toBeUndefined();
    expect(req.approval_mode).toBeUndefined();
  });

  it('falls back to an agent select on machines without reported accounts', async () => {
    const { $, render } = await setup({ kind: 'chat', worker: worker('w-empty', 'oldbox') });
    expect($('[data-testid="dispatch-no-accounts"]').textContent).toContain('oldbox');
    expect($('[data-testid="dispatch-composer"]')).toBeNull();
    $<HTMLButtonElement>('[data-testid="dispatch-submit"]').click();
    await render();
    expect(createSession.mock.calls[0][0]).toMatchObject({ agent: 'claude', session_kind: 'chat', worker_selector: { worker_id: 'w-empty' } });
    expect(createSession.mock.calls[0][0].account_id).toBeUndefined();
  });

  it('shows API errors and stays open', async () => {
    createSession.mockRejectedValueOnce(new Error('No machine online'));
    const { $, render, host } = await setup();
    $<HTMLButtonElement>('[data-testid="dispatch-submit"]').click();
    await render();
    expect($('[role="alert"]').textContent).toContain('No machine online');
    expect(host.closed).toBe(0);
  });
});

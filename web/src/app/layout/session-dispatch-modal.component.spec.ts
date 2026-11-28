import { Component, signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FIXTURE_WORKER_ID, claude2Account, claudeAccount, codexLoggedOut, cursorAccount } from '../../testing/agent-chat.fixtures';
import { Worker } from '../core/models/syre.models';
import { ApiClientService } from '../core/services/api-client.service';
import { AgentAccountsStore } from '../core/state/agent-accounts.store';
import { FleetStore } from '../core/state/fleet.store';
import { resetTooltipWarmState } from '../shared/tooltip.directive';
import { SessionDispatchModalComponent, SessionDispatchRequest } from './session-dispatch-modal.component';

const worker = (id: string, name: string): Worker => ({
  id,
  sid: id,
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

import { TakeControlService } from '../modes/take-control.service';
describe('SessionDispatchModalComponent (shared composer)', () => {
  let createSession: ReturnType<typeof vi.fn>;
  let setTaskControl: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    resetTooltipWarmState();
    createSession = vi.fn(async () => ({ task_id: 'task-1', status: 'queued' }));
    setTaskControl = vi.fn(async () => ({ ok: true }));
    TestBed.configureTestingModule({
      providers: [
        {
          provide: ApiClientService,
          useValue: { createSession, setTaskControl, loadHolds: vi.fn(), listHolds: vi.fn(async () => ({ holds: [] })), setUnauthorizedHandler: vi.fn(), uploadAttachment: vi.fn(), listDirectory: vi.fn(async () => ({ entries: [] })) },
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
    document.querySelectorAll('.sy-tooltip').forEach((n) => n.remove());
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

  it('starts an interactive agent session with an empty prompt from the single primary button', async () => {
    const { $, render, host } = await setup();
    // One primary action: the composer's own send button is hidden inside the dialog.
    expect($('[data-testid="send-button"]')).toBeNull();
    const send = $<HTMLButtonElement>('[data-testid="dispatch-submit"]');
    expect(send.textContent?.trim()).toBe('Start session');
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
    expect(req.shell).toBeUndefined();
    // The user's own terminal: they hold control from the start (audited like the button).
    expect(setTaskControl).toHaveBeenCalledWith('task-1', true);
    expect(TestBed.inject(TakeControlService).has('task-1')).toBe(true);
  });

  it('opens an agent session read-only: no control taken on its behalf', async () => {
    const { $, render } = await setup();
    $<HTMLButtonElement>('[data-testid="dispatch-submit"]').click();
    await render();
    expect(createSession).toHaveBeenCalledTimes(1);
    expect(setTaskControl).not.toHaveBeenCalled();
  });

  it('offers a terminal shell picker when the machine reports available shells', async () => {
    const fleet = TestBed.inject(FleetStore);
    const winWorker: Worker = {
      ...worker('w-win', 'winbox'),
      os: 'windows',
      default_shell: 'git-bash',
      facts: {
        cpu_cores: 8,
        ram_total_mb: 16384,
        swap_total_mb: 0,
        swap_used_mb: 0,
        disk_total_gb: 500,
        disk_free_gb: 200,
        tmp_is_tmpfs: false,
        is_wsl: false,
        is_container: false,
        is_mac_container: false,
        is_vm: false,
        shells: [
          { id: 'git-bash', label: 'Git Bash', path: 'C:\\Program Files\\Git\\bin\\bash.exe' },
          { id: 'pwsh', label: 'PowerShell 7', path: 'C:\\Program Files\\PowerShell\\7\\pwsh.exe' },
          { id: 'powershell', label: 'PowerShell 5.1', path: 'C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe' },
        ],
      },
    };
    fleet.upsertWorker(winWorker);

    const { $, render } = await setup({ kind: 'terminal', worker: winWorker });
    const select = $<HTMLSelectElement>('[data-testid="dispatch-shell"]');
    expect(select).toBeTruthy();
    expect(select.options[0].textContent?.trim()).toBe('Machine default (Git Bash)');
    expect(Array.from(select.options).map((o) => o.value)).toEqual(['', 'git-bash', 'pwsh', 'powershell']);

    select.value = 'pwsh';
    select.dispatchEvent(new Event('change'));
    await render();
    $<HTMLButtonElement>('[data-testid="dispatch-submit"]').click();
    await render();
    expect(createSession.mock.calls[0][0]).toMatchObject({
      session_kind: 'terminal',
      agent: 'shell',
      shell: 'pwsh',
      worker_selector: { worker_id: 'w-win' },
    });
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

import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Worker } from '../../core/models/konsert.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { ControlStore } from '../../core/state/control.store';
import { FleetStore } from '../../core/state/fleet.store';
import { HoldsRequestsComponent } from './holds-requests.component';
import { PauseEverythingComponent, PausedBannerComponent } from './pause-everything.component';
import { SessionControlsComponent, sessionActions } from './session-controls.component';
import { YourActionsComponent } from './your-actions.component';

async function settle(fixture: {
  detectChanges(): void;
  whenStable(): Promise<unknown>;
}): Promise<void> {
  fixture.detectChanges();
  await fixture.whenStable();
  await new Promise((r) => setTimeout(r, 0));
  fixture.detectChanges();
}

function byTestId(root: HTMLElement, id: string): HTMLElement | null {
  return root.querySelector(`[data-testid="${id}"]`);
}

describe('sessionActions', () => {
  it('offers the right interventions per status', () => {
    expect(sessionActions('task', 'running')).toEqual({
      suspend: true,
      resume: false,
      cancel: true,
      veto: true,
    });
    expect(sessionActions('task', 'suspended')).toEqual({
      suspend: false,
      resume: true,
      cancel: true,
      veto: true,
    });
    expect(sessionActions('task', 'completed')).toEqual({
      suspend: false,
      resume: false,
      cancel: false,
      veto: false,
    });
    expect(sessionActions('chat', 'working')).toEqual({
      suspend: true,
      resume: false,
      cancel: false,
      veto: true,
    });
    expect(sessionActions('chat', 'suspended')).toEqual({
      suspend: false,
      resume: true,
      cancel: false,
      veto: true,
    });
    expect(sessionActions('chat', 'done')).toEqual({
      suspend: false,
      resume: false,
      cancel: false,
      veto: false,
    });
  });
});

describe('control components', () => {
  let api: Record<string, ReturnType<typeof vi.fn>>;

  beforeEach(() => {
    api = {
      suspendTask: vi.fn(async () => ({ ok: true })),
      resumeChat: vi.fn(async () => ({ ok: true })),
      vetoTask: vi.fn(async () => ({ ok: true })),
      cancelTask: vi.fn(async () => ({ ok: true })),
      pauseFleet: vi.fn(async () => ({
        paused: true,
        paused_by: 'alice',
        paused_at: new Date().toISOString(),
      })),
      resumeFleet: vi.fn(async () => ({ paused: false })),
      releaseHold: vi.fn(async (id: string) => ({ id, released_at: new Date().toISOString() })),
      resolveRequest: vi.fn(async (id: string, status: string) => ({ id, status })),
    };
    TestBed.configureTestingModule({ providers: [{ provide: ApiClientService, useValue: api }] });
    TestBed.inject(FleetStore).workers.set([
      { id: 'w1', sid: 'm-aaaaa', name: 'host-1', status: 'online' } as Worker,
    ]);
  });

  afterEach(() => TestBed.resetTestingModule());

  it('suspends a running task and vetoes it with a reason and note', async () => {
    const fixture = TestBed.createComponent(SessionControlsComponent);
    fixture.componentRef.setInput('kind', 'task');
    fixture.componentRef.setInput('sessionId', 't1');
    fixture.componentRef.setInput('status', 'running');
    await settle(fixture);
    const root = fixture.nativeElement as HTMLElement;
    expect(byTestId(root, 'session-resume')).toBeNull();

    byTestId(root, 'session-suspend')!.click();
    await settle(fixture);
    expect(api['suspendTask']).toHaveBeenCalledWith('t1');

    byTestId(root, 'session-veto')!.click();
    await settle(fixture);
    const submit = byTestId(root, 'veto-submit') as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    byTestId(root, 'veto-reason-wrong_machine')!.click();
    const note = byTestId(root, 'veto-note') as HTMLTextAreaElement;
    note.value = 'use the linux box';
    note.dispatchEvent(new Event('input'));
    await settle(fixture);
    expect(submit.disabled).toBe(false);
    submit.click();
    await settle(fixture);
    expect(api['vetoTask']).toHaveBeenCalledWith('t1', 'wrong_machine', 'use the linux box');
    expect(byTestId(root, 'veto-submit')).toBeNull();
  });

  it('resumes a suspended chat', async () => {
    const fixture = TestBed.createComponent(SessionControlsComponent);
    fixture.componentRef.setInput('kind', 'chat');
    fixture.componentRef.setInput('sessionId', 'c1');
    fixture.componentRef.setInput('status', 'suspended');
    await settle(fixture);
    const root = fixture.nativeElement as HTMLElement;
    expect(byTestId(root, 'session-suspend')).toBeNull();
    expect(byTestId(root, 'session-cancel')).toBeNull();
    byTestId(root, 'session-resume')!.click();
    await settle(fixture);
    expect(api['resumeChat']).toHaveBeenCalledWith('c1');
  });

  it('pauses everything, shows the banner and resumes', async () => {
    const button = TestBed.createComponent(PauseEverythingComponent);
    const banner = TestBed.createComponent(PausedBannerComponent);
    await settle(button);
    await settle(banner);
    const bRoot = banner.nativeElement as HTMLElement;
    expect(byTestId(bRoot, 'paused-banner')).toBeNull();

    byTestId(button.nativeElement, 'pause-everything')!.click();
    await settle(button);
    await settle(banner);
    expect(api['pauseFleet']).toHaveBeenCalled();
    expect(byTestId(bRoot, 'paused-banner')?.textContent).toContain('by alice');
    expect(byTestId(button.nativeElement, 'pause-everything')).toBeNull();

    byTestId(button.nativeElement, 'resume-everything')!.click();
    await settle(button);
    await settle(banner);
    expect(api['resumeFleet']).toHaveBeenCalled();
    expect(byTestId(bRoot, 'paused-banner')).toBeNull();
  });

  it('lists holds and requests for a machine with release / mark done', async () => {
    const control = TestBed.inject(ControlStore);
    control.applyHold({
      id: 'h1',
      sid: 'h-abcde',
      scope_kind: 'resource',
      worker_id: 'w1',
      resource: { kind: 'service', name: 'nginx' },
      summary: 'You stopped nginx on host-1',
      cause: { actor: 'user', source: 'terminal', confidence: 'high' },
      created_at: new Date().toISOString(),
    });
    control.applyHold({
      id: 'h2',
      sid: 'h-other',
      scope_kind: 'machine',
      worker_id: 'w2',
      summary: 'other',
      cause: { actor: 'user', confidence: 'certain' },
      created_at: new Date().toISOString(),
    });
    control.applyRequest({
      id: 'r1',
      sid: 'rq-abcde',
      worker_id: 'w1',
      description: 'Restart nginx; I have no sudo',
      status: 'open',
      created_at: new Date().toISOString(),
    });

    const fixture = TestBed.createComponent(HoldsRequestsComponent);
    fixture.componentRef.setInput('workerId', 'w1');
    await settle(fixture);
    const root = fixture.nativeElement as HTMLElement;
    expect(root.querySelectorAll('[data-hold]')).toHaveLength(1);
    expect(byTestId(root, 'holds-list')?.textContent).toContain('service nginx on host-1');
    expect(byTestId(root, 'requests-list')?.textContent).toContain('Restart nginx');

    byTestId(root, 'hold-release-h-abcde')!.click();
    byTestId(root, 'request-done-rq-abcde')!.click();
    await settle(fixture);
    expect(api['releaseHold']).toHaveBeenCalledWith('h1', undefined);
    expect(api['resolveRequest']).toHaveBeenCalledWith('r1', 'done', undefined);
    expect(root.querySelectorAll('[data-hold]')).toHaveLength(0);
    expect(root.querySelectorAll('[data-request]')).toHaveLength(0);
  });

  it('renders the your-actions timeline with actor, source and confidence', async () => {
    const control = TestBed.inject(ControlStore);
    control.applyEvent({
      id: 7,
      created_at: new Date().toISOString(),
      kind: 'action',
      action: 'command',
      cause: { actor: 'user', source: 'terminal', confidence: 'high' },
      worker_id: 'w1',
      summary: 'You stopped nginx on host-1 via a Konsert terminal',
    });
    const fixture = TestBed.createComponent(YourActionsComponent);
    await settle(fixture);
    const text = (fixture.nativeElement as HTMLElement).textContent ?? '';
    expect(text).toContain('You stopped nginx');
    expect(text).toContain('You via a Konsert terminal');
    expect(text).toContain('high');
    expect(text).toContain('host-1');
  });
});

import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ShellPolicySettings } from '../core/models/activity.models';
import { ApiClientService } from '../core/services/api-client.service';
import { FleetStore } from '../core/state/fleet.store';
import { ShellPolicySettingsComponent, shellModeLabel } from './shell-policy-settings.component';

describe('ShellPolicySettingsComponent', () => {
  let api: Record<string, ReturnType<typeof vi.fn>>;

  beforeEach(() => {
    api = {
      getShellPolicy: vi.fn(async (): Promise<ShellPolicySettings> => ({ mode: 'remind', idle_minutes: 30, projects: {} })),
      setShellPolicy: vi.fn(async (p: ShellPolicySettings) => p),
    };
    TestBed.configureTestingModule({ providers: [{ provide: ApiClientService, useValue: api }] });
  });

  afterEach(() => TestBed.resetTestingModule());

  async function setup() {
    TestBed.inject(FleetStore).projects.set([
      { id: 'p1', sid: 'p-1', workspace_id: 'w', name: 'webapp', is_default: false, created_at: '' },
    ] as never);
    const fixture = TestBed.createComponent(ShellPolicySettingsComponent);
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();
    return { fixture, root: fixture.nativeElement as HTMLElement };
  }

  it('shows the default mode in plain words and saves a global mode, idle period and a project override', async () => {
    const { fixture, root } = await setup();
    expect(root.querySelector('[data-testid="shells-hint"]')?.textContent).toContain('After 30 min idle, one Needs-you item per session');
    const mode = root.querySelector<HTMLSelectElement>('[data-testid="shells-mode"]')!;
    mode.value = 'stop';
    mode.dispatchEvent(new Event('change'));
    const idle = root.querySelector<HTMLInputElement>('[data-testid="shells-idle"]')!;
    idle.value = '45';
    idle.dispatchEvent(new Event('input'));
    fixture.detectChanges();
    expect(root.querySelector('[data-testid="shells-hint"]')?.textContent).toContain('idle for 45 min');
    expect(root.querySelector('[data-testid="shells-hint"]')?.textContent).toContain('Never when it is pinned with Keep');
    const proj = root.querySelector<HTMLSelectElement>('[data-testid="shells-project-p-1"]')!;
    proj.value = 'ask';
    proj.dispatchEvent(new Event('change'));
    root.querySelector<HTMLButtonElement>('[data-testid="shells-save"]')!.click();
    await fixture.whenStable();
    expect(api['setShellPolicy']).toHaveBeenCalledWith({ mode: 'stop', idle_minutes: 45, projects: { p1: 'ask' } });
  });

  it('labels modes', () => {
    expect(shellModeLabel('remind')).toBe('Remind me');
    expect(shellModeLabel('ask')).toBe('Ask the agent');
    expect(shellModeLabel('stop')).toBe('Stop after idle');
  });
});

import { TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { AutopilotProjectState, AutopilotState, Project } from '../../core/models/syre.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { ControlStore } from '../../core/state/control.store';
import { FleetStore } from '../../core/state/fleet.store';
import { AutopilotControlComponent } from './autopilot-control.component';

const proj = (id: string, name: string, extra: Partial<Project> = {}): Project => ({
  id,
  sid: `p-${id}`,
  name,
  is_default: false,
  created_at: '2026-11-25T00:00:00Z',
  ...extra,
});

function makeProjectState(id: string, name: string, overrides: Partial<AutopilotProjectState> = {}): AutopilotProjectState {
  return {
    project_id: id,
    project_sid: `p-${id}`,
    project_name: name,
    play: false,
    autopilot: false,
    autopilot_paused: false,
    global: false,
    fleet_paused: false,
    state: 'paused',
    counts: { dispatchable: 0, running: 0, needs_you: 0, review: 0 },
    ...overrides,
  };
}

describe('AutopilotControlComponent', () => {
  let fleet: FleetStore;
  let control: ControlStore;
  let api: {
    setProjectAutopilot: ReturnType<typeof vi.fn>;
    updateSettings: ReturnType<typeof vi.fn>;
    getAutopilot: ReturnType<typeof vi.fn>;
  };

  beforeEach(() => {
    api = {
      setProjectAutopilot: vi.fn(),
      updateSettings: vi.fn(),
      getAutopilot: vi.fn(),
    };
    TestBed.configureTestingModule({
      imports: [AutopilotControlComponent],
      providers: [{ provide: ApiClientService, useValue: api }],
    });
    fleet = TestBed.inject(FleetStore);
    control = TestBed.inject(ControlStore);
    fleet.projects.set([proj('p1', 'Billing'), proj('p2', 'Search')]);
    fleet.applyAutopilot({
      global: false,
      fleet_paused: false,
      state: 'paused',
      counts: { dispatchable: 3, running: 1, needs_you: 1, review: 0 },
      projects: [
        makeProjectState('p1', 'Billing', {
          counts: { dispatchable: 2, running: 1, needs_you: 0, review: 0 },
        }),
        makeProjectState('p2', 'Search', {
          state: 'finished',
          counts: { dispatchable: 0, running: 0, needs_you: 0, review: 2 },
        }),
      ],
    });
  });

  it('shows all-projects and per-project states and counts, live-updated via FleetStore', () => {
    const fixture = TestBed.createComponent(AutopilotControlComponent);
    fixture.detectChanges();
    const el: HTMLElement = fixture.nativeElement;

    const stateEl = el.querySelector('[data-testid="autopilot-state"]')!;
    expect(stateEl.getAttribute('data-state')).toBe('paused');
    expect(el.querySelector('[data-testid="autopilot-count-dispatchable"]')?.textContent).toContain('3 dispatchable');
    expect(el.querySelector('[data-testid="autopilot-count-running"]')?.textContent).toContain('1 running');
    expect(el.querySelector('[data-testid="autopilot-count-needs-you"]')?.textContent).toContain('1 needs you');

    // Switch scope to Billing (p1).
    const select = el.querySelector<HTMLSelectElement>('[data-testid="autopilot-scope"]')!;
    select.value = 'p1';
    select.dispatchEvent(new Event('change'));
    fixture.detectChanges();

    expect(el.querySelector('[data-testid="autopilot-count-dispatchable"]')?.textContent).toContain('2 dispatchable');
    expect(el.querySelector('[data-testid="autopilot-count-running"]')?.textContent).toContain('1 running');

    // Live fleet update transitions Billing to finished.
    fleet.applyAutopilot({
      global: false,
      fleet_paused: false,
      state: 'finished',
      counts: { dispatchable: 0, running: 0, needs_you: 0, review: 3 },
      projects: [
        makeProjectState('p1', 'Billing', {
          state: 'finished',
          counts: { dispatchable: 0, running: 0, needs_you: 0, review: 1 },
        }),
        makeProjectState('p2', 'Search', {
          state: 'finished',
          counts: { dispatchable: 0, running: 0, needs_you: 0, review: 2 },
        }),
      ],
    });
    fixture.detectChanges();
    expect(stateEl.getAttribute('data-state')).toBe('finished');
    expect(stateEl.textContent?.trim()).toBe('Finished');
    expect(el.querySelector('[data-testid="autopilot-count-running"]')?.textContent).toContain('0 running');
  });

  it('toggles per-project Play/Pause via REST and global Play/Pause via settings', async () => {
    const fixture = TestBed.createComponent(AutopilotControlComponent);
    fixture.detectChanges();
    const el: HTMLElement = fixture.nativeElement;

    // Toggle global Play.
    const nextGlobal: AutopilotState = {
      global: true,
      fleet_paused: false,
      state: 'playing',
      counts: { dispatchable: 3, running: 1, needs_you: 1, review: 0 },
      projects: [makeProjectState('p1', 'Billing', { play: true, global: true, state: 'playing' })],
    };
    api.updateSettings.mockResolvedValue({ ...fleet.settings(), autopilot_global: true });
    api.getAutopilot.mockResolvedValue(nextGlobal);

    const toggleBtn = el.querySelector<HTMLButtonElement>('[data-testid="autopilot-toggle"]')!;
    toggleBtn.click();
    await fixture.whenStable();
    fixture.detectChanges();

    expect(api.updateSettings).toHaveBeenCalledWith({ autopilot_global: true });
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('true');
    expect(el.querySelector('[data-testid="autopilot-state"]')?.getAttribute('data-state')).toBe('playing');

    // Switch to project p1 and press Pause.
    const select = el.querySelector<HTMLSelectElement>('[data-testid="autopilot-scope"]')!;
    select.value = 'p1';
    select.dispatchEvent(new Event('change'));
    fixture.detectChanges();

    api.setProjectAutopilot.mockResolvedValue(
      makeProjectState('p1', 'Billing', { play: false, autopilot: false, autopilot_paused: true, global: true, state: 'paused' }),
    );
    toggleBtn.click();
    await fixture.whenStable();
    fixture.detectChanges();

    expect(api.setProjectAutopilot).toHaveBeenCalledWith('p1', false);
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('false');
  });

  it('disables toggle and shows Paused (fleet) when Pause everything is active', () => {
    control.applyPause({ paused: true, paused_by: 'alice' });
    const fixture = TestBed.createComponent(AutopilotControlComponent);
    fixture.detectChanges();
    const el: HTMLElement = fixture.nativeElement;

    const toggleBtn = el.querySelector<HTMLButtonElement>('[data-testid="autopilot-toggle"]')!;
    expect(toggleBtn.disabled).toBe(true);
    expect(el.querySelector('[data-testid="autopilot-state"]')?.textContent?.trim()).toBe('Paused (fleet)');
  });
});

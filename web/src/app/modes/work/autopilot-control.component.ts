import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { AutopilotCounts, AutopilotDisplayState } from '../../core/models/syre.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { ControlStore } from '../../core/state/control.store';
import { FleetStore } from '../../core/state/fleet.store';
import { IconComponent } from '../../shared/icon.component';
import { TooltipDirective } from '../../shared/tooltip.directive';

const STATE_LABELS: Record<AutopilotDisplayState, string> = {
  playing: 'Playing',
  paused: 'Paused',
  finished: 'Finished',
};

/**
 * Autopilot Play/Pause control in the Work mode header.
 * Allows playing/pausing a selected project or all projects (global autopilot_global),
 * displaying live state (playing / paused / finished) and ticket counts (dispatchable,
 * running, needs you). Distinct from the emergency Pause everything control.
 */
@Component({
  selector: 'sy-autopilot-control',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective],
  host: { class: 'inline-flex flex-wrap items-center gap-2' },
  template: `
    <div
      class="inline-flex flex-wrap items-center gap-1.5 rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-1)] border border-[var(--sy-hairline)] px-2 py-0.5 text-[12px]"
      data-testid="autopilot-control"
    >
      <select
        class="bg-transparent text-[var(--sy-text)] text-[12px] font-medium pr-1 py-0.5 border-0 focus:outline-none cursor-pointer"
        aria-label="Autopilot scope"
        data-testid="autopilot-scope"
        [value]="effectiveScope()"
        (change)="onScopeChange($event)"
      >
        <option value="all">All projects</option>
        @for (p of projects(); track p.id) {
          <option [value]="p.id">{{ p.name }}</option>
        }
      </select>

      <button
        type="button"
        class="sy-btn sy-btn-sm"
        [class.bg-[var(--sy-green-soft)]]="playing()"
        [class.text-[var(--sy-green)]]="playing()"
        [class.sy-btn-soft]="!playing()"
        [attr.aria-pressed]="playing()"
        [disabled]="busy() || fleetPaused()"
        [syTooltip]="toggleTooltip()"
        data-testid="autopilot-toggle"
        (click)="toggle()"
      >
        <app-icon [name]="playing() ? 'pause' : 'play'" [size]="11" />
        <span>{{ playing() ? 'Pause' : 'Play' }}</span>
      </button>

      <span
        class="inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-[11px] font-medium"
        [class.bg-[var(--sy-green-soft)]]="displayState() === 'playing'"
        [class.text-[var(--sy-green)]]="displayState() === 'playing'"
        [class.bg-[var(--sy-blue-soft)]]="displayState() === 'finished'"
        [class.text-[var(--sy-blue)]]="displayState() === 'finished'"
        [class.bg-[var(--sy-surface-2)]]="displayState() === 'paused'"
        [class.text-[var(--sy-text-muted)]]="displayState() === 'paused'"
        [attr.data-state]="displayState()"
        data-testid="autopilot-state"
      >
        {{ stateLabel() }}
      </span>

      <span
        class="hidden sm:inline-flex items-center gap-1.5 text-[11px] font-mono-ide text-[var(--sy-text-secondary)] pl-0.5"
        data-testid="autopilot-counts"
      >
        <span data-testid="autopilot-count-dispatchable">{{ counts().dispatchable }} dispatchable</span>
        <span aria-hidden="true">·</span>
        <span data-testid="autopilot-count-running">{{ counts().running }} running</span>
        <span aria-hidden="true">·</span>
        <span
          [class.text-[var(--sy-amber)]]="counts().needs_you > 0"
          data-testid="autopilot-count-needs-you"
        >
          {{ counts().needs_you }} needs you
        </span>
      </span>
    </div>
  `,
})
export class AutopilotControlComponent {
  private readonly fleet = inject(FleetStore);
  private readonly control = inject(ControlStore);
  private readonly api = inject(ApiClientService);

  /** 'all' for global autopilot, or a project UUID. */
  readonly scope = signal<string>('all');
  readonly busy = signal(false);

  readonly projects = computed(() => this.fleet.projects());

  /** Falls back to 'all' if a previously selected project was deleted. */
  readonly effectiveScope = computed(() => {
    const s = this.scope();
    if (s === 'all') return 'all';
    return this.projects().some((p) => p.id === s) ? s : 'all';
  });

  readonly fleetPaused = computed(() => this.control.paused() || this.fleet.autopilot().fleet_paused);

  readonly selectedProjectState = computed(() => {
    const s = this.effectiveScope();
    if (s === 'all') return null;
    return this.fleet.autopilot().projects.find((p) => p.project_id === s) ?? null;
  });

  readonly playing = computed(() => {
    if (this.fleetPaused()) return false;
    const s = this.effectiveScope();
    const ap = this.fleet.autopilot();
    if (s === 'all') return ap.global;
    const ps = this.selectedProjectState();
    if (ps) return ps.play;
    const proj = this.projects().find((p) => p.id === s);
    if (!proj || proj.autopilot_paused) return false;
    return ap.global || !!proj.autopilot;
  });

  readonly displayState = computed<AutopilotDisplayState>(() => {
    if (this.fleetPaused()) return 'paused';
    const s = this.effectiveScope();
    const ap = this.fleet.autopilot();
    if (s === 'all') return ap.state;
    const ps = this.selectedProjectState();
    if (ps) return ps.state;
    return this.playing() ? 'playing' : 'paused';
  });

  readonly stateLabel = computed(() => {
    if (this.fleetPaused()) return 'Paused (fleet)';
    return STATE_LABELS[this.displayState()];
  });

  readonly counts = computed<AutopilotCounts>(() => {
    const s = this.effectiveScope();
    const ap = this.fleet.autopilot();
    if (s === 'all') return ap.counts;
    return this.selectedProjectState()?.counts ?? { dispatchable: 0, running: 0, needs_you: 0, review: 0 };
  });

  readonly toggleTooltip = computed(() => {
    if (this.fleetPaused()) return 'Pause everything is active — resume the fleet first';
    const isAll = this.effectiveScope() === 'all';
    if (this.playing()) {
      return isAll ? 'Pause global autopilot (projects playing individually stay on)' : 'Pause autopilot for this project';
    }
    return isAll ? 'Play autopilot for all projects not explicitly paused' : 'Play autopilot for this project';
  });

  onScopeChange(ev: Event): void {
    const target = ev.target as HTMLSelectElement | null;
    if (target) this.scope.set(target.value);
  }

  async toggle(): Promise<void> {
    if (this.busy() || this.fleetPaused()) return;
    const s = this.effectiveScope();
    const nextPlay = !this.playing();
    this.busy.set(true);
    try {
      if (s === 'all') {
        const updated = await this.api.updateSettings({ autopilot_global: nextPlay });
        this.fleet.settings.set(updated);
        const ap = await this.api.getAutopilot();
        this.fleet.applyAutopilot(ap);
      } else {
        const ps = await this.api.setProjectAutopilot(s, nextPlay);
        this.fleet.applyAutopilotProject(ps);
      }
    } finally {
      this.busy.set(false);
    }
  }
}

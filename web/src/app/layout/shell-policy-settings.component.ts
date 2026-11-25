import { ChangeDetectionStrategy, Component, OnInit, computed, inject, signal } from '@angular/core';
import { ShellPolicyMode, ShellPolicySettings } from '../core/models/activity.models';
import { ApiClientService } from '../core/services/api-client.service';
import { TooltipDirective } from '../shared/tooltip.directive';
import { FleetStore } from '../core/state/fleet.store';

/** The modes, in the order offered, with what each one does in plain words. */
export const SHELL_POLICY_MODES: ReadonlyArray<{ id: ShellPolicyMode; label: string; hint: (minutes: number) => string }> = [
  {
    id: 'remind',
    label: 'Remind me',
    hint: (m) => `After ${m} min idle, one Needs-you item per session: Kill all, Ask agent to clean up, or Keep running.`,
  },
  {
    id: 'ask',
    label: 'Ask the agent',
    hint: () => 'When a session ends with shells left, Syre asks its agent once to stop what it started (not while you are in control).',
  },
  {
    id: 'stop',
    label: 'Stop after idle',
    hint: (m) => `Kills the shells of a session idle for ${m} min, audit-logged. Never when it is pinned with Keep.`,
  },
];

export function shellModeLabel(mode: string): string {
  return SHELL_POLICY_MODES.find((m) => m.id === mode)?.label ?? mode;
}

/**
 * Settings → Shells left running (SYR-DCEZV): what Syre does about shells (dev servers,
 * watchers) a finished session leaves running: a global mode and idle period, and a mode
 * per project.
 */
@Component({
  selector: 'sy-shell-policy-settings',
  imports: [TooltipDirective],
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section class="rounded-[var(--sy-radius-lg)] bg-black/20 p-4 space-y-3 text-[12px]" data-testid="settings-shells" aria-labelledby="sy-shells-title">
      <div id="sy-shells-title" class="text-[11px] font-semibold uppercase tracking-wider text-[#8e9098]">Shells left running</div>
      <p class="m-0 text-[11.5px] text-[#8e9098]">Dev servers and watchers a session started and left running after it ended.</p>
      @if (draft(); as d) {
        <div class="grid grid-cols-[1fr_auto] items-center gap-2">
          <label for="sy-shells-mode" class="text-[#d4d4d8]">By default</label>
          <select id="sy-shells-mode" class="sy-field sy-field-sm w-44" data-testid="shells-mode" [value]="d.mode" (change)="setMode($any($event.target).value)">
            @for (m of modes; track m.id) {
              <option [value]="m.id" [selected]="m.id === d.mode">{{ m.label }}</option>
            }
          </select>
          <label for="sy-shells-idle" class="text-[#d4d4d8]">Idle period</label>
          <span class="flex items-center gap-1.5">
            <input id="sy-shells-idle" type="number" min="1" max="10080" class="sy-field sy-field-sm w-20 font-mono-ide" data-testid="shells-idle" [value]="d.idle_minutes" (input)="setIdle($any($event.target).value)" />
            <span class="text-[10.5px] text-[#8e9098]">min</span>
          </span>
        </div>
        <p class="m-0 text-[11.5px] leading-relaxed text-[#a1a3ab]" data-testid="shells-hint">{{ hint() }}</p>
        @if (projects().length) {
          <details class="rounded-[var(--sy-radius-md)] bg-black/20" data-testid="shells-projects" [open]="overrides() > 0">
            <summary class="cursor-pointer list-none px-3 py-2 text-[12px] text-[#a1a3ab] hover:text-[#ececed]">
              Per project{{ overrides() ? ' (' + overrides() + ' set)' : '' }}
            </summary>
            <div class="px-3 pb-3 space-y-1.5">
              @for (p of projects(); track p.id) {
                <label class="flex items-center gap-2">
                  <span class="flex-1 truncate text-[#d4d4d8]" [syTooltip]="p.name">{{ p.name }}</span>
                  <select class="sy-field sy-field-sm w-44" [attr.data-testid]="'shells-project-' + p.sid" [value]="d.projects[p.id] || ''" (change)="setProject(p.id, $any($event.target).value)">
                    <option value="" [selected]="!d.projects[p.id]">Default ({{ label(d.mode) }})</option>
                    @for (m of modes; track m.id) {
                      <option [value]="m.id" [selected]="d.projects[p.id] === m.id">{{ m.label }}</option>
                    }
                  </select>
                </label>
              }
            </div>
          </details>
        }
        <div class="flex items-center gap-2">
          <button type="button" class="sy-btn sy-btn-sm sy-btn-primary" data-testid="shells-save" [disabled]="saving()" (click)="save()">Save</button>
          @if (note()) {
            <span class="text-[11px] text-[#9ece6a]" role="status">{{ note() }}</span>
          }
        </div>
      } @else if (!error()) {
        <p class="m-0 text-[#8e9098]">Loading…</p>
      }
      @if (error()) {
        <p class="m-0 text-[11.5px] text-[#f3a5a5]" role="alert">{{ error() }}</p>
      }
    </section>
  `,
})
export class ShellPolicySettingsComponent implements OnInit {
  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);

  readonly modes = SHELL_POLICY_MODES;
  readonly draft = signal<ShellPolicySettings | null>(null);
  readonly saving = signal(false);
  readonly note = signal('');
  readonly error = signal('');

  readonly projects = computed(() => [...this.fleet.projects()].sort((a, b) => a.name.localeCompare(b.name)));
  readonly overrides = computed(() => Object.values(this.draft()?.projects ?? {}).filter(Boolean).length);
  readonly hint = computed(() => {
    const d = this.draft();
    const m = SHELL_POLICY_MODES.find((x) => x.id === d?.mode);
    return m && d ? m.hint(d.idle_minutes) : '';
  });

  async ngOnInit(): Promise<void> {
    try {
      this.draft.set(normalize(await this.api.getShellPolicy()));
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : 'Could not load the shell policy');
    }
  }

  label(mode: string): string {
    return shellModeLabel(mode);
  }

  setMode(mode: ShellPolicyMode): void {
    this.draft.update((d) => (d ? { ...d, mode } : d));
    this.note.set('');
  }

  setIdle(v: string): void {
    this.draft.update((d) => (d ? { ...d, idle_minutes: Math.round(Number(v)) || 0 } : d));
    this.note.set('');
  }

  setProject(id: string, mode: ShellPolicyMode | ''): void {
    this.draft.update((d) => {
      if (!d) return d;
      const projects = { ...d.projects };
      if (mode) projects[id] = mode;
      else delete projects[id];
      return { ...d, projects };
    });
    this.note.set('');
  }

  async save(): Promise<void> {
    const d = this.draft();
    if (!d) return;
    this.saving.set(true);
    this.error.set('');
    try {
      this.draft.set(normalize(await this.api.setShellPolicy(d)));
      this.note.set('Saved');
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : 'Could not save the shell policy');
    } finally {
      this.saving.set(false);
    }
  }
}

function normalize(p: ShellPolicySettings): ShellPolicySettings {
  return { mode: p.mode || 'remind', idle_minutes: p.idle_minutes || 30, projects: { ...(p.projects ?? {}) } };
}

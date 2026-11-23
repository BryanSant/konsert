import { ChangeDetectionStrategy, Component, computed, inject, input, output } from '@angular/core';
import { WorkItem } from '../../core/state/work-logic';
import { WorkStore } from '../../core/state/work.store';
import { IconComponent } from '../../shared/icon.component';

import { ControlLabelComponent } from '../control-label.component';
import { ModeUiStore } from '../mode-ui.store';

/**
 * Command's left pane, "Live work": running and queued sessions grouped by project.
 * Deliberately plain and self-contained: the project-story design (headlines, machine
 * heat avatars, activity indicators) replaces this component wholesale once signed off.
 * It shows no needs-you items: those live only in the right rail.
 */
@Component({
  selector: 'ks-live-work',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, ControlLabelComponent],
  host: { class: 'flex flex-col min-h-0' },
  template: `
    <div class="flex items-center justify-between px-4 pt-3 pb-2 shrink-0">
      <h2 class="m-0 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--ks-text-secondary)]" id="ks-live-work-heading">Live work</h2>
      @if (!ui.firstRun()) {
        <span class="font-mono-ide text-[11px] text-[var(--ks-text-muted)]">{{ work.live().length }}</span>
      }
    </div>
    @if (ui.firstRun()) {
      <p class="px-4 text-[13px] leading-relaxed text-[var(--ks-text-secondary)]" data-testid="live-work-empty">
        Every agent session on your machines shows up here, whether the orchestrator started it or you did, grouped by project.
      </p>
    } @else if (groups().length === 0) {
      <p class="px-4 text-[12.5px] leading-relaxed text-[var(--ks-text-muted)]" data-testid="live-work-empty">
        Nothing is running. Ask the orchestrator for something, or start a session.
      </p>
      <div class="px-3 pt-2 flex flex-wrap gap-1.5">
        <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" (click)="ui.newSession('chat')"><app-icon name="plus" [size]="12" />Agent chat</button>
        <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" (click)="ui.newSession('terminal')"><app-icon name="shell" [size]="12" />Terminal</button>
      </div>
    } @else {
      <nav class="flex-1 min-h-0 overflow-y-auto px-2 pb-3" aria-labelledby="ks-live-work-heading" data-testid="live-work">
        @for (g of groups(); track g.key) {
          <section class="pt-2" [attr.data-project]="g.name">
            <h3 class="flex items-center gap-2 px-2 pb-1 m-0 text-[11px] font-medium uppercase tracking-wide text-[var(--ks-text-muted)]">
              <span class="truncate">{{ g.name }}</span>
              <span class="font-mono-ide normal-case tracking-normal">{{ summary(g.running, g.queued) }}</span>
            </h3>
            @if (headlines()[g.name]; as h) {
              <p class="m-0 px-2 pb-1 text-[11.5px] leading-snug text-[var(--ks-text-secondary)]" data-testid="project-headline">{{ h }}</p>
            }
            <ul class="m-0 p-0 list-none">
              @for (it of g.items; track it.id) {
                <li>
                  <button
                    type="button"
                    class="ks-focusable w-full flex items-center gap-2 rounded-[var(--ks-radius-md)] px-2 py-1.5 text-left hover:bg-[var(--ks-raised)]"
                    [class.bg-[var(--ks-blue-soft)]]="activeId() === it.id"
                    [attr.aria-current]="activeId() === it.id ? 'true' : null"
                    [attr.data-work-item]="it.sid"
                    (click)="open.emit(it)"
                  >
                    <ks-control-label [label]="it.control" [fixed]="true" />
                    <span class="flex-1 min-w-0">
                      <span class="block truncate text-[12.5px] text-[var(--ks-text)]">{{ it.title }}</span>
                      <span class="block truncate text-[11px]" [class.text-[var(--ks-amber)]]="it.stale" [class.text-[var(--ks-text-muted)]]="!it.stale">{{ meta(it) }}</span>
                    </span>
                  </button>
                </li>
              }
            </ul>
          </section>
        }
      </nav>
    }
  `,
})
export class LiveWorkComponent {
  readonly activeId = input('');
  readonly open = output<WorkItem>();
  readonly work = inject(WorkStore);
  readonly ui = inject(ModeUiStore);
  readonly groups = computed(() => this.work.liveByProject());
  /** The orchestrator's headline per project name, where it wrote one. */
  readonly headlines = computed(() => {
    const out: Record<string, string> = {};
    for (const g of this.work.projectGroups()) if (g.headlineSource === 'orchestrator' && g.headline) out[g.name] = g.headline;
    return out;
  });

  summary(running: number, queued: number): string {
    const parts = [];
    if (running) parts.push(`${running} running`);
    if (queued) parts.push(`${queued} queued`);
    return parts.join(' · ');
  }

  meta(it: WorkItem): string {
    const who = it.sessionKind === 'terminal' ? 'terminal' : it.agent;
    return [who, it.machine, it.observed ? 'observed' : '', it.stale ? 'machine not reporting' : it.now].filter(Boolean).join(' · ');
  }
}

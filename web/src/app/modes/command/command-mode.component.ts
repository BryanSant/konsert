import { ChangeDetectionStrategy, Component, afterNextRender, computed, effect, inject, signal, untracked, viewChild, Injector } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { ActivatedRoute, Router } from '@angular/router';
import { map } from 'rxjs';
import { WorkItem } from '../../core/state/work-logic';
import { WorkStore } from '../../core/state/work.store';
import { ChatViewComponent } from '../../features/chat/chat-view.component';
import { HotkeyService } from '../../layout/hotkey.service';
import { IconComponent } from '../../shared/icon.component';
import { PopoverComponent, PopoverTriggerDirective } from '../../shared/popover.component';
import { TooltipDirective } from '../../shared/tooltip.directive';
import { ModeUiStore } from '../mode-ui.store';
import { CommandTabsStore } from './command-tabs.store';
import { FirstRunComponent } from './first-run.component';
import { FleetStripComponent } from './fleet-strip.component';
import { LiveWorkComponent } from './live-work.component';
import { NeedsYouRailComponent } from './needs-you-rail.component';
import { SessionTabComponent } from './session-tab.component';

export const ORCHESTRATOR = 'orchestrator';

/**
 * 1 Command, the desktop default: chatting with the orchestrator is the main job. Live
 * work on the left, the conversation tabs in the centre (the orchestrator pinned, any
 * agent session as another tab), the one Needs-you area on the right, the fleet/quota
 * strip at the bottom. On a fresh hub the orchestrator tab is the welcome.
 */
@Component({
  selector: 'ks-command-mode',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [
    ChatViewComponent,
    LiveWorkComponent,
    NeedsYouRailComponent,
    SessionTabComponent,
    FirstRunComponent,
    FleetStripComponent,
    IconComponent,
    TooltipDirective,
    PopoverComponent,
    PopoverTriggerDirective,
  ],
  host: { class: 'flex flex-col h-full min-h-0' },
  template: `
    <div class="flex flex-1 min-h-0">
      <aside class="hidden lg:flex flex-col w-[272px] shrink-0 bg-[var(--ks-bg-chrome)] border-r border-[var(--ks-hairline)]" aria-label="Live work">
        <ks-live-work class="flex-1" [activeId]="activeId()" (open)="openItem($event)" />
      </aside>

      <main class="flex-1 min-w-0 flex flex-col overflow-hidden" aria-label="Conversations">
        <div class="flex items-end gap-0.5 px-3 pt-2 shrink-0 border-b border-[var(--ks-hairline)] overflow-x-auto" role="tablist" aria-label="Conversations" data-testid="command-tabs" (keydown)="onTabKey($event)">
          @for (t of tabs(); track t.id; let i = $index) {
            <div class="group relative flex items-center shrink-0" [attr.data-tab]="t.id === orchestrator ? 'orchestrator' : t.sid">
              <button
                type="button"
                role="tab"
                class="ks-command-tab"
                [id]="'ks-ctab-' + t.id"
                [attr.aria-selected]="activeId() === t.id"
                [attr.aria-controls]="'ks-cpanel-' + t.id"
                [tabindex]="activeId() === t.id ? 0 : -1"
                [class.pr-8]="t.id !== orchestrator"
                (click)="select(t.id)"
              >
                <app-icon [name]="t.icon" [size]="13" />
                <span class="max-w-[200px] truncate">{{ t.label }}</span>
                @if (t.attention) {
                  <span class="h-1.5 w-1.5 rounded-full bg-[var(--ks-amber)]" aria-hidden="true"></span>
                  <span class="ks-sr-only">(waiting for you)</span>
                }
              </button>
              @if (t.id !== orchestrator) {
                <button
                  type="button"
                  class="ks-icon-btn ks-icon-btn-sm absolute right-1 top-1/2 -translate-y-1/2 !w-6 !h-6"
                  tabindex="-1"
                  [attr.aria-label]="'Close ' + t.label"
                  [attr.data-testid]="'close-tab-' + t.sid"
                  (click)="close(t.id)"
                >
                  <app-icon name="close" [size]="11" />
                </button>
              }
            </div>
          }
          <ks-popover label="New session" placement="bottom-start" panelClass="w-[220px]">
            <button type="button" ksPopoverTrigger class="ks-icon-btn mb-1 ml-1" aria-label="New session" ksTooltip="New agent chat or terminal" data-testid="command-new">
              <app-icon name="plus" [size]="14" />
            </button>
            <button type="button" role="menuitem" class="ks-menu-item items-center" (click)="ui.newSession('chat')">
              <app-icon name="sparkle" [size]="14" />
              <span class="ks-menu-label">New agent chat…</span>
            </button>
            <button type="button" role="menuitem" class="ks-menu-item items-center" (click)="ui.newSession('terminal')">
              <app-icon name="shell" [size]="14" />
              <span class="ks-menu-label">New terminal…</span>
            </button>
          </ks-popover>
        </div>

        <div class="flex-1 min-h-0 relative">
          <!-- The orchestrator stays mounted (its draft, scroll and stream survive tab switches). -->
          <div role="tabpanel" [id]="'ks-cpanel-' + orchestrator" [attr.aria-labelledby]="'ks-ctab-' + orchestrator" class="absolute inset-0" [class.hidden]="activeId() !== orchestrator">
            @if (showWelcome()) {
              <ks-first-run (start)="startChatting($event)" />
            } @else {
              <app-chat-view (openNewTerminal)="ui.newSession('terminal')" />
            }
          </div>
          @if (activeItem(); as it) {
            <div role="tabpanel" [id]="'ks-cpanel-' + it.id" [attr.aria-labelledby]="'ks-ctab-' + it.id" class="absolute inset-0">
              <ks-session-tab [item]="it" />
            </div>
          } @else if (missingSid()) {
            <div class="absolute inset-0 flex flex-col items-center justify-center gap-3 text-[13px] text-[var(--ks-text-secondary)]" role="status">
              <span>No session {{ missingSid() }} here (it may have been removed).</span>
              <button type="button" class="ks-btn ks-btn-soft" (click)="select(orchestrator)">Back to the orchestrator</button>
            </div>
          }
        </div>
      </main>

      <aside class="hidden md:flex flex-col w-[300px] lg:w-[332px] shrink-0 bg-[var(--ks-bg-chrome)] border-l border-[var(--ks-hairline)]" aria-label="Needs you">
        <ks-needs-you-rail class="flex-1" />
      </aside>
    </div>
    <ks-fleet-strip />
  `,
  styles: `
    .ks-command-tab {
      display: flex;
      align-items: center;
      gap: 6px;
      height: 34px;
      padding: 0 12px;
      border: 0;
      border-radius: var(--ks-radius-md) var(--ks-radius-md) 0 0;
      background: transparent;
      color: var(--ks-text-secondary);
      font-size: 12.5px;
      font-weight: 500;
      white-space: nowrap;
      cursor: pointer;
    }
    .ks-command-tab:hover {
      color: var(--ks-text);
      background: var(--ks-raised);
    }
    .ks-command-tab[aria-selected='true'] {
      color: var(--ks-text);
      background: var(--ks-surface-1);
      box-shadow: inset 0 -2px 0 var(--ks-blue);
    }
    .ks-command-tab:focus-visible {
      outline: none;
      box-shadow: var(--ks-focus-ring);
    }
  `,
})
export class CommandModeComponent {
  readonly orchestrator = ORCHESTRATOR;
  readonly ui = inject(ModeUiStore);
  private readonly work = inject(WorkStore);
  private readonly tabStore = inject(CommandTabsStore);
  private readonly router = inject(Router);
  private readonly injector = inject(Injector);
  private readonly chatView = viewChild(ChatViewComponent);

  readonly sid = toSignal(inject(ActivatedRoute).paramMap.pipe(map((p) => p.get('sid') ?? '')), { initialValue: '' });
  /** The welcome stays up after "Add this machine" until the user starts chatting. */
  private readonly welcomeDismissed = signal(false);
  private readonly sawFirstRun = signal(false);
  readonly showWelcome = computed(() => this.ui.firstRun() || (this.sawFirstRun() && !this.welcomeDismissed()));

  readonly activeItem = computed<WorkItem | null>(() => {
    const sid = this.sid();
    return sid ? (this.work.bySid(sid) ?? null) : null;
  });
  readonly activeId = computed(() => this.activeItem()?.id ?? (this.sid() ? '' : ORCHESTRATOR));
  readonly missingSid = computed(() => (this.sid() && !this.activeItem() && this.ui.fleetLoaded() ? this.sid() : ''));

  readonly tabs = computed(() => {
    const byId = this.work.byId();
    const out: Array<{ id: string; sid: string; label: string; icon: string; attention: boolean }> = [
      { id: ORCHESTRATOR, sid: '', label: 'Orchestrator', icon: 'orchestrator', attention: false },
    ];
    for (const id of this.tabStore.ids()) {
      const it = byId[id];
      if (!it) continue;
      out.push({
        id,
        sid: it.sid,
        label: `${it.sessionKind === 'terminal' ? it.machine || 'terminal' : it.agent} · ${it.title}`,
        icon: it.sessionKind === 'terminal' ? 'shell' : it.agent,
        attention: it.control === 'ASKING',
      });
    }
    return out;
  });

  constructor() {
    // Opening /command/<sid> opens that session as a tab.
    effect(() => {
      const it = this.activeItem();
      if (it) untracked(() => this.tabStore.add(it.id));
    });
    effect(() => {
      if (this.ui.firstRun()) untracked(() => this.sawFirstRun.set(true));
    });
    // Forget tabs whose session is gone once everything has loaded.
    effect(() => {
      if (!this.ui.fleetLoaded()) return;
      const byId = this.work.byId();
      if (Object.keys(byId).length === 0) return;
      untracked(() => this.tabStore.prune((id) => !!byId[id]));
    });
    inject(HotkeyService).register((action) => {
      if (action.kind === 'select-tab') {
        const t = this.tabs()[action.index];
        if (!t) return false;
        this.select(t.id);
        return true;
      }
      if (action.kind === 'close-tab') {
        const id = this.activeId();
        if (!id || id === ORCHESTRATOR) return false;
        this.close(id);
        return true;
      }
      return false;
    });
  }

  select(id: string): void {
    if (id === ORCHESTRATOR) {
      void this.router.navigate(['/command']);
      return;
    }
    const it = this.work.byId()[id];
    if (it) void this.router.navigate(['/command', it.sid]);
  }

  openItem(it: WorkItem): void {
    this.select(it.id);
  }

  close(id: string): void {
    const next = this.tabStore.close(id);
    if (this.activeId() === id) this.select(next || ORCHESTRATOR);
  }

  /** Arrow keys move along the tabs, Home/End jump, Delete closes (not the orchestrator). */
  onTabKey(ev: KeyboardEvent): void {
    if ((ev.target as HTMLElement).getAttribute('role') !== 'tab') return;
    const list = this.tabs();
    const i = list.findIndex((t) => t.id === this.activeId());
    let next = -1;
    if (ev.key === 'ArrowRight') next = (i + 1) % list.length;
    else if (ev.key === 'ArrowLeft') next = (i - 1 + list.length) % list.length;
    else if (ev.key === 'Home') next = 0;
    else if (ev.key === 'End') next = list.length - 1;
    else if ((ev.key === 'Delete' || ev.key === 'Backspace') && list[i] && list[i].id !== ORCHESTRATOR) {
      ev.preventDefault();
      this.close(list[i].id);
      return;
    }
    if (next < 0) return;
    ev.preventDefault();
    this.select(list[next].id);
    afterNextRender(() => document.getElementById(`ks-ctab-${list[next].id}`)?.focus(), { injector: this.injector });
  }

  /** From the welcome: switch to the orchestrator chat with the first message ready. */
  startChatting(text: string): void {
    this.welcomeDismissed.set(true);
    afterNextRender(
      () => {
        const composer = this.chatView()?.composer();
        composer?.setDraft(text);
        composer?.focus();
      },
      { injector: this.injector },
    );
  }

}

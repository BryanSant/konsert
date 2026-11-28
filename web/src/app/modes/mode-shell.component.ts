import { PeopleStore } from '../core/state/people.store';
import { DOCUMENT } from '@angular/common';
import { ChangeDetectionStrategy, Component, DestroyRef, computed, inject } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { NavigationEnd, Router, RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { filter, map } from 'rxjs';
import { AuthService } from '../core/services/auth.service';
import { SyreSocketService } from '../core/services/syre-socket.service';
import { ControlStore } from '../core/state/control.store';
import { FleetStore } from '../core/state/fleet.store';
import { phoneSignal } from './media';
import { NeedsYouStore } from '../core/state/needs-you.store';
import { fromTextField } from '../core/state/questions-logic';
import { WorkStore } from '../core/state/work.store';
import { PauseEverythingComponent, PausedBannerComponent, shortWhen } from '../features/control/pause-everything.component';
import { IconComponent } from '../shared/icon.component';
import { TooltipDirective } from '../shared/tooltip.directive';
import { MODES } from '../app.routes';
import { ModeUiStore } from './mode-ui.store';

/** Keys 1–4 switch modes unless the user is typing or a dialog is open. */
export function modeForKey(ev: Pick<KeyboardEvent, 'key' | 'metaKey' | 'ctrlKey' | 'altKey' | 'shiftKey' | 'target' | 'defaultPrevented'>): string | null {
  if (ev.metaKey || ev.ctrlKey || ev.altKey || ev.shiftKey || ev.defaultPrevented) return null;
  if (fromTextField(ev.target)) return null;
  const el = ev.target as HTMLElement | null;
  if (el?.closest?.('dialog[open], [aria-modal="true"], .xterm')) return null;
  return MODES.find((m) => m.key === ev.key)?.id ?? null;
}

/**
 * The frame around every desktop mode: the syre mark, modes 1 Command / 2 Work /
 * 3 Fleet / 4 Wall (keys 1–4), search, the needs-you count, fleet state and the quiet
 * Pause all. Work, Fleet and Wall also get the one-line status banner; Command has its
 * Needs-you rail instead.
 */
@Component({
  selector: 'sy-mode-shell',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RouterOutlet, RouterLink, RouterLinkActive, IconComponent, TooltipDirective, PauseEverythingComponent, PausedBannerComponent],
  host: { class: 'flex flex-col h-full min-h-0' },
  template: `
    <header class="flex items-center gap-2 md:gap-4 h-12 px-3 md:px-4 shrink-0 bg-[var(--sy-bg-chrome)] border-b border-[var(--sy-hairline)]" data-testid="mode-header">
      <a routerLink="/command" class="sy-focusable flex items-center gap-2 rounded-[var(--sy-radius-sm)] px-1 text-[14px] font-semibold tracking-tight text-[var(--sy-text)] no-underline" aria-label="Syre home">
        <span class="h-2 w-2 rounded-full" [class.bg-[var(--sy-green)]]="healthy()" [class.bg-[var(--sy-text-muted)]]="!healthy()" aria-hidden="true"></span>
        syre
      </a>
      @if (phone()) {
        <a routerLink="/inbox" class="sy-btn sy-btn-sm !h-[44px]" data-testid="back-to-inbox">
          <span class="inline-flex rotate-180"><app-icon name="chevron-right" [size]="12" /></span>
          Inbox
        </a>
      }
      <nav aria-label="Modes" class="hidden md:flex items-center gap-0.5">
        @for (m of modes; track m.id) {
          <a
            [routerLink]="['/', m.id]"
            routerLinkActive="sy-mode-active"
            #rla="routerLinkActive"
            [attr.aria-current]="rla.isActive ? 'page' : null"
            [attr.aria-keyshortcuts]="m.key"
            [attr.data-mode]="m.id"
            class="sy-mode-link"
          >
            <span class="font-mono-ide text-[11px] text-[var(--sy-text-muted)]">{{ m.key }}</span>
            {{ m.label }}
          </a>
        }
      </nav>

      <div class="flex-1 hidden md:flex justify-center min-w-0">
        <button type="button" class="sy-focusable flex items-center gap-2 w-full max-w-[380px] h-8 rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-1)] px-3 text-[12.5px] text-[var(--sy-text-muted)] hover:bg-[var(--sy-surface-2)]" data-testid="open-palette" aria-keyshortcuts="Meta+K Control+K" (click)="ui.paletteOpen.set(true)">
          <span class="sy-kbd">⌘K</span>
          <span class="truncate">Search work, machines, or run a command</span>
        </button>
      </div>
      <span class="flex-1 md:hidden"></span>

      @if (needsYou.count() > 0) {
        <button
          type="button" (click)="needsYou.open()"
          class="sy-chip sy-chip-amber min-h-8"
          data-testid="needs-you-count"
          [attr.aria-label]="needsYou.count() + ' need you'"
          syTooltip="Go through what needs you: questions, requests, overrides, vetoes and reviews"
        >
          <app-icon name="bell" [size]="13" />
          <span class="font-mono-ide font-semibold">{{ needsYou.count() }}</span>
          <span class="hidden lg:inline">need you</span>
        </button>
      }
      <span class="hidden sm:inline font-mono-ide text-[11.5px]" data-testid="fleet-state" [class.text-[var(--sy-violet)]]="control.paused()" [class.text-[var(--sy-text-muted)]]="!control.paused()">{{ fleetState() }}</span>
      @if (!ui.firstRun()) {
        <sy-pause-everything />
      }
      <button type="button" class="sy-icon-btn" aria-label="Settings" syTooltip="Settings" (click)="ui.settingsOpen.set(true)">
        <app-icon name="settings" [size]="15" />
      </button>
    </header>

    <sy-paused-banner />

    @if (showBanner()) {
      <div class="flex flex-wrap items-center gap-x-3.5 gap-y-1 px-4 py-1.5 text-[12.5px] shrink-0 border-b border-[var(--sy-hairline)]" [class.bg-[var(--sy-amber-soft)]]="needsYou.count() > 0" data-testid="status-banner" role="status">
        @if (needsYou.count() > 0) {
          <button type="button" class="font-semibold text-[var(--sy-text)] hover:underline cursor-pointer" (click)="needsYou.open()">▲ {{ needsYou.count() }} need{{ needsYou.count() === 1 ? 's' : '' }} you</button>
        } @else {
          <span class="text-[var(--sy-text-secondary)]">Nothing needs you</span>
        }
        <span>{{ counts().working }} working</span>
        <span class="text-[var(--sy-text-muted)]" aria-hidden="true">·</span>
        <span>{{ counts().queued }} queued</span>
        @if (counts().review) {
          <span class="text-[var(--sy-text-muted)]" aria-hidden="true">·</span>
          <span>{{ counts().review }} ready for review</span>
        }
        @if (counts().held) {
          <span class="text-[var(--sy-text-muted)]" aria-hidden="true">·</span>
          <span>{{ counts().held }} held or suspended</span>
        }
        <span class="text-[var(--sy-text-muted)]" aria-hidden="true">·</span>
        <span>fleet {{ ui.onlineCount() }}/{{ total() }} up</span>
      </div>
    }

    <div class="flex-1 min-h-0 relative">
      <router-outlet />
    </div>
  `,
  styles: `
    .sy-mode-link {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      height: 30px;
      padding: 0 10px;
      border-radius: var(--sy-radius-sm);
      color: var(--sy-text-secondary);
      font-size: 13px;
      font-weight: 500;
      text-decoration: none;
    }
    .sy-mode-link:hover {
      background: var(--sy-raised);
      color: var(--sy-text);
    }
    .sy-mode-link:focus-visible {
      outline: none;
      box-shadow: var(--sy-focus-ring);
    }
    .sy-mode-link.sy-mode-active {
      background: var(--sy-raised-strong);
      color: var(--sy-text);
      box-shadow: inset 0 -2px 0 var(--sy-accent-tab);
    }
  `,
})
export class ModeShellComponent {
  readonly ui = inject(ModeUiStore);
  readonly control = inject(ControlStore);
  readonly needsYou = inject(NeedsYouStore);
  private readonly work = inject(WorkStore);
  private readonly fleet = inject(FleetStore);
  private readonly auth = inject(AuthService);
  private readonly people = inject(PeopleStore);
  private readonly socket = inject(SyreSocketService);
  private readonly router = inject(Router);
  readonly modes = MODES;

  private readonly url = toSignal(
    this.router.events.pipe(
      filter((e): e is NavigationEnd => e instanceof NavigationEnd),
      map((e) => e.urlAfterRedirects),
    ),
    { initialValue: this.router.url },
  );
  readonly showBanner = computed(() => !this.url().startsWith('/command') && !this.ui.firstRun());
  readonly counts = this.work.counts;
  readonly total = computed(() => this.fleet.workers().length);
  readonly healthy = computed(() => this.socket.state() === 'connected' && this.ui.onlineCount() > 0);
  readonly phone = phoneSignal();

  readonly fleetState = computed(() => {
    const p = this.control.pause();
    if (p.paused) {
      const by = p.paused_by ? ` by ${p.paused_by === this.auth.user() ? 'you' : this.people.nameOf(p.paused_by)}` : '';
      const at = shortWhen(p.paused_at);
      return `FLEET: PAUSED${by}${at ? ` ${at}` : ''}`;
    }
    const n = this.fleet.workers().length;
    if (this.ui.fleetLoaded() && n === 0) return 'FLEET: no machines yet';
    return 'FLEET: RUNNING';
  });

  constructor() {
    const doc = inject(DOCUMENT);
    const onKey = (ev: KeyboardEvent) => {
      const mode = modeForKey(ev);
      if (!mode) return;
      ev.preventDefault();
      void this.router.navigate(['/', mode]);
    };
    doc.addEventListener('keydown', onKey);
    inject(DestroyRef).onDestroy(() => doc.removeEventListener('keydown', onKey));
  }
}

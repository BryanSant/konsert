import { DOCUMENT } from '@angular/common';
import { ChangeDetectionStrategy, Component, DestroyRef, computed, inject } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { NavigationEnd, Router, RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { filter, map } from 'rxjs';
import { AuthService } from '../core/services/auth.service';
import { KonsertSocketService } from '../core/services/konsert-socket.service';
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
 * The frame around every desktop mode: the konsert mark, modes 1 Command / 2 Work /
 * 3 Fleet / 4 Wall (keys 1–4), search, the needs-you count, fleet state and the quiet
 * Pause all. Work, Fleet and Wall also get the one-line status banner; Command has its
 * Needs-you rail instead.
 */
@Component({
  selector: 'ks-mode-shell',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RouterOutlet, RouterLink, RouterLinkActive, IconComponent, TooltipDirective, PauseEverythingComponent, PausedBannerComponent],
  host: { class: 'flex flex-col h-full min-h-0' },
  template: `
    <header class="flex items-center gap-2 md:gap-4 h-12 px-3 md:px-4 shrink-0 bg-[var(--ks-bg-chrome)] border-b border-[var(--ks-hairline)]" data-testid="mode-header">
      <a routerLink="/command" class="ks-focusable flex items-center gap-2 rounded-[var(--ks-radius-sm)] px-1 text-[14px] font-semibold tracking-tight text-[var(--ks-text)] no-underline" aria-label="Konsert home">
        <span class="h-2 w-2 rounded-full" [class.bg-[var(--ks-green)]]="healthy()" [class.bg-[var(--ks-text-muted)]]="!healthy()" aria-hidden="true"></span>
        konsert
      </a>
      @if (phone()) {
        <a routerLink="/inbox" class="ks-btn ks-btn-sm !h-[44px]" data-testid="back-to-inbox">
          <span class="inline-flex rotate-180"><app-icon name="chevron-right" [size]="12" /></span>
          Inbox
        </a>
      }
      <nav aria-label="Modes" class="hidden md:flex items-center gap-0.5">
        @for (m of modes; track m.id) {
          <a
            [routerLink]="['/', m.id]"
            routerLinkActive="ks-mode-active"
            #rla="routerLinkActive"
            [attr.aria-current]="rla.isActive ? 'page' : null"
            [attr.aria-keyshortcuts]="m.key"
            [attr.data-mode]="m.id"
            class="ks-mode-link"
          >
            <span class="font-mono-ide text-[11px] text-[var(--ks-text-muted)]">{{ m.key }}</span>
            {{ m.label }}
          </a>
        }
      </nav>

      <div class="flex-1 hidden md:flex justify-center min-w-0">
        <button type="button" class="ks-focusable flex items-center gap-2 w-full max-w-[380px] h-8 rounded-[var(--ks-radius-md)] bg-[var(--ks-surface-1)] px-3 text-[12.5px] text-[var(--ks-text-muted)] hover:bg-[var(--ks-surface-2)]" data-testid="open-palette" aria-keyshortcuts="Meta+K Control+K" (click)="ui.paletteOpen.set(true)">
          <span class="ks-kbd">⌘K</span>
          <span class="truncate">Search work, machines, or run a command</span>
        </button>
      </div>
      <span class="flex-1 md:hidden"></span>

      @if (needsYou.count() > 0) {
        <button
          type="button" (click)="needsYou.open()"
          class="ks-chip ks-chip-amber min-h-8"
          data-testid="needs-you-count"
          [attr.aria-label]="needsYou.count() + ' need you'"
          ksTooltip="Go through what needs you: questions, requests, vetoes and reviews"
        >
          <app-icon name="bell" [size]="13" />
          <span class="font-mono-ide font-semibold">{{ needsYou.count() }}</span>
          <span class="hidden lg:inline">need you</span>
        </button>
      }
      <span class="hidden sm:inline font-mono-ide text-[11.5px]" data-testid="fleet-state" [class.text-[var(--ks-violet)]]="control.paused()" [class.text-[var(--ks-text-muted)]]="!control.paused()">{{ fleetState() }}</span>
      @if (!ui.firstRun()) {
        <ks-pause-everything />
      }
      <button type="button" class="ks-icon-btn" aria-label="Settings" ksTooltip="Settings" (click)="ui.settingsOpen.set(true)">
        <app-icon name="settings" [size]="15" />
      </button>
    </header>

    <ks-paused-banner />

    @if (showBanner()) {
      <div class="flex flex-wrap items-center gap-x-3.5 gap-y-1 px-4 py-1.5 text-[12.5px] shrink-0 border-b border-[var(--ks-hairline)]" [class.bg-[var(--ks-amber-soft)]]="needsYou.count() > 0" data-testid="status-banner" role="status">
        @if (needsYou.count() > 0) {
          <button type="button" class="font-semibold text-[var(--ks-text)] hover:underline cursor-pointer" (click)="needsYou.open()">▲ {{ needsYou.count() }} need{{ needsYou.count() === 1 ? 's' : '' }} you</button>
        } @else {
          <span class="text-[var(--ks-text-secondary)]">Nothing needs you</span>
        }
        <span>{{ counts().working }} working</span>
        <span class="text-[var(--ks-text-muted)]" aria-hidden="true">·</span>
        <span>{{ counts().queued }} queued</span>
        @if (counts().review) {
          <span class="text-[var(--ks-text-muted)]" aria-hidden="true">·</span>
          <span>{{ counts().review }} ready for review</span>
        }
        @if (counts().held) {
          <span class="text-[var(--ks-text-muted)]" aria-hidden="true">·</span>
          <span>{{ counts().held }} held or suspended</span>
        }
        <span class="text-[var(--ks-text-muted)]" aria-hidden="true">·</span>
        <span>fleet {{ ui.onlineCount() }}/{{ total() }} up</span>
      </div>
    }

    <div class="flex-1 min-h-0 relative">
      <router-outlet />
    </div>
  `,
  styles: `
    .ks-mode-link {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      height: 30px;
      padding: 0 10px;
      border-radius: var(--ks-radius-sm);
      color: var(--ks-text-secondary);
      font-size: 13px;
      font-weight: 500;
      text-decoration: none;
    }
    .ks-mode-link:hover {
      background: var(--ks-raised);
      color: var(--ks-text);
    }
    .ks-mode-link:focus-visible {
      outline: none;
      box-shadow: var(--ks-focus-ring);
    }
    .ks-mode-link.ks-mode-active {
      background: var(--ks-raised-strong);
      color: var(--ks-text);
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
  private readonly socket = inject(KonsertSocketService);
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
      const by = p.paused_by ? ` by ${p.paused_by === this.auth.user() ? 'you' : p.paused_by}` : '';
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

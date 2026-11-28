import { Component, DestroyRef, OnInit, inject, signal } from '@angular/core';
import { NavigationError, Router, RouterOutlet } from '@angular/router';
import { AppUpdateService } from './core/services/app-update.service';
import { AuthService } from './core/services/auth.service';
import { codeFromPath, isLauncherLink } from './features/auth/device-name';
import { LoginViewComponent } from './features/auth/login-view.component';
import { PairViewComponent } from './features/auth/pair-view.component';

@Component({
  selector: 'app-root',
  standalone: true,
  imports: [LoginViewComponent, PairViewComponent, RouterOutlet],
  template: `
    @if (auth.loading()) {
      <div class="h-screen w-screen flex items-center justify-center bg-[#111214] text-[#8e9098] text-[13px] font-mono-ide" role="status">
        Connecting to Syre...
      </div>
    } @else if (!auth.authenticated()) {
      @if (pairCode(); as code) {
        <app-pair-view [code]="code" [launch]="launchLink" (paired)="leavePairing()" (cancel)="leavePairing()" />
      } @else {
        <app-login-view (pairCode)="startPairing($event)" />
      }
    } @else {
      <!-- Modes and the classic shell are lazy route chunks: the login screen doesn't pay for them. -->
      <router-outlet />
      @if (routeLoadFailed()) {
        <div class="fixed inset-0 z-[70] flex flex-col gap-3 items-center justify-center bg-[#111214] text-[#f09595] text-[13px]" role="alert">
          <span>Syre failed to load. The hub may have been upgraded.</span>
          <button type="button" (click)="updates.reload()" class="rounded-md border border-[#2c2e36] px-3 py-1.5 text-[#ececed] cursor-pointer">Reload</button>
        </div>
      }
    }

    @if (updates.updateReady() || updates.unrecoverable()) {
      <div
        role="alert"
        class="fixed bottom-10 left-1/2 -translate-x-1/2 z-[60] flex items-center gap-3 rounded-lg border border-[#2d3039] bg-[#1b1c20] px-4 py-2.5 text-[12px] text-[#e4e4e7] shadow-2xl"
      >
        <span>
          {{ updates.unrecoverable() ? 'This version of Syre can no longer run. Reload to continue.' : 'A new version of Syre is available.' }}
        </span>
        <button type="button" (click)="updates.reload()" class="rounded-md bg-[#ececed] hover:bg-white px-3 py-1 font-medium text-[#111214] cursor-pointer">
          Reload
        </button>
        @if (!updates.unrecoverable()) {
          <button type="button" (click)="updates.dismiss()" aria-label="Dismiss update notice" class="text-[#8e9098] hover:text-[#ececed] cursor-pointer">Later</button>
        }
      </div>
    }
  `,
})
export class App implements OnInit {
  readonly auth = inject(AuthService);
  readonly updates = inject(AppUpdateService);
  private readonly destroyRef = inject(DestroyRef);
  /** A lazy route chunk failed to load (usually: the hub was upgraded under this page). */
  readonly routeLoadFailed = signal(false);
  /** The pairing code of a /pair/{code} sign-in link this signed-out browser opened. */
  readonly pairCode = signal(typeof location === 'undefined' ? '' : codeFromPath(location.pathname, 'pair'));
  /** The pairing link came from the local launcher: sign in without asking. */
  readonly launchLink = typeof location !== 'undefined' && isLauncherLink(location.hostname, location.hash);
  private readonly router = inject(Router);

  constructor() {
    const sub = inject(Router).events.subscribe((ev) => {
      if (ev instanceof NavigationError) this.routeLoadFailed.set(true);
    });
    this.destroyRef.onDestroy(() => sub.unsubscribe());
  }

  startPairing(code: string): void {
    this.pairCode.set(code);
    void this.router.navigateByUrl(`/pair/${code}`);
  }

  /** Paired (or cancelled): drop the code from the address bar and open the app. */
  leavePairing(): void {
    this.pairCode.set('');
    void this.router.navigateByUrl('/', { replaceUrl: true });
  }

  ngOnInit(): void {
    void this.auth.checkAuth();
    this.updates.start(this.destroyRef);
  }
}

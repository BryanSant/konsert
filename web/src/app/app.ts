import { Component, DestroyRef, OnInit, inject } from '@angular/core';
import { AppUpdateService } from './core/services/app-update.service';
import { AuthService } from './core/services/auth.service';
import { LoginViewComponent } from './features/auth/login-view.component';
import { AppShellComponent } from './layout/app-shell.component';

@Component({
  selector: 'app-root',
  standalone: true,
  imports: [LoginViewComponent, AppShellComponent],
  template: `
    @if (auth.loading()) {
      <div class="h-screen w-screen flex items-center justify-center bg-[#111214] text-[#8e9098] text-[13px] font-mono-ide" role="status">
        Connecting to Konsert...
      </div>
    } @else if (!auth.authenticated()) {
      <app-login-view />
    } @else {
      <!-- The shell (xterm, modals, stores' UI) is a separate lazy chunk: the login screen doesn't pay for it. -->
      @defer (on immediate) {
        <app-shell />
      } @placeholder {
        <div class="h-screen w-screen flex items-center justify-center bg-[#111214] text-[#8e9098] text-[13px] font-mono-ide" role="status">
          Loading workspace...
        </div>
      } @error {
        <div class="h-screen w-screen flex flex-col gap-3 items-center justify-center bg-[#111214] text-[#f09595] text-[13px]" role="alert">
          <span>Konsert failed to load. The hub may have been upgraded.</span>
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
          {{ updates.unrecoverable() ? 'This version of Konsert can no longer run. Reload to continue.' : 'A new version of Konsert is available.' }}
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

  ngOnInit(): void {
    void this.auth.checkAuth();
    this.updates.start(this.destroyRef);
  }
}

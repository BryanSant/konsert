import { Component, inject, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AuthService } from '../../core/services/auth.service';
import { normalizeCode } from './device-name';

/**
 * Signed out. Signing in is pairing: open a one-time link from `syre serve`,
 * `syre pair`, or a signed-in device (or type its code here). The recovery key is
 * a fallback for emergencies.
 */
@Component({
  selector: 'app-login-view',
  standalone: true,
  imports: [FormsModule],
  template: `
    <main class="min-h-dvh w-screen flex items-center justify-center bg-[#111214] px-4 py-8">
      <div class="w-full max-w-sm rounded-xl border border-[#26282e] bg-[#18191c] p-6 shadow-2xl">
        <div class="flex items-center gap-3 mb-5">
          <img src="logo.svg" alt="" class="h-9 w-9 rounded-lg" />
          <div>
            <h1 class="text-[15px] font-semibold tracking-tight text-[#ececed]">Syre</h1>
            <p class="text-[12px] text-[#8e9098]">Sign in this device</p>
          </div>
        </div>

        @if (approving()) {
          <p
            class="mb-4 rounded-lg border border-[#4a668a]/40 bg-[#4a668a]/10 px-3 py-2 text-[12px] text-[#c9d6e8]"
            data-testid="approve-hint"
          >
            A CLI is asking to sign in. Sign this browser in first, then approve it.
          </p>
        }

        <ol class="space-y-2.5 text-[12.5px] leading-relaxed text-[#c4c5ca] list-none p-0 m-0">
          <li class="flex gap-2.5">
            <span class="text-[#8e9098] font-mono-ide">1</span
            ><span
              >Open the <b class="text-[#ececed] font-medium">Sign in</b> link that
              <code class="font-mono-ide text-[11.5px] text-[#ececed]">syre serve</code> printed,
              or scan its QR code.</span
            >
          </li>
          <li class="flex gap-2.5">
            <span class="text-[#8e9098] font-mono-ide">2</span
            ><span
              >Need a new one? Run
              <code class="font-mono-ide text-[11.5px] text-[#ececed]">syre pair</code> on the
              hub, or use <b class="text-[#ececed] font-medium">Add a device</b> on a signed-in
              device.</span
            >
          </li>
        </ol>

        <form (ngSubmit)="useCode()" class="mt-5 space-y-2" aria-label="Sign in with a code">
          <label for="pair-code" class="block text-[11px] font-medium text-[#8e9098]"
            >Have a code?</label
          >
          <div class="flex gap-2">
            <input
              id="pair-code"
              name="code"
              [ngModel]="code()"
              (ngModelChange)="code.set($event)"
              placeholder="XXXX-XXXX"
              autocomplete="one-time-code"
              autocapitalize="characters"
              spellcheck="false"
              maxlength="12"
              class="min-w-0 flex-1 rounded-lg border border-[#2c2e36] bg-[#111214] px-3 py-2 text-[14px] tracking-[0.2em] uppercase text-[#ececed] placeholder-[#5e6068] font-mono-ide focus:border-[#4a668a] focus:outline-none"
            />
            <button
              type="submit"
              [disabled]="!validCode()"
              class="rounded-lg bg-[#ececed] hover:bg-white disabled:opacity-40 px-4 py-2 text-[13px] font-medium text-[#111214] cursor-pointer"
            >
              Continue
            </button>
          </div>
        </form>

        @if (auth.error()) {
          <div
            role="alert"
            class="mt-4 rounded-lg border border-[#e05d5d]/30 bg-[#e05d5d]/10 px-3 py-2 text-[12px] text-[#f09595]"
          >
            {{ auth.error() }}
          </div>
        }

        <div class="mt-5 border-t border-[#26282e] pt-4">
          @if (!showRecovery()) {
            <button
              type="button"
              (click)="showRecovery.set(true)"
              class="text-[12px] text-[#8e9098] hover:text-[#ececed] underline-offset-2 hover:underline cursor-pointer"
            >
              Use recovery key
            </button>
          } @else {
            <form
              (ngSubmit)="submitRecovery()"
              class="space-y-3"
              aria-label="Sign in with the recovery key"
            >
              <div>
                <label for="token" class="block text-[11px] font-medium text-[#8e9098] mb-1.5"
                  >Recovery key</label
                >
                <input
                  id="token"
                  type="password"
                  name="token"
                  [ngModel]="token()"
                  (ngModelChange)="token.set($event)"
                  autocomplete="current-password"
                  class="w-full rounded-lg border border-[#2c2e36] bg-[#111214] px-3 py-2 text-[13px] text-[#ececed] placeholder-[#5e6068] font-mono-ide focus:border-[#4a668a] focus:outline-none"
                />
                <p class="mt-1.5 text-[11px] leading-snug text-[#7c7e86]">
                  For emergencies. It is <code class="font-mono-ide">recovery_key</code> in the
                  hub's <code class="font-mono-ide">syre-tokens.yaml</code>.
                </p>
              </div>
              <button
                type="submit"
                [disabled]="submitting() || !token().trim()"
                class="w-full rounded-lg border border-[#2c2e36] hover:border-[#4a4d57] disabled:opacity-40 px-4 py-2 text-[13px] font-medium text-[#ececed] transition cursor-pointer"
              >
                {{ submitting() ? 'Signing in...' : 'Sign In' }}
              </button>
            </form>
          }
        </div>
      </div>
    </main>
  `,
})
export class LoginViewComponent {
  readonly auth = inject(AuthService);
  /** Emits a typed code; the app shows the pairing page for it. */
  readonly pairCode = output<string>();

  readonly code = signal('');
  readonly token = signal('');
  readonly submitting = signal(false);
  readonly showRecovery = signal(false);
  readonly approving = signal(
    typeof location !== 'undefined' && location.pathname.startsWith('/approve/'),
  );

  validCode(): boolean {
    return normalizeCode(this.code()) !== '';
  }

  useCode(): void {
    const c = normalizeCode(this.code());
    if (c) this.pairCode.emit(c);
  }

  async submitRecovery(): Promise<void> {
    if (!this.token().trim()) return;
    this.submitting.set(true);
    try {
      await this.auth.login(this.token().trim());
    } finally {
      this.submitting.set(false);
    }
  }
}

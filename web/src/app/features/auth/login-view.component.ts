import { Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AuthService } from '../../core/services/auth.service';

@Component({
  selector: 'app-login-view',
  standalone: true,
  imports: [FormsModule],
  template: `
    <div class="h-screen w-screen flex items-center justify-center bg-[#111214] px-4">
      <div class="w-full max-w-sm rounded-xl border border-[#26282e] bg-[#18191c] p-6 shadow-2xl">
        <div class="flex items-center gap-3 mb-6">
          <img src="icon.svg" alt="Konsert" class="h-9 w-9 rounded-lg" />
          <div>
            <h1 class="text-[15px] font-semibold tracking-tight text-[#ececed]">Konsert</h1>
            <p class="text-[12px] text-[#8e9098]">Distributed Agent Control Plane</p>
          </div>
        </div>

        <form (ngSubmit)="submit()" class="space-y-4">
          <div>
            <label for="token" class="block text-[11px] font-medium text-[#8e9098] mb-1.5">
              Admin Token
            </label>
            <input
              id="token"
              type="password"
              name="token"
              [ngModel]="token()"
              (ngModelChange)="token.set($event)"
              placeholder="Enter controller admin token..."
              autocomplete="current-password"
              class="w-full rounded-lg border border-[#2c2e36] bg-[#111214] px-3 py-2 text-[13px] text-[#ececed] placeholder-[#5e6068] font-mono-ide focus:border-[#4a668a] focus:outline-none"
            />
          </div>

          @if (auth.error()) {
            <div class="rounded-lg border border-[#e05d5d]/30 bg-[#e05d5d]/10 px-3 py-2 text-[12px] text-[#f09595]">
              {{ auth.error() }}
            </div>
          }

          <button
            type="submit"
            [disabled]="submitting() || !token().trim()"
            class="w-full rounded-lg bg-[#ececed] hover:bg-white disabled:opacity-40 px-4 py-2 text-[13px] font-medium text-[#111214] transition cursor-pointer"
          >
            {{ submitting() ? 'Signing in...' : 'Sign In' }}
          </button>
        </form>
      </div>
    </div>
  `,
})
export class LoginViewComponent {
  readonly auth = inject(AuthService);
  readonly token = signal('');
  readonly submitting = signal(false);

  async submit(): Promise<void> {
    if (!this.token().trim()) return;
    this.submitting.set(true);
    try {
      await this.auth.login(this.token().trim());
    } finally {
      this.submitting.set(false);
    }
  }
}

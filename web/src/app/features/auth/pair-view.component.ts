import { Component, inject, input, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AuthService } from '../../core/services/auth.service';
import { defaultDeviceName } from './device-name';

/**
 * /pair/{code}: name this device and sign in with a one-time pairing code. On success
 * the hub sets this browser's own session cookie and the app opens.
 */
@Component({
  selector: 'app-pair-view',
  standalone: true,
  imports: [FormsModule],
  template: `
    <main class="min-h-dvh w-screen flex items-center justify-center bg-[#111214] px-4 py-8">
      <form
        (ngSubmit)="submit()"
        class="w-full max-w-sm rounded-xl border border-[#26282e] bg-[#18191c] p-6 shadow-2xl space-y-5"
        aria-labelledby="pair-title"
      >
        <div class="flex items-center gap-3">
          <img src="logo.svg" alt="" class="h-9 w-9 rounded-lg" />
          <div>
            <h1 id="pair-title" class="text-[15px] font-semibold tracking-tight text-[#ececed]">
              Sign in to Syre
            </h1>
            <p class="text-[12px] text-[#8e9098]">
              Code
              <span
                class="font-mono-ide tracking-[0.15em] text-[#ececed]"
                data-testid="pair-code"
                >{{ code() }}</span
              >
            </p>
          </div>
        </div>

        <div>
          <label for="device-name" class="block text-[11px] font-medium text-[#8e9098] mb-1.5"
            >Name this device</label
          >
          <input
            id="device-name"
            name="name"
            [ngModel]="name()"
            (ngModelChange)="name.set($event)"
            maxlength="60"
            autocomplete="off"
            class="w-full rounded-lg border border-[#2c2e36] bg-[#111214] px-3 py-2 text-[14px] text-[#ececed] focus:border-[#4a668a] focus:outline-none"
          />
          <p class="mt-1.5 text-[11px] leading-snug text-[#7c7e86]">
            Shown in Settings → Devices, where you can revoke it. It stays signed in while you use
            it at least every few months.
          </p>
        </div>

        @if (auth.error()) {
          <div
            role="alert"
            class="rounded-lg border border-[#e05d5d]/30 bg-[#e05d5d]/10 px-3 py-2 text-[12px] text-[#f09595]"
          >
            {{ auth.error() }}
          </div>
        }

        <div class="flex items-center gap-3">
          <button
            type="submit"
            [disabled]="submitting() || !name().trim()"
            class="flex-1 rounded-lg bg-[#ececed] hover:bg-white disabled:opacity-40 px-4 py-2 text-[13px] font-medium text-[#111214] transition cursor-pointer"
          >
            {{ submitting() ? 'Signing in...' : 'Sign in' }}
          </button>
          <button
            type="button"
            (click)="cancel.emit()"
            class="text-[12px] text-[#8e9098] hover:text-[#ececed] cursor-pointer"
          >
            Cancel
          </button>
        </div>
      </form>
    </main>
  `,
})
export class PairViewComponent {
  readonly auth = inject(AuthService);
  readonly code = input.required<string>();
  readonly paired = output<void>();
  readonly cancel = output<void>();

  readonly name = signal(defaultDeviceName());
  readonly submitting = signal(false);

  constructor() {
    this.auth.error.set('');
  }

  async submit(): Promise<void> {
    if (!this.name().trim()) return;
    this.submitting.set(true);
    try {
      if (await this.auth.pair(this.code(), this.name().trim())) this.paired.emit();
    } finally {
      this.submitting.set(false);
    }
  }
}

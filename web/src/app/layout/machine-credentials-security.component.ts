import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { RotationResponse } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';

/**
 * Settings → Security: "Rotate machine credentials" (KON-W1YRP). Machines that joined by
 * hand lose their credential at once and are listed as needing a new join code;
 * machines the hub installs itself are not affected.
 */
@Component({
  selector: 'app-machine-credentials-security',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="rounded-[var(--ks-radius-lg)] bg-black/20 p-4 space-y-3" data-testid="security-machine-credentials">
      <div class="text-[11px] font-semibold uppercase tracking-wider text-[#8e9098]">Security</div>
      <div class="flex items-start justify-between gap-3">
        <div>
          <div class="text-[12.5px] text-[#e4e4e7]">Rotate machine credentials</div>
          <p class="text-[11.5px] text-[#8e9098] leading-relaxed mt-0.5">
            Machines that joined with a code are disconnected and need a new code. Machines the hub installs itself are
            not affected.
          </p>
        </div>
        @if (!confirming()) {
          <button type="button" class="ks-btn ks-btn-sm ks-btn-danger shrink-0" (click)="confirming.set(true)" data-testid="rotate-machine-credentials">
            Rotate
          </button>
        }
      </div>
      @if (confirming()) {
        <div class="rounded-md bg-[#e05d5d]/10 p-2.5 space-y-2" role="alertdialog" aria-label="Confirm rotating machine credentials">
          <p class="text-[11.5px] text-[#e6b8b8]">Disconnect every machine that joined with a code until it joins again?</p>
          <div class="flex justify-end gap-2">
            <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" (click)="confirming.set(false)" [disabled]="busy()">Cancel</button>
            <button
              type="button"
              class="rounded-md bg-[#c94a4a] hover:bg-[#d85555] disabled:opacity-50 px-3 py-1 text-[11px] font-medium text-white cursor-pointer"
              (click)="rotate()"
              [disabled]="busy()"
              data-testid="confirm-rotate-machine-credentials"
            >
              {{ busy() ? 'Rotating...' : 'Yes, rotate' }}
            </button>
          </div>
        </div>
      }
      @if (error()) {
        <div role="alert" class="rounded-md bg-[#e05d5d]/10 px-3 py-2 text-[11.5px] text-[#f09595]">{{ error() }}</div>
      }
      @if (result(); as r) {
        <div class="space-y-1.5 text-[11.5px]" role="status" data-testid="rotation-result">
          @if (r.results.length === 0) {
            <div class="text-[#8fd49d]">No machines joined with a code; nothing to rotate.</div>
          } @else {
            <div class="text-[#c8cad0]">
              {{ r.needs_join }} machine(s) need a new join code (Add machine → Manual){{ r.failed ? ', ' + r.failed + ' failed' : '' }}:
            </div>
            <ul class="space-y-1">
              @for (m of r.results; track m.worker_id) {
                <li class="flex items-center justify-between gap-2 rounded bg-[#141518] px-2.5 py-1.5">
                  <span class="text-[#e4e4e7] truncate">{{ m.name }} <span class="font-mono-ide text-[#86888f]">{{ m.worker_sid }}</span></span>
                  <span [class.text-[#f09595]]="m.action === 'failed'" [class.text-[#d9a043]]="m.action !== 'failed'">
                    {{ m.action === 'failed' ? 'failed: ' + m.error : 'needs a new join code' }}
                  </span>
                </li>
              }
            </ul>
          }
        </div>
      }
    </div>
  `,
})
export class MachineCredentialsSecurityComponent {
  private readonly api = inject(ApiClientService);
  readonly confirming = signal(false);
  readonly busy = signal(false);
  readonly error = signal('');
  readonly result = signal<RotationResponse | null>(null);

  async rotate(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      this.result.set(await this.api.rotateMachineCredentials());
      this.confirming.set(false);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Rotation failed');
    } finally {
      this.busy.set(false);
    }
  }
}

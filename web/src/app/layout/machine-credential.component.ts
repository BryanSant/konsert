import { ChangeDetectionStrategy, Component, OnInit, inject, input, signal } from '@angular/core';
import { MachineCredentialInfo, Worker } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';

const SOURCE_LABELS: Record<string, string> = {
  join: 'joined with a code',
  local: 'installed by the hub',
  ssh: 'installed over ssh',
};

/** "3m ago" style relative time. */
export function ago(iso: string | undefined, now = Date.now()): string {
  if (!iso) return 'never';
  const t = new Date(iso).getTime();
  if (isNaN(t)) return iso;
  const s = Math.max(0, Math.floor((now - t) / 1000));
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

/**
 * Fleet → machine Details: the machine's own credential (KON-RK9YD) and "Revoke
 * credential", which disconnects it at once; it can join again with a new code.
 */
@Component({
  selector: 'app-machine-credential',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section class="rounded-lg bg-[#141518] px-3 py-2.5 text-[11px] space-y-2" aria-labelledby="machine-credential-heading" data-testid="machine-credential">
      <div class="flex items-center justify-between gap-2">
        <h4 id="machine-credential-heading" class="text-[#86888f]">Credential</h4>
        @if (info(); as i) {
          @if (i.active.length > 0 && !confirming()) {
            <button type="button" class="ks-btn ks-btn-sm ks-btn-danger" (click)="confirming.set(true)" data-testid="revoke-credential">
              Revoke credential
            </button>
          }
        }
      </div>
      @if (info(); as i) {
        @if (i.active.length > 0) {
          @for (c of i.active; track c.id) {
            <div class="text-[#c8cad0]">
              <span class="font-mono-ide">{{ c.sid }}</span>
              <span class="text-[#86888f]"> · {{ sourceLabel(c.source) }} · issued {{ ago(c.created_at) }} · last used {{ ago(c.last_used_at) }}</span>
            </div>
          }
        } @else {
          <div class="text-[#c8cad0]" data-testid="credential-none">
            No active credential.
            @if (worker().launch === 'local' || worker().launch === 'ssh') {
              The hub issues a new one when it starts this machine.
            } @else {
              Join it again with a code from Add machine → Manual.
            }
          </div>
        }
      } @else if (error()) {
        <div class="text-[#f09595]">{{ error() }}</div>
      } @else {
        <div class="text-[#86888f]">Loading...</div>
      }
      @if (confirming()) {
        <div class="rounded-md bg-[#e05d5d]/10 p-2.5 space-y-2" role="alertdialog" aria-label="Confirm revoking the credential">
          <p class="text-[11.5px] text-[#e6b8b8] leading-relaxed">
            Revoke the credential of "{{ worker().name }}"? Its konsertd is disconnected right away and stays out until
            it joins again with a new code.
          </p>
          <div class="flex justify-end gap-2">
            <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" (click)="confirming.set(false)" [disabled]="busy()">Cancel</button>
            <button
              type="button"
              class="rounded-md bg-[#c94a4a] hover:bg-[#d85555] disabled:opacity-50 px-3 py-1 text-[11px] font-medium text-white cursor-pointer"
              (click)="revoke()"
              [disabled]="busy()"
              data-testid="confirm-revoke-credential"
            >
              {{ busy() ? 'Revoking...' : 'Yes, revoke' }}
            </button>
          </div>
        </div>
      }
      @if (notice()) {
        <div class="text-[#98dba5]" role="status">{{ notice() }}</div>
      }
    </section>
  `,
})
export class MachineCredentialComponent implements OnInit {
  readonly worker = input.required<Worker>();
  private readonly api = inject(ApiClientService);

  readonly info = signal<MachineCredentialInfo | null>(null);
  readonly error = signal('');
  readonly confirming = signal(false);
  readonly busy = signal(false);
  readonly notice = signal('');
  readonly ago = ago;

  ngOnInit(): void {
    void this.load();
  }

  sourceLabel(src: string): string {
    return SOURCE_LABELS[src] ?? src;
  }

  async load(): Promise<void> {
    try {
      this.info.set(await this.api.getMachineCredential(this.worker().id));
      this.error.set('');
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not load the credential');
    }
  }

  async revoke(): Promise<void> {
    this.busy.set(true);
    try {
      await this.api.revokeMachineCredential(this.worker().id);
      this.confirming.set(false);
      this.notice.set('Revoked. The machine was disconnected.');
      await this.load();
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not revoke the credential');
    } finally {
      this.busy.set(false);
    }
  }
}

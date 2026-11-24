import { ChangeDetectionStrategy, Component, OnDestroy, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { JoinCode, JoinCodeStatus } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { FleetStore } from '../core/state/fleet.store';
import { IconComponent } from '../shared/icon.component';

/** How often the Manual tab checks whether its code was used. */
const POLL_MS = 2000;

/** The command a manual machine runs: `konsertd join <hub> <code>` plus any options. */
export function joinCommand(code: JoinCode, opts: { sandbox: boolean; enableMock: boolean }): string {
  const parts = [code.command];
  if (opts.sandbox) parts.push('--sandbox');
  if (opts.enableMock) parts.push('--enable-mock-driver');
  return parts.join(' ');
}

/** "14 min" / "45 s" until expiry; "" once expired. */
export function expiresIn(expiresAt: string, now: number): string {
  const ms = new Date(expiresAt).getTime() - now;
  if (!(ms > 0)) return '';
  const s = Math.ceil(ms / 1000);
  return s >= 60 ? `${Math.ceil(s / 60)} min` : `${s} s`;
}

/**
 * Add machine → Manual (KON-RK9YD): one command with a short-lived, single-use join
 * code. The machine trades it for its own credential; this tab follows it until it is
 * online.
 */
@Component({
  selector: 'app-manual-join',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [FormsModule, IconComponent],
  template: `
    <div class="space-y-3 text-[12px]" data-testid="manual-join">
      <p class="text-[11.5px] text-[#8e9098] leading-relaxed">
        For containers, Windows, or machines the hub cannot reach over ssh. Run this on the machine; the code works
        once, for 15 minutes. It installs <code class="font-mono-ide text-[#b4b6be]">konsertd</code> to start at login
        (add <code class="font-mono-ide text-[#b4b6be]">--foreground</code> to run it in that terminal only).
      </p>

      <div class="flex items-center gap-3 text-[11px]">
        <label class="inline-flex items-center gap-1.5 text-[#b4b6be] cursor-pointer">
          <input type="checkbox" [ngModel]="sandbox()" (ngModelChange)="sandbox.set($event)" class="accent-[#4a668a]" />
          <span>Opt-in sandboxed mode</span>
        </label>
        <label class="inline-flex items-center gap-1.5 text-[#b4b6be] cursor-pointer">
          <input type="checkbox" [ngModel]="enableMock()" (ngModelChange)="enableMock.set($event)" class="accent-[#4a668a]" />
          <span>Include mock driver</span>
        </label>
      </div>

      @if (error()) {
        <div role="alert" class="rounded-lg bg-[#e05d5d]/10 px-3 py-2.5 text-[11.5px] text-[#f09595] break-words">{{ error() }}</div>
      }

      @if (code(); as jc) {
        <div class="rounded-lg bg-[#111214] p-3 space-y-2">
          <div class="flex items-center justify-between text-[11px] text-[#8a8c94]">
            <span>
              Join command ·
              @if (status() === 'expired') {
                <span class="text-[#f09595]" data-testid="join-expiry">expired</span>
              } @else if (status() === 'used') {
                <span data-testid="join-expiry">used</span>
              } @else {
                <span data-testid="join-expiry">expires in {{ remaining() }}</span>
              }
            </span>
            <button
              type="button"
              (click)="copy()"
              [disabled]="status() !== 'pending'"
              class="inline-flex items-center gap-1 rounded px-2 py-0.5 bg-[#202228] hover:bg-[#2a2c34] active:scale-95 text-[#d4d4d8] transition cursor-pointer disabled:opacity-50"
            >
              <app-icon name="copy" [size]="11" />
              <span>{{ copied() ? 'Copied' : 'Copy' }}</span>
            </button>
          </div>
          <pre
            class="font-mono-ide text-[11.5px] text-[#e4e4e7] whitespace-pre-wrap break-all"
            [class.opacity-50]="status() !== 'pending'"
            data-testid="join-command"
          >{{ command() }}</pre>
        </div>

        @if (joined(); as m) {
          <div class="flex items-center justify-between gap-3 rounded-lg bg-[#141518] px-3 py-2.5" data-testid="joined-machine">
            <div class="flex items-center gap-2 min-w-0">
              <app-icon name="server" [size]="14" class="text-[#8a8c94]" />
              <span class="text-[12.5px] font-medium text-[#e4e4e7] truncate">{{ m.name }}</span>
              <span class="font-mono-ide text-[11px] text-[#86888f]">{{ m.sid }}</span>
            </div>
            @if (m.online) {
              <span class="inline-flex items-center gap-1.5 text-[11px] text-[#8fd49d]" data-testid="joined-state">
                <app-icon name="check-circle" [size]="13" />
                <span>Online</span>
              </span>
            } @else {
              <span class="inline-flex items-center gap-1.5 text-[11px] text-[#c8cad0]" data-testid="joined-state">
                <span class="h-2.5 w-2.5 rounded-full border-[1.5px] border-[#60a5fa] border-t-transparent ks-ring-spin"></span>
                <span>Joined · connecting...</span>
              </span>
            }
          </div>
        } @else if (status() === 'pending') {
          <div class="flex items-center gap-2 text-[11.5px] text-[#86888f]">
            <span class="h-2.5 w-2.5 rounded-full border-[1.5px] border-[#60a5fa] border-t-transparent ks-ring-spin"></span>
            <span>Waiting for the machine to join...</span>
          </div>
        }

        @if (status() !== 'pending') {
          <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" (click)="newCode()" [disabled]="busy()" data-testid="new-join-code">
            {{ status() === 'used' ? 'Add another machine' : 'New code' }}
          </button>
        }
      } @else if (busy()) {
        <div class="rounded-lg bg-[#151619] px-3 py-3 text-[12px] text-[#86888f]">Creating a join code...</div>
      }
    </div>
  `,
})
export class ManualJoinComponent implements OnInit, OnDestroy {
  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);

  readonly sandbox = signal(false);
  readonly enableMock = signal(false);
  readonly code = signal<JoinCode | null>(null);
  readonly codeStatus = signal<JoinCodeStatus | null>(null);
  readonly busy = signal(false);
  readonly error = signal('');
  readonly copied = signal(false);
  readonly now = signal(Date.now());

  readonly command = computed(() => {
    const c = this.code();
    return c ? joinCommand(c, { sandbox: this.sandbox(), enableMock: this.enableMock() }) : '';
  });
  readonly remaining = computed(() => {
    const c = this.code();
    return c ? expiresIn(c.expires_at, this.now()) : '';
  });
  readonly status = computed<'pending' | 'used' | 'expired'>(() => {
    const st = this.codeStatus()?.status;
    if (st === 'used') return 'used';
    if (st === 'expired' || (this.code() && !this.remaining())) return 'expired';
    return 'pending';
  });
  /** The machine that joined with the code, live from the fleet. */
  readonly joined = computed(() => {
    const st = this.codeStatus();
    if (st?.status !== 'used' || !st.worker_id) return null;
    const w = this.fleet.workersById()[st.worker_id];
    return {
      name: w?.name || st.worker_name || 'machine',
      sid: w?.sid || st.worker_sid || '',
      online: w?.status === 'online',
    };
  });

  private timer: ReturnType<typeof setInterval> | null = null;

  ngOnInit(): void {
    void this.newCode();
    this.timer = setInterval(() => void this.tick(), POLL_MS);
  }

  ngOnDestroy(): void {
    if (this.timer) clearInterval(this.timer);
  }

  async newCode(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set('');
    try {
      const jc = await this.api.createJoinCode();
      this.code.set(jc);
      this.codeStatus.set(null);
      this.now.set(Date.now());
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not create a join code');
    } finally {
      this.busy.set(false);
    }
  }

  private async tick(): Promise<void> {
    this.now.set(Date.now());
    const c = this.code();
    const st = this.codeStatus()?.status;
    if (!c) return;
    if (st === 'used') {
      // Follow the joined machine until it is online (the socket usually reports first).
      if (!this.joined()?.online) await this.fleet.refresh();
      return;
    }
    if (this.status() === 'expired') return;
    try {
      const next = await this.api.getJoinCode(c.id);
      if (this.code()?.id !== c.id) return;
      this.codeStatus.set(next);
      if (next.status === 'used') await this.fleet.refresh();
    } catch {
      // keep polling
    }
  }

  async copy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(this.command());
      this.copied.set(true);
      setTimeout(() => this.copied.set(false), 1800);
    } catch {
      // ignore
    }
  }
}

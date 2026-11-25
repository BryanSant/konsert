import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { ActivatedRoute, Router } from '@angular/router';
import { LoginRequest } from '../../core/models/device.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { normalizeCode } from './device-name';

const KIND_LABEL: Record<string, string> = {
  cli: 'syre CLI',
  mcp: 'MCP client',
  browser: 'browser',
  phone: 'phone',
};

/**
 * /approve/{code}: `syre login` on another machine asks to sign in. A signed-in user
 * checks the name and code match the terminal and approves (or denies) it.
 */
@Component({
  selector: 'sy-approve-login',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'block min-h-dvh w-screen bg-[#111214]' },
  template: `
    <main class="min-h-dvh flex items-center justify-center px-4 py-8">
      <section
        class="w-full max-w-sm rounded-xl border border-[#26282e] bg-[#18191c] p-6 shadow-2xl space-y-5"
        aria-labelledby="approve-title"
      >
        <div class="flex items-center gap-3">
          <img src="logo.svg" alt="" class="h-9 w-9 rounded-lg" />
          <h1 id="approve-title" class="text-[15px] font-semibold tracking-tight text-[#ececed]">
            Approve sign-in?
          </h1>
        </div>

        @if (error()) {
          <div
            role="alert"
            class="rounded-lg border border-[#e05d5d]/30 bg-[#e05d5d]/10 px-3 py-2 text-[12px] text-[#f09595]"
          >
            {{ error() }}
          </div>
        }

        @if (req(); as r) {
          <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-[12.5px] m-0">
            <dt class="text-[#8e9098]">Device</dt>
            <dd class="m-0 text-[#ececed] font-medium" data-testid="approve-name">{{ r.name }}</dd>
            <dt class="text-[#8e9098]">Kind</dt>
            <dd class="m-0 text-[#c4c5ca]">{{ kindLabel(r.kind) }}</dd>
            @if (r.created_ip) {
              <dt class="text-[#8e9098]">From</dt>
              <dd class="m-0 text-[#c4c5ca] font-mono-ide">{{ r.created_ip }}</dd>
            }
            <dt class="text-[#8e9098]">Code</dt>
            <dd class="m-0 text-[#ececed] font-mono-ide tracking-[0.15em]">{{ displayCode() }}</dd>
          </dl>

          @switch (r.status) {
            @case ('pending') {
              <p class="text-[12px] leading-snug text-[#a1a3ab]">
                Approve only if you just ran <code class="font-mono-ide">syre login</code> and
                the code above matches your terminal. The device gets its own credential, which you
                can revoke in Settings → Devices.
              </p>
              <div class="flex gap-2">
                <button
                  type="button"
                  class="flex-1 rounded-lg bg-[#ececed] hover:bg-white disabled:opacity-40 px-4 py-2 text-[13px] font-medium text-[#111214] cursor-pointer"
                  [disabled]="busy()"
                  (click)="decide(true)"
                >
                  Approve
                </button>
                <button
                  type="button"
                  class="rounded-lg border border-[#2c2e36] hover:border-[#4a4d57] disabled:opacity-40 px-4 py-2 text-[13px] text-[#ececed] cursor-pointer"
                  [disabled]="busy()"
                  (click)="decide(false)"
                >
                  Deny
                </button>
              </div>
            }
            @case ('approved') {
              <p role="status" class="text-[13px] text-[#8fd49d]">
                Approved. The CLI is signed in; you can close this tab.
              </p>
            }
            @case ('redeemed') {
              <p role="status" class="text-[13px] text-[#8fd49d]">
                Approved. The CLI is signed in; you can close this tab.
              </p>
            }
            @case ('denied') {
              <p role="status" class="text-[13px] text-[#f09595]">
                Denied. The CLI was not signed in.
              </p>
            }
            @default {
              <p role="status" class="text-[13px] text-[#f0c895]">
                This request expired. Run <code class="font-mono-ide">syre login</code> again.
              </p>
            }
          }
        } @else if (!error()) {
          <p class="text-[12.5px] text-[#8e9098]" role="status">Loading…</p>
        }

        <button
          type="button"
          class="text-[12px] text-[#8e9098] hover:text-[#ececed] cursor-pointer"
          (click)="home()"
        >
          Continue to Syre
        </button>
      </section>
    </main>
  `,
})
export class ApproveLoginComponent implements OnInit {
  private readonly api = inject(ApiClientService);
  private readonly router = inject(Router);
  /** The code from the route (/approve/{code}). */
  readonly code = signal(inject(ActivatedRoute).snapshot.paramMap.get('code') ?? '');

  readonly req = signal<LoginRequest | null>(null);
  readonly error = signal('');
  readonly busy = signal(false);

  displayCode(): string {
    return normalizeCode(this.code()) || this.code();
  }

  kindLabel(kind: string): string {
    return KIND_LABEL[kind] ?? kind;
  }

  async ngOnInit(): Promise<void> {
    try {
      this.req.set(await this.api.getLoginRequest(this.displayCode()));
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Unknown sign-in request');
    }
  }

  async decide(approve: boolean): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.decideLoginRequest(this.displayCode(), approve);
      this.req.update((r) => (r ? { ...r, status: approve ? 'approved' : 'denied' } : r));
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not record the decision');
      try {
        this.req.set(await this.api.getLoginRequest(this.displayCode()));
      } catch {
        // keep the error
      }
    } finally {
      this.busy.set(false);
    }
  }

  home(): void {
    void this.router.navigateByUrl('/');
  }
}

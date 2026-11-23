import {
  ChangeDetectionStrategy,
  Component,
  OnDestroy,
  OnInit,
  computed,
  inject,
  output,
  signal,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import {
  SSHBootstrapResult,
  SSHHost,
  SSHHostKey,
  SSH_STAGE_HOST_KEY_CONFIRMATION,
  Worker,
} from '../core/models/konsert.models';
import { ApiClientService, SSHBootstrapEvent } from '../core/services/api-client.service';
import { FleetStore } from '../core/state/fleet.store';
import { IconComponent } from '../shared/icon.component';
import { ModalDialogComponent } from '../shared/modal-dialog.component';

/** Matches an ~/.ssh/config host to a registered machine (by name, host group or hostname). */
export function workerMatchesHost(w: Worker, host: SSHHost): boolean {
  const alias = host.alias.toLowerCase();
  return (
    w.name.toLowerCase() === alias ||
    w.host_group.toLowerCase() === alias ||
    (!!host.hostname && w.name.toLowerCase() === host.hostname.toLowerCase())
  );
}

function needsHostKeyConfirmation(r: SSHBootstrapResult): boolean {
  return (
    !r.ok &&
    (!!r.host_key_confirmation_required || r.stage === SSH_STAGE_HOST_KEY_CONFIRMATION) &&
    (r.host_keys?.length ?? 0) > 0
  );
}

function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === 'AbortError';
}

/** Herdr-style Add Machine: SSH config discovery, one-click bootstrap, manual fallback. */

@Component({
  selector: 'app-add-machine-modal',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [FormsModule, IconComponent, ModalDialogComponent],
  template: `
    <app-modal-dialog labelledBy="add-machine-title" panelClass="w-full max-w-[540px] rounded-xl border border-[#2d3038] bg-[#1b1c20] p-5 space-y-4" (close)="close.emit()">
      <div class="flex items-start justify-between gap-3">
        <div>
          <h3 class="text-[15px] font-semibold text-[#ececed]" id="add-machine-title">Add a machine</h3>
          <p class="text-[12px] text-[#8e9098] mt-1 leading-relaxed">
            A VPS, a Mac mini, a box at home: Konsert reaches it over ssh, and agents there keep running while your laptop sleeps.
          </p>
        </div>
        <button
          type="button"
          (click)="close.emit()"
          aria-label="Close dialog"
          class="text-[11px] text-[#6d7078] hover:text-[#ececed] transition cursor-pointer shrink-0 pt-0.5"
        >
          Esc
        </button>
      </div>

      <!-- From your ssh config -->
      <div class="space-y-2">
        <div class="text-[11px] text-[#787a82]">From your ssh config</div>

        @if (sshHostsLoading()) {
          <div class="py-6 text-center text-[12px] text-[#6d7078]">Reading ~/.ssh/config...</div>
        } @else if (sshHosts().length === 0) {
          <div class="rounded-lg border border-[#26282f] bg-[#151619] px-3 py-3 text-[12px] text-[#6d7078]">
            No hosts found in <code class="font-mono-ide text-[#9a9ca4]">~/.ssh/config</code>. Enter an SSH target below or use the manual command.
          </div>
        } @else {
          <div class="max-h-[290px] overflow-y-auto -mx-1 px-1 space-y-1">
            @for (host of sshHosts(); track host.alias) {
              <div class="flex items-center justify-between gap-3 rounded-lg px-2 py-1.5 hover:bg-[#22242a]/80 transition">
                <div class="flex items-center gap-2.5 min-w-0">
                  <app-icon name="server" [size]="14" class="text-[#787a82]" />
                  <span class="text-[12.5px] font-medium text-[#e4e4e7] shrink-0">{{ host.alias }}</span>
                  <span class="text-[12px] text-[#6d7078] truncate">{{ host.display }}</span>
                </div>

                <div class="shrink-0">
                  @if (host.added) {
                    <button
                      type="button"
                      (click)="inspectSSHHostMachine(host)"
                      class="inline-flex items-center gap-1.5 rounded-md border border-transparent hover:border-[#32353f] hover:bg-[#24262d] px-2 py-1 text-[11px] text-[#8e9098] hover:text-[#ececed] transition cursor-pointer"
                      title="View machine details or remove"
                    >
                      <app-icon name="check-circle" [size]="13" />
                      <span>Added · Details</span>
                    </button>
                  } @else if (bootstrappingTarget() === host.alias) {
                    <span class="inline-flex items-center gap-1.5 rounded-md border border-[#32353f] bg-[#24262d] px-2.5 py-1 text-[11px] text-[#c8cad0]">
                      <span class="h-2.5 w-2.5 rounded-full border-[1.5px] border-[#60a5fa] border-t-transparent ks-ring-spin"></span>
                      <span>{{ sshStageLabel() || 'Connecting...' }}</span>
                    </span>
                  } @else {
                    <button
                      type="button"
                      [disabled]="!!bootstrappingTarget()"
                      (click)="bootstrapSSHHost(host.alias)"
                      class="rounded-md border border-[#32353f] bg-[#24262d] hover:bg-[#2d3039] disabled:opacity-45 px-3 py-1 text-[11px] font-medium text-[#e4e4e7] transition cursor-pointer"
                    >
                      Add
                    </button>
                  }
                </div>
              </div>
            }
          </div>
        }
      </div>

      <!-- Or type how ssh reaches it -->
      <div class="pt-1 space-y-1.5">
        <div class="text-[11px] text-[#787a82]">Or type how ssh reaches it</div>
        <form (ngSubmit)="bootstrapCustomSSH()" class="flex items-center gap-2">
          <input
            type="text"
            name="customSshTarget"
            [ngModel]="customSshTarget()"
            (ngModelChange)="customSshTarget.set($event)"
            placeholder="user@host"
            autocomplete="off"
            spellcheck="false"
            class="flex-1 rounded-lg border border-[#383c48] focus:border-[#4a668a] bg-[#141518] px-3 py-2 text-[12.5px] text-[#ececed] placeholder-[#5e6068] focus:outline-none transition"
          />
          <button
            type="submit"
            [disabled]="!customSshTarget().trim() || !!bootstrappingTarget()"
            class="rounded-lg border border-[#32353f] bg-[#2a2c34] hover:bg-[#343741] disabled:opacity-45 px-4 py-2 text-[12px] font-medium text-[#e4e4e7] transition cursor-pointer shrink-0"
          >
            {{ bootstrappingTarget() && bootstrappingTarget() === customSshTarget().trim() ? 'Adding...' : 'Add' }}
          </button>
        </form>
      </div>

      @if (bootstrappingTarget() && sshStageDetail()) {
        <div class="rounded-lg border border-[#2d3039] bg-[#141518] px-3 py-2 flex items-center gap-2 text-[11.5px] text-[#b4b6be]">
          <span class="h-2.5 w-2.5 rounded-full border-[1.5px] border-[#60a5fa] border-t-transparent ks-ring-spin shrink-0"></span>
          <span class="font-medium text-[#e4e4e7]">{{ sshStageLabel() }}:</span>
          <span class="truncate">{{ sshStageDetail() }}</span>
        </div>
      }
      @if (sshStatusMsg()) {
        <div class="rounded-lg border border-[#69b879]/30 bg-[#69b879]/10 px-3 py-2 text-[11.5px] text-[#98dba5]">
          {{ sshStatusMsg() }}
        </div>
      }
      @if (hostKeyPrompt(); as prompt) {
        <div
          role="alertdialog"
          aria-labelledby="ks-hostkey-title"
          aria-describedby="ks-hostkey-desc"
          class="rounded-lg border border-[#d9a043]/40 bg-[#d9a043]/10 px-3 py-3 space-y-2.5 text-[11.5px] text-[#e8c88a]"
        >
          <div id="ks-hostkey-title" class="text-[12.5px] font-semibold text-[#f4d9a4]">
            Verify the host key for {{ prompt.target }}
          </div>
          <p id="ks-hostkey-desc" class="leading-relaxed">
            Konsert has never connected to this machine. Before trusting it, check that a fingerprint
            below matches the one on the machine itself (for example
            <code class="font-mono-ide text-[#f4d9a4]">ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub</code>).
            If it does not match, do not continue.
          </p>
          <ul class="space-y-1.5">
            @for (key of prompt.keys; track key.fingerprint) {
              <li class="flex items-center justify-between gap-2 rounded-md border border-[#3a3428] bg-[#15140f] px-2.5 py-1.5">
                <div class="min-w-0">
                  <div class="text-[10.5px] text-[#a79a7e]">{{ key.type }}</div>
                  <code class="font-mono-ide text-[11px] text-[#f4f4f5] break-all">{{ key.fingerprint }}</code>
                </div>
                <button
                  type="button"
                  (click)="confirmHostKey(key)"
                  [attr.aria-label]="'Fingerprint matches; trust ' + key.type + ' key and continue'"
                  class="shrink-0 rounded-md border border-[#d9a043]/50 bg-[#2a2416] hover:bg-[#3a3120] px-2.5 py-1 text-[11px] font-medium text-[#f4d9a4] transition cursor-pointer"
                >
                  It matches, trust it
                </button>
              </li>
            }
          </ul>
          <div class="flex justify-end">
            <button type="button" (click)="hostKeyPrompt.set(null)" class="rounded-md px-2.5 py-1 text-[11px] text-[#a1a3ab] hover:text-[#ececed] cursor-pointer">
              Cancel
            </button>
          </div>
        </div>
      }
      @if (sshErrorMsg()) {
        @if (hostKeyChanged()) {
          <div role="alert" class="rounded-lg border-2 border-[#e05d5d] bg-[#e05d5d]/15 px-3 py-2.5 text-[12px] text-[#ffb4b4] break-words space-y-1">
            <div class="text-[13px] font-semibold text-[#ffd0d0]">Host key changed: possible man-in-the-middle</div>
            <div>{{ sshErrorMsg() }}</div>
          </div>
        } @else {
          <div role="alert" class="rounded-lg border border-[#e05d5d]/30 bg-[#e05d5d]/10 px-3 py-2 text-[11.5px] text-[#f09595] break-words">
            {{ sshErrorMsg() }}
          </div>
        }
      }

      <!-- Collapsible manual konsertd command for containers / Windows / no-SSH hosts -->
      <div class="pt-1 border-t border-[#26282f]">
        <button
          type="button"
          (click)="showManualConnect.set(!showManualConnect())"
          class="inline-flex items-center gap-1.5 text-[11px] text-[#787a82] hover:text-[#c8cad0] transition cursor-pointer"
        >
          <app-icon [name]="showManualConnect() ? 'chevron-down' : 'chevron-right'" [size]="12" />
          <span>Manual konsertd command (containers, Windows, or outbound-only hosts)</span>
        </button>

        @if (showManualConnect()) {
          <div class="mt-2.5 space-y-2.5 text-[12px]">
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

            <div class="rounded-lg border border-[#2b2d35] bg-[#111214] p-3 space-y-2">
              <div class="flex items-center justify-between text-[11px] text-[#787a82]">
                <span>Connect command</span>
                <button
                  type="button"
                  (click)="copyConnectCmd()"
                  class="inline-flex items-center gap-1 rounded px-2 py-0.5 bg-[#202228] hover:bg-[#2a2c34] active:scale-95 text-[#d4d4d8] transition cursor-pointer"
                >
                  <app-icon name="copy" [size]="11" />
                  <span>{{ copiedCmd() ? 'Copied' : 'Copy' }}</span>
                </button>
              </div>
              <pre class="font-mono-ide text-[11px] text-[#e4e4e7] whitespace-pre-wrap break-all">{{ connectCommand() }}</pre>
            </div>
          </div>
        }
      </div>
    </app-modal-dialog>
  `,
})
export class AddMachineModalComponent implements OnInit, OnDestroy {
  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);

  readonly close = output<void>();
  /** User asked to see an already-added machine. */
  readonly inspectMachine = output<string>();

  readonly sshHosts = signal<SSHHost[]>([]);
  readonly sshHostsLoading = signal(false);
  readonly customSshTarget = signal('');
  readonly bootstrappingTarget = signal('');
  readonly sshStageLabel = signal('');
  readonly sshStageDetail = signal('');
  readonly sshStatusMsg = signal('');
  readonly sshErrorMsg = signal('');
  readonly showManualConnect = signal(false);
  readonly sandbox = signal(false);
  readonly enableMock = signal(false);
  readonly copiedCmd = signal(false);
  /** Unknown host key awaiting the operator's fingerprint confirmation. */
  readonly hostKeyPrompt = signal<{ target: string; keys: SSHHostKey[] } | null>(null);
  /** The hub refuses changed host keys outright; flag that error prominently. */
  readonly hostKeyChanged = computed(() => /host key .* has changed/i.test(this.sshErrorMsg()));

  /** Aborts an in-flight bootstrap stream when the modal closes. */
  private bootstrapAbort: AbortController | null = null;
  private readonly timers = new Set<ReturnType<typeof setTimeout>>();

  readonly connectCommand = computed(() => {
    const parts = [`konsertd run --controller ${workerControllerUrl()}`];
    parts.push('--token "$KONSERT_FLEET_TOKEN"');
    if (this.sandbox()) parts.push('--sandbox');
    if (this.enableMock()) parts.push('--enable-mock-driver');
    return parts.join(' ');
  });

  async ngOnInit(): Promise<void> {
    this.sshHostsLoading.set(true);
    try {
      const res = await this.api.listSSHHosts();
      this.sshHosts.set(res.hosts ?? []);
    } catch {
      this.sshHosts.set([]);
    } finally {
      this.sshHostsLoading.set(false);
    }
  }

  ngOnDestroy(): void {
    this.bootstrapAbort?.abort();
    for (const t of this.timers) clearTimeout(t);
  }

  inspectSSHHostMachine(host: SSHHost): void {
    const match = this.fleet.workers().find((w) => workerMatchesHost(w, host));
    if (match) {
      this.inspectMachine.emit(match.id);
    }
  }

  async bootstrapSSHHost(target: string, acceptHostKey?: string): Promise<void> {
    const clean = target.trim();
    if (!clean || this.bootstrappingTarget()) return;
    this.hostKeyPrompt.set(null);
    this.bootstrappingTarget.set(clean);
    this.sshStageLabel.set('Connecting');
    this.sshStageDetail.set(`Reaching ${clean} over SSH...`);
    this.sshStatusMsg.set('');
    this.sshErrorMsg.set('');
    const abort = new AbortController();
    this.bootstrapAbort = abort;
    try {
      const result = await this.api.bootstrapSSH(
        {
          target: clean,
          controller_url: workerControllerUrl(),
          sandbox: this.sandbox(),
          enable_mock: this.enableMock(),
          accept_host_key: acceptHostKey || undefined,
        },
        { signal: abort.signal, onEvent: (ev) => this.onBootstrapEvent(ev) },
      );
      if (needsHostKeyConfirmation(result)) {
        this.hostKeyPrompt.set({ target: clean, keys: result.host_keys ?? [] });
        return;
      }
      if (!result.ok) {
        const prefix = result.stage ? `[${result.stage}] ` : '';
        this.sshErrorMsg.set(`${prefix}${result.error || `Failed to add ${clean}`}`);
        return;
      }
      this.sshStatusMsg.set(result.message || `konsertd ready on ${clean}.`);
      this.sshHosts.update((list) =>
        list.map((h) => (h.alias === clean || h.display === clean ? { ...h, added: true } : h)),
      );
      this.later(() => void this.fleet.refresh(), 1200);
    } catch (err) {
      if (isAbort(err)) return;
      this.sshErrorMsg.set(err instanceof Error ? err.message : `SSH bootstrap failed for ${clean}`);
    } finally {
      if (this.bootstrapAbort === abort) this.bootstrapAbort = null;
      this.bootstrappingTarget.set('');
      this.sshStageLabel.set('');
      this.sshStageDetail.set('');
    }
  }

  /** Operator confirmed a fingerprint: retry the same bootstrap trusting that key. */
  async confirmHostKey(key: SSHHostKey): Promise<void> {
    const prompt = this.hostKeyPrompt();
    if (!prompt) return;
    await this.bootstrapSSHHost(prompt.target, key.fingerprint);
  }

  /** Handles one streamed bootstrap frame. Extend here for new interactive steps. */
  private onBootstrapEvent(ev: SSHBootstrapEvent): void {
    switch (ev.type) {
      case 'stage': {
        const stage = ev as { label?: string; detail?: string };
        this.sshStageLabel.set(stage.label || 'Working');
        this.sshStageDetail.set(stage.detail || '');
        break;
      }
      default:
        // 'done' is consumed by ApiClientService; unknown frames are ignored for now.
        break;
    }
  }

  async bootstrapCustomSSH(): Promise<void> {
    const target = this.customSshTarget().trim();
    if (!target) return;
    await this.bootstrapSSHHost(target);
    if (!this.sshErrorMsg() && !this.hostKeyPrompt()) {
      this.customSshTarget.set('');
    }
  }

  async copyConnectCmd(): Promise<void> {
    try {
      await navigator.clipboard.writeText(this.connectCommand());
      this.copiedCmd.set(true);
      this.later(() => this.copiedCmd.set(false), 1800);
    } catch {
      // ignore
    }
  }

  private later(fn: () => void, ms: number): void {
    const t = setTimeout(() => {
      this.timers.delete(t);
      fn();
    }, ms);
    this.timers.add(t);
  }
}

/** WebSocket URL a worker daemon should dial back to (this hub). */
function workerControllerUrl(): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${window.location.host}/ws/worker`;
}

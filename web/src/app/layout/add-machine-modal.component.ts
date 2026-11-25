import {
  ChangeDetectionStrategy,
  Component,
  OnDestroy,
  OnInit,
  computed,
  inject,
  input,
  output,
  signal,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import {
  LocalHostInfo,
  SSHBootstrapResult,
  SSHHost,
  SSHHostKey,
  SSH_STAGE_HOST_KEY_CONFIRMATION,
  Worker,
} from '../core/models/syre.models';
import { ApiClientService, SSHBootstrapEvent } from '../core/services/api-client.service';
import { FleetStore } from '../core/state/fleet.store';
import { SetupStore } from '../core/state/setup.store';
import { IconComponent } from '../shared/icon.component';
import { TooltipDirective } from '../shared/tooltip.directive';
import { ModalDialogComponent } from '../shared/modal-dialog.component';
import { ManualJoinComponent } from './manual-join.component';

/** Matches an ~/.ssh/config host to a registered machine (by name, host group or hostname). */
export function workerMatchesHost(w: Worker, host: SSHHost): boolean {
  const alias = host.alias.toLowerCase();
  // A machine the hub installed over ssh is exactly its target (aliases may share a HostName).
  if (w.ssh_target) return w.ssh_target.toLowerCase() === alias;
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

/** What adding this machine will do, shown under its name. */
export function localHostSummary(local: LocalHostInfo): string {
  if (local.connected) return local.added ? 'syred is running · added' : 'syred is running — will attach';
  if (!local.syred_found) return `syred not found: ${local.fix || 'install syred next to syre or on PATH'}`;
  if (local.error) return local.error;
  if (local.launch_state === 'failed') return `Could not start syred: ${local.launch_error || 'unknown error'}`;
  return 'Will start syred (installed to start at login)';
}

function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === 'AbortError';
}

/** Herdr-style Add Machine: SSH config discovery, one-click bootstrap, manual fallback. */

@Component({
  selector: 'app-add-machine-modal',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [TooltipDirective, FormsModule, IconComponent, ModalDialogComponent, ManualJoinComponent],
  template: `
    <app-modal-dialog labelledBy="add-machine-title" panelClass="w-full max-w-[540px] rounded-[var(--sy-radius-xl)] bg-[var(--sy-surface-2)] p-6 space-y-5" (close)="close.emit()">
      <div class="flex items-start justify-between gap-3">
        <div>
          <h3 class="text-[15px] font-semibold text-[#ececed]" id="add-machine-title">
            {{ firstRun() ? 'Add your first machine' : 'Add a machine' }}
          </h3>
          <p class="text-[12px] text-[#8e9098] mt-1 leading-relaxed">
            @if (firstRun()) {
              Agents run on machines you connect. Start with this one, add others over ssh, or close this to use
              Syre as a control plane only.
            } @else {
              A VPS, a Mac mini, a box at home: Syre reaches it over ssh, or it joins with a one-time code. Agents
              there keep running while your laptop sleeps.
            }
          </p>
        </div>
        <button
          type="button"
          (click)="close.emit()"
          aria-label="Esc: close dialog"
          class="text-[11px] text-[#86888f] hover:text-[#ececed] transition cursor-pointer shrink-0 pt-0.5"
        >
          Esc
        </button>
      </div>

      <div class="flex items-center gap-1 rounded-lg bg-[#151619] p-1 text-[12px]" role="tablist" aria-label="How to add the machine">
        <button
          type="button"
          role="tab"
          id="add-machine-tab-auto"
          [attr.aria-selected]="tab() === 'auto'"
          (click)="tab.set('auto')"
          class="flex-1 rounded-md px-3 py-1.5 transition cursor-pointer"
          [class.bg-[#2a2c34]]="tab() === 'auto'"
          [class.text-[#ececed]]="tab() === 'auto'"
          [class.text-[#8a8c94]]="tab() !== 'auto'"
          data-testid="add-machine-tab-auto"
        >
          This machine &amp; ssh
        </button>
        <button
          type="button"
          role="tab"
          id="add-machine-tab-manual"
          [attr.aria-selected]="tab() === 'manual'"
          (click)="tab.set('manual')"
          class="flex-1 rounded-md px-3 py-1.5 transition cursor-pointer"
          [class.bg-[#2a2c34]]="tab() === 'manual'"
          [class.text-[#ececed]]="tab() === 'manual'"
          [class.text-[#8a8c94]]="tab() !== 'manual'"
          data-testid="add-machine-tab-manual"
        >
          Manual
        </button>
      </div>

      @if (tab() === 'manual') {
        <div role="tabpanel" aria-labelledby="add-machine-tab-manual">
          <app-manual-join />
        </div>
      } @else {
      <!-- This machine (the hub's host) -->
      <div class="space-y-2" data-testid="local-host">
        <div class="text-[11px] text-[#8a8c94]">This machine</div>
        @if (setup.loading() && !local()) {
          <div class="rounded-lg bg-[#151619] px-3 py-3 text-[12px] text-[#86888f]">Looking for syred...</div>
        } @else if (local(); as lh) {
          <div class="flex items-center justify-between gap-3 rounded-lg px-2 py-1.5 hover:bg-[#22242a]/80 transition">
            <div class="flex items-center gap-2.5 min-w-0">
              <app-icon name="monitor" [size]="14" class="text-[#8a8c94]" />
              <div class="min-w-0">
                <div class="text-[12.5px] font-medium text-[#e4e4e7] truncate">This machine ({{ lh.hostname }})</div>
                <div
                  class="text-[11.5px] truncate"
                  [class.text-[#86888f]]="lh.syred_found && !lh.error && lh.launch_state !== 'failed'"
                  [class.text-[#f09595]]="!lh.syred_found || !!lh.error || lh.launch_state === 'failed'"
                  [syTooltip]="localSummary()"
                  data-testid="local-host-state"
                >
                  {{ localSummary() }}
                </div>
              </div>
            </div>
            <div class="shrink-0">
              @if (localPhase() === 'online' || (lh.added && lh.connected)) {
                <span class="inline-flex items-center gap-1.5 px-2 py-1 text-[11px] text-[#8fd49d]">
                  <app-icon name="check-circle" [size]="13" />
                  <span>Online</span>
                </span>
              } @else if (localPhase() === 'starting') {
                <span class="inline-flex items-center gap-1.5 rounded-md bg-[#24262d] px-3 py-1 text-[11px] text-[#c8cad0]">
                  <span class="h-2.5 w-2.5 rounded-full border-[1.5px] border-[#60a5fa] border-t-transparent sy-ring-spin"></span>
                  <span>Starting syred...</span>
                </span>
              } @else if (lh.syred_found || lh.connected) {
                <button
                  type="button"
                  [disabled]="localBusy() || (!lh.connected && !lh.worker_id)"
                  (click)="addLocal()"
                  class="sy-btn sy-btn-sm sy-btn-soft"
                  data-testid="add-local"
                >
                  {{ lh.connected ? 'Attach' : localPhase() === 'failed' || lh.launch_state === 'failed' ? 'Retry' : 'Add' }}
                </button>
              }
            </div>
          </div>
        } @else if (setup.error()) {
          <div class="rounded-lg bg-[#151619] px-3 py-3 text-[12px] text-[#86888f]">Could not check this machine: {{ setup.error() }}</div>
        }
        @if (localMsg()) {
          <div class="rounded-lg bg-[#69b879]/10 px-3 py-2.5 text-[11.5px] text-[#98dba5]">{{ localMsg() }}</div>
        }
        @if (localError()) {
          <div role="alert" class="rounded-lg bg-[#e05d5d]/10 px-3 py-2.5 text-[11.5px] text-[#f09595] break-words">{{ localError() }}</div>
        }
      </div>

      <!-- From your ssh config -->
      <div class="space-y-2">
        <div class="text-[11px] text-[#8a8c94]">From your ssh config</div>

        @if (sshHostsLoading()) {
          <div class="py-6 text-center text-[12px] text-[#86888f]">Reading ~/.ssh/config...</div>
        } @else if (sshHosts().length === 0) {
          <div class="rounded-lg bg-[#151619] px-3 py-3 text-[12px] text-[#86888f]">
            No hosts found in <code class="font-mono-ide text-[#9a9ca4]">~/.ssh/config</code>. Enter an SSH target below or use the manual command.
          </div>
        } @else {
          <div class="max-h-[290px] overflow-y-auto -mx-1 px-1 space-y-1">
            @for (host of sshHosts(); track host.alias) {
              <div class="flex items-center justify-between gap-3 rounded-lg px-2 py-1.5 hover:bg-[#22242a]/80 transition">
                <div class="flex items-center gap-2.5 min-w-0">
                  <app-icon name="server" [size]="14" class="text-[#8a8c94]" />
                  <span class="text-[12.5px] font-medium text-[#e4e4e7] shrink-0">{{ host.alias }}</span>
                  <span class="text-[12px] text-[#86888f] truncate">{{ host.display }}</span>
                </div>

                <div class="shrink-0">
                  @if (host.added) {
                    <button
                      type="button"
                      (click)="inspectSSHHostMachine(host)"
                      class="sy-btn sy-btn-sm"
                      syTooltip="View machine details or remove"
                    >
                      <app-icon name="check-circle" [size]="13" />
                      <span>Added · Details</span>
                    </button>
                  } @else if (bootstrappingTarget() === host.alias) {
                    <span class="inline-flex items-center gap-1.5 rounded-md bg-[#24262d] px-3 py-1 text-[11px] text-[#c8cad0]">
                      <span class="h-2.5 w-2.5 rounded-full border-[1.5px] border-[#60a5fa] border-t-transparent sy-ring-spin"></span>
                      <span>{{ sshStageLabel() || 'Connecting...' }}</span>
                    </span>
                  } @else {
                    <button
                      type="button"
                      [disabled]="!!bootstrappingTarget()"
                      (click)="bootstrapSSHHost(host.alias)"
                      class="sy-btn sy-btn-sm sy-btn-soft"
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
        <div class="text-[11px] text-[#8a8c94]">Or type how ssh reaches it</div>
        <form (ngSubmit)="bootstrapCustomSSH()" class="flex items-center gap-2">
          <input
            type="text"
            name="customSshTarget"
            [ngModel]="customSshTarget()"
            (ngModelChange)="customSshTarget.set($event)"
            placeholder="user@host"
            autocomplete="off"
            spellcheck="false"
            class="sy-field flex-1 text-[12.5px]"
          />
          <button
            type="submit"
            [disabled]="!customSshTarget().trim() || !!bootstrappingTarget()"
            class="sy-btn sy-btn-soft shrink-0"
          >
            {{ bootstrappingTarget() && bootstrappingTarget() === customSshTarget().trim() ? 'Adding...' : 'Add' }}
          </button>
        </form>
      </div>

      @if (bootstrappingTarget() && sshStageDetail()) {
        <div class="rounded-lg bg-[#141518] px-3 py-2.5 flex items-center gap-2 text-[11.5px] text-[#b4b6be]">
          <span class="h-2.5 w-2.5 rounded-full border-[1.5px] border-[#60a5fa] border-t-transparent sy-ring-spin shrink-0"></span>
          <span class="font-medium text-[#e4e4e7]">{{ sshStageLabel() }}:</span>
          <span class="truncate">{{ sshStageDetail() }}</span>
        </div>
      }
      @if (sshStatusMsg()) {
        <div class="rounded-lg bg-[#69b879]/10 px-3 py-2.5 text-[11.5px] text-[#98dba5]">
          {{ sshStatusMsg() }}
        </div>
      }
      @if (hostKeyPrompt(); as prompt) {
        <div
          role="alertdialog"
          aria-labelledby="sy-hostkey-title"
          aria-describedby="sy-hostkey-desc"
          class="rounded-lg bg-[#d9a043]/10 px-3 py-3 space-y-2.5 text-[11.5px] text-[#e8c88a]"
        >
          <div id="sy-hostkey-title" class="text-[12.5px] font-semibold text-[#f4d9a4]">
            Verify the host key for {{ prompt.target }}
          </div>
          <p id="sy-hostkey-desc" class="leading-relaxed">
            Syre has never connected to this machine. Before trusting it, check that a fingerprint
            below matches the one on the machine itself (for example
            <code class="font-mono-ide text-[#f4d9a4]">ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub</code>).
            If it does not match, do not continue.
          </p>
          <ul class="space-y-1.5">
            @for (key of prompt.keys; track key.fingerprint) {
              <li class="flex items-center justify-between gap-2 rounded-md bg-[#15140f] px-3 py-1.5">
                <div class="min-w-0">
                  <div class="text-[10.5px] text-[#a79a7e]">{{ key.type }}</div>
                  <code class="font-mono-ide text-[11px] text-[#f4f4f5] break-all">{{ key.fingerprint }}</code>
                </div>
                <button
                  type="button"
                  (click)="confirmHostKey(key)"
                  [attr.aria-label]="'Fingerprint matches; trust ' + key.type + ' key and continue'"
                  class="sy-btn sy-btn-sm sy-btn-soft shrink-0"
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
          <div role="alert" class="rounded-lg bg-[#e05d5d]/15 px-3 py-2.5 text-[12px] text-[#ffb4b4] break-words space-y-1">
            <div class="text-[13px] font-semibold text-[#ffd0d0]">Host key changed: possible man-in-the-middle</div>
            <div>{{ sshErrorMsg() }}</div>
          </div>
        } @else {
          <div role="alert" class="rounded-lg bg-[#e05d5d]/10 px-3 py-2.5 text-[11.5px] text-[#f09595] break-words">
            {{ sshErrorMsg() }}
          </div>
        }
      }

      <div class="flex items-center gap-3 pt-1 text-[11px]">
        <label class="inline-flex items-center gap-1.5 text-[#b4b6be] cursor-pointer">
          <input type="checkbox" [ngModel]="sandbox()" (ngModelChange)="sandbox.set($event)" class="accent-[#4a668a]" />
          <span>Opt-in sandboxed mode</span>
        </label>
        <label class="inline-flex items-center gap-1.5 text-[#b4b6be] cursor-pointer">
          <input type="checkbox" [ngModel]="enableMock()" (ngModelChange)="enableMock.set($event)" class="accent-[#4a668a]" />
          <span>Include mock driver</span>
        </label>
      </div>
      }
    </app-modal-dialog>
  `,
})
export class AddMachineModalComponent implements OnInit, OnDestroy {
  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);
  readonly setup = inject(SetupStore);

  /** Opened automatically on a hub's first run. */
  readonly firstRun = input(false);
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
  /** Which way to add: this machine and ssh hosts, or a manual join code. */
  readonly tab = signal<'auto' | 'manual'>('auto');
  readonly sandbox = signal(false);
  readonly enableMock = signal(false);
  /** Unknown host key awaiting the operator's fingerprint confirmation. */
  readonly hostKeyPrompt = signal<{ target: string; keys: SSHHostKey[] } | null>(null);
  /** The hub refuses changed host keys outright; flag that error prominently. */
  readonly hostKeyChanged = computed(() => /host key .* has changed/i.test(this.sshErrorMsg()));

  readonly local = this.setup.local;
  readonly localSummary = computed(() => {
    const lh = this.local();
    return lh ? localHostSummary(lh) : '';
  });
  /** The local machine being added (by worker id), followed until it is online. */
  readonly localAddId = signal('');
  readonly localBusy = signal(false);
  readonly localMsg = signal('');
  readonly localError = signal('');
  readonly localPhase = computed<'idle' | 'starting' | 'online' | 'failed'>(() => {
    const id = this.localAddId();
    if (!id) return 'idle';
    const w = this.fleet.workersById()[id];
    if (w?.status === 'online') return 'online';
    if (w?.launch_state === 'failed') return 'failed';
    return 'starting';
  });

  /** Aborts an in-flight bootstrap stream when the modal closes. */
  private bootstrapAbort: AbortController | null = null;
  private readonly timers = new Set<ReturnType<typeof setTimeout>>();
  private destroyed = false;

  async ngOnInit(): Promise<void> {
    void this.setup.load();
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
    this.destroyed = true;
    this.bootstrapAbort?.abort();
    for (const t of this.timers) clearTimeout(t);
  }

  /** Adds this machine: attaches its running syred, or has the hub start one. */
  async addLocal(): Promise<void> {
    if (this.localBusy()) return;
    this.localBusy.set(true);
    this.localMsg.set('');
    this.localError.set('');
    try {
      const res = await this.api.addLocalWorker();
      this.setup.markCompleted();
      this.localAddId.set(res.worker_id);
      await this.fleet.refresh();
      if (res.state === 'connected') {
        this.localMsg.set(`Attached the syred running on ${this.local()?.hostname ?? 'this machine'}.`);
      }
      this.watchLocal();
    } catch (err) {
      this.localError.set(err instanceof Error ? err.message : 'Could not add this machine');
    } finally {
      this.localBusy.set(false);
    }
  }

  /** Follows a starting local daemon; the socket usually reports first, this is a fallback. */
  private watchLocal(): void {
    if (this.destroyed) return;
    const phase = this.localPhase();
    if (phase === 'online' || phase === 'failed') {
      void this.setup.load();
      return;
    }
    this.later(() => {
      void this.fleet.refresh().then(() => this.watchLocal());
    }, 2000);
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
      this.setup.markCompleted();
      this.sshStatusMsg.set(result.message || `syred ready on ${clean}.`);
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

  private later(fn: () => void, ms: number): void {
    const t = setTimeout(() => {
      this.timers.delete(t);
      fn();
    }, ms);
    this.timers.add(t);
  }
}

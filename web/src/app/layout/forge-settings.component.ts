import { ChangeDetectionStrategy, Component, DestroyRef, OnInit, inject, signal } from '@angular/core';
import { ForgeDeviceStart, ForgeHost, ForgeKind } from '../core/models/forge.models';
import { ApiClientService } from '../core/services/api-client.service';
import { IconComponent } from '../shared/icon.component';
import { TooltipDirective } from '../shared/tooltip.directive';

/** Human-readable forge kind label. */
export function forgeKindLabel(kind: string): string {
  switch (kind) {
    case 'github':
      return 'GitHub';
    case 'gitlab':
      return 'GitLab';
    case 'forgejo':
      return 'Forgejo';
    default:
      return 'Unknown';
  }
}

/** Plain-word description of where Konsert gets credentials for a forge host. */
export function credentialSourceLabel(source: string, kind: string): string {
  switch (source) {
    case 'git-credential':
      return 'from your git credentials';
    case 'gh':
      return 'from gh';
    case 'glab':
      return 'from glab';
    case 'pasted':
      return 'token you pasted';
    case 'device':
      return `signed in with ${kind === 'gitlab' ? 'GitLab' : 'GitHub'}`;
    case 'none':
    case '':
    default:
      return 'not connected';
  }
}

/** True when the hub holds a stored token that the user can remove. */
export function hasStoredToken(source: string): boolean {
  return source === 'pasted' || source === 'device';
}

/** Builds the POST /api/forges/detect payload from a remote URL or bare hostname. */
export function buildDetectPayload(raw: string): { remote_url?: string; host?: string } {
  const trimmed = raw.trim();
  if (trimmed.includes('://') || (trimmed.includes(':') && trimmed.includes('/'))) {
    return { remote_url: trimmed };
  }
  return { host: trimmed };
}

interface ActiveDeviceFlow {
  id: string;
  host: string;
  kind: ForgeKind;
  userCode: string;
  verificationUri: string;
  verificationUriComplete?: string;
  intervalSec: number;
  status: 'pending' | 'authorized' | 'expired' | 'denied' | 'error';
  message?: string;
}

/**
 * Settings → Forges (KON-CK7S2): lists every forge host Konsert has seen with its kind
 * and credential source, lets you add a host with detection, paste or remove an encrypted
 * token, and sign in via OAuth device flow when configured on the hub.
 */
@Component({
  selector: 'ks-forge-settings',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective],
  template: `
    <section
      id="settings-forges"
      class="rounded-[var(--ks-radius-lg)] bg-black/20 p-4 space-y-3 text-[12px]"
      data-testid="settings-forges"
      aria-labelledby="ks-forges-title"
    >
      <div class="flex items-baseline justify-between gap-2">
        <div id="ks-forges-title" class="text-[11px] font-semibold uppercase tracking-wider text-[var(--ks-text-muted)]">
          Forges
        </div>
        @if (forges().length > 0) {
          <span class="text-[10.5px] text-[var(--ks-text-muted)]">
            {{ forges().length }} {{ forges().length === 1 ? 'host' : 'hosts' }}
          </span>
        }
      </div>

      <p class="m-0 text-[11.5px] text-[var(--ks-text-secondary)]">
        Used to open and merge PRs/MRs when you approve work (delivery modes).
      </p>

      @if (loading()) {
        <p class="m-0 text-[var(--ks-text-muted)]">Loading forges…</p>
      } @else if (forges().length === 0) {
        <p class="m-0 rounded-[var(--ks-radius-md)] bg-black/20 px-3 py-2.5 text-[11.5px] text-[var(--ks-text-muted)]" data-testid="forges-empty">
          No forge hosts seen yet. Add a host below, or connect a repo with a git remote.
        </p>
      } @else {
        <ul class="m-0 p-0 list-none space-y-2" data-testid="forges-list">
          @for (f of forges(); track f.host) {
            <li
              class="rounded-[var(--ks-radius-md)] bg-black/25 border border-[var(--ks-hairline)] p-3 space-y-2.5"
              [attr.data-forge-host]="f.host"
            >
              <div class="flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
                <div class="flex items-center gap-2 min-w-0">
                  <span
                    class="h-2 w-2 shrink-0 rounded-full"
                    [class.bg-[var(--ks-green)]]="f.credential_source !== 'none'"
                    [class.bg-[var(--ks-text-muted)]]="f.credential_source === 'none'"
                    aria-hidden="true"
                  ></span>
                  <span class="font-mono-ide text-[12.5px] font-medium text-[var(--ks-text)] break-all" data-testid="forge-host">
                    {{ f.host }}
                  </span>
                  <span
                    class="shrink-0 rounded-[var(--ks-radius-sm)] bg-[var(--ks-raised)] px-1.5 py-0.5 text-[10.5px] font-medium text-[var(--ks-text-secondary)]"
                    data-testid="forge-kind"
                  >
                    {{ kindLabel(f.kind) }}
                  </span>
                </div>
                <span
                  class="text-[11.5px]"
                  [class.text-[var(--ks-green)]]="f.credential_source !== 'none'"
                  [class.text-[var(--ks-text-muted)]]="f.credential_source === 'none'"
                  data-testid="forge-source"
                >
                  {{ sourceLabel(f.credential_source, f.kind) }}
                </span>
              </div>

              <div class="flex flex-wrap items-center gap-2">
                <button
                  type="button"
                  class="ks-btn ks-btn-sm"
                  data-testid="forge-paste-btn"
                  [ksTooltip]="isStored(f.credential_source) ? 'Replace the token stored on the hub' : 'Store an encrypted personal access token on the hub'"
                  (click)="openPaste(f.host)"
                >
                  <app-icon name="key" [size]="12" />
                  <span>{{ isStored(f.credential_source) ? 'Replace token' : 'Paste token' }}</span>
                </button>

                @if (isStored(f.credential_source)) {
                  <button
                    type="button"
                    class="ks-btn ks-btn-sm text-[#f09595]"
                    data-testid="forge-remove-btn"
                    ksTooltip="Delete the stored token from the hub"
                    (click)="askRemove(f.host)"
                  >
                    <app-icon name="trash" [size]="12" />
                    <span>Remove token</span>
                  </button>
                }

                @if (f.device_flow_available) {
                  <button
                    type="button"
                    class="ks-btn ks-btn-sm ks-btn-primary"
                    data-testid="forge-connect-btn"
                    [disabled]="startingDeviceHost() === f.host"
                    [ksTooltip]="'Sign in with ' + kindLabel(f.kind) + ' using a one-time device code'"
                    (click)="startDevice(f)"
                  >
                    {{ startingDeviceHost() === f.host ? 'Starting…' : 'Connect' }}
                  </button>
                } @else {
                  <span
                    class="text-[11px] text-[var(--ks-text-muted)]"
                    data-testid="forge-device-unavailable"
                    [ksTooltip]="deviceUnavailableTooltip(f.kind)"
                  >
                    {{ deviceUnavailableText(f.kind) }}
                  </span>
                }
              </div>

              @if (pasteHost() === f.host) {
                <div class="rounded-[var(--ks-radius-md)] bg-[var(--ks-surface-1)] border border-[var(--ks-hairline)] p-2.5 space-y-2" data-testid="forge-paste-panel">
                  <label [for]="'ks-forge-tok-' + f.host" class="block text-[11px] text-[var(--ks-text-secondary)]">
                    Personal access token for <span class="font-mono-ide text-[var(--ks-text)]">{{ f.host }}</span>
                  </label>
                  <input
                    [id]="'ks-forge-tok-' + f.host"
                    type="password"
                    autocomplete="off"
                    spellcheck="false"
                    data-testid="forge-token-input"
                    placeholder="Paste token…"
                    class="ks-field ks-field-sm w-full font-mono-ide"
                    [value]="tokenDraft()"
                    (input)="tokenDraft.set($any($event.target).value)"
                    (keydown.enter)="$event.preventDefault(); saveToken(f)"
                  />
                  <div class="flex flex-wrap items-center justify-between gap-2">
                    <span class="text-[10.5px] text-[var(--ks-text-muted)]">
                      Encrypted at rest on the hub; never shown again.
                    </span>
                    <div class="flex items-center gap-1.5">
                      <button type="button" class="ks-btn ks-btn-sm" (click)="cancelPaste()">Cancel</button>
                      <button
                        type="button"
                        class="ks-btn ks-btn-sm ks-btn-primary"
                        data-testid="forge-token-save"
                        [disabled]="savingToken() || !tokenDraft().trim()"
                        (click)="saveToken(f)"
                      >
                        {{ savingToken() ? 'Saving…' : 'Save token' }}
                      </button>
                    </div>
                  </div>
                </div>
              }

              @if (confirmRemoveHost() === f.host) {
                <div
                  class="flex flex-wrap items-center justify-between gap-2 rounded-[var(--ks-radius-md)] bg-[var(--ks-red-soft)] border border-[#e86b6b]/25 px-3 py-2"
                  data-testid="forge-remove-confirm-panel"
                  role="alert"
                >
                  <span class="text-[11.5px] text-[#f3a5a5]">
                    Remove the stored token for <span class="font-mono-ide">{{ f.host }}</span>?
                  </span>
                  <div class="flex items-center gap-1.5">
                    <button type="button" class="ks-btn ks-btn-sm" (click)="confirmRemoveHost.set(null)">Cancel</button>
                    <button
                      type="button"
                      class="ks-btn ks-btn-sm bg-[#e86b6b]/20 text-[#f8b4b4] hover:bg-[#e86b6b]/30"
                      data-testid="forge-remove-confirm"
                      [disabled]="removingHost() === f.host"
                      (click)="confirmRemove(f.host)"
                    >
                      {{ removingHost() === f.host ? 'Removing…' : 'Remove' }}
                    </button>
                  </div>
                </div>
              }

              @if (deviceFlow(); as flow) {
                @if (flow.host === f.host) {
                  <div
                    class="rounded-[var(--ks-radius-md)] bg-[var(--ks-surface-1)] border border-[var(--ks-hairline)] p-3 space-y-2.5"
                    data-testid="forge-device-panel"
                  >
                    @if (flow.status === 'pending') {
                      <div class="flex flex-wrap items-center justify-between gap-2">
                        <div>
                          <div class="text-[10.5px] text-[var(--ks-text-muted)]">Enter this code on {{ kindLabel(flow.kind) }}</div>
                          <div class="font-mono-ide text-[16px] font-semibold tracking-widest text-[var(--ks-text)]" data-testid="forge-device-code">
                            {{ flow.userCode }}
                          </div>
                        </div>
                        <button
                          type="button"
                          class="ks-btn ks-btn-sm"
                          data-testid="forge-device-copy"
                          ksTooltip="Copy device code"
                          (click)="copyDeviceCode(flow.userCode)"
                        >
                          <app-icon name="copy" [size]="12" />
                          <span>{{ copiedCode() ? 'Copied' : 'Copy' }}</span>
                        </button>
                      </div>
                      <div class="flex flex-wrap items-center justify-between gap-2">
                        <a
                          [href]="flow.verificationUriComplete || flow.verificationUri"
                          target="_blank"
                          rel="noopener noreferrer"
                          class="text-[11.5px] text-[var(--ks-blue)] hover:underline break-all"
                          data-testid="forge-device-link"
                        >
                          Open {{ flow.verificationUri }} ↗
                        </a>
                        <button type="button" class="ks-btn ks-btn-sm" (click)="cancelDevice()">Cancel</button>
                      </div>
                      <p class="m-0 text-[11px] text-[var(--ks-text-secondary)]" role="status" data-testid="forge-device-status">
                        Waiting for you to finish signing in…
                      </p>
                    } @else if (flow.status === 'authorized') {
                      <div class="flex items-center justify-between gap-2">
                        <span class="text-[11.5px] text-[var(--ks-green)]" role="status" data-testid="forge-device-status">
                          Signed in to {{ flow.host }}.
                        </span>
                        <button type="button" class="ks-btn ks-btn-sm" (click)="cancelDevice()">Done</button>
                      </div>
                    } @else {
                      <div class="flex flex-wrap items-center justify-between gap-2">
                        <span
                          class="text-[11.5px]"
                          [class.text-[var(--ks-amber)]]="flow.status === 'expired'"
                          [class.text-[#f3a5a5]]="flow.status !== 'expired'"
                          role="alert"
                          data-testid="forge-device-status"
                        >
                          {{ flow.message }}
                        </span>
                        <div class="flex items-center gap-1.5">
                          <button type="button" class="ks-btn ks-btn-sm" (click)="cancelDevice()">Close</button>
                          <button type="button" class="ks-btn ks-btn-sm ks-btn-primary" (click)="startDevice(f)">Try again</button>
                        </div>
                      </div>
                    }
                  </div>
                }
              }
            </li>
          }
        </ul>
      }

      <!-- Add a forge -->
      <div class="pt-1 space-y-1.5" data-testid="forge-add-form">
        <label for="ks-forge-add-input" class="block text-[11px] text-[var(--ks-text-secondary)]">
          Add a forge
        </label>
        <div class="flex flex-col sm:flex-row gap-2">
          <input
            id="ks-forge-add-input"
            type="text"
            autocomplete="off"
            spellcheck="false"
            data-testid="forge-add-input"
            placeholder="github.com or https://git.example.com/org/repo"
            class="ks-field ks-field-sm flex-1 min-w-0 font-mono-ide"
            [value]="addInput()"
            (input)="addInput.set($any($event.target).value)"
            (keydown.enter)="$event.preventDefault(); addForge()"
          />
          <button
            type="button"
            class="ks-btn ks-btn-sm shrink-0"
            data-testid="forge-add-submit"
            [disabled]="detecting() || !addInput().trim()"
            ksTooltip="Detect the forge kind from a git remote URL or hostname"
            (click)="addForge()"
          >
            {{ detecting() ? 'Detecting…' : 'Detect & add' }}
          </button>
        </div>
      </div>

      @if (notice()) {
        <p class="m-0 text-[11.5px] text-[var(--ks-green)]" role="status" data-testid="forge-notice">{{ notice() }}</p>
      }
      @if (error()) {
        <p class="m-0 text-[11.5px] text-[#f3a5a5]" role="alert" data-testid="forge-error">{{ error() }}</p>
      }
    </section>
  `,
})
export class ForgeSettingsComponent implements OnInit {
  private readonly api = inject(ApiClientService);
  private readonly destroyRef = inject(DestroyRef);

  readonly forges = signal<ForgeHost[]>([]);
  readonly loading = signal(true);
  readonly error = signal('');
  readonly notice = signal('');

  readonly addInput = signal('');
  readonly detecting = signal(false);

  readonly pasteHost = signal<string | null>(null);
  readonly tokenDraft = signal('');
  readonly savingToken = signal(false);

  readonly confirmRemoveHost = signal<string | null>(null);
  readonly removingHost = signal<string | null>(null);

  readonly startingDeviceHost = signal<string | null>(null);
  readonly deviceFlow = signal<ActiveDeviceFlow | null>(null);
  readonly copiedCode = signal(false);

  private pollTimer: ReturnType<typeof setTimeout> | null = null;
  private copyTimer: ReturnType<typeof setTimeout> | null = null;

  constructor() {
    this.destroyRef.onDestroy(() => {
      this.stopPolling();
      this.tokenDraft.set('');
      if (this.copyTimer) clearTimeout(this.copyTimer);
    });
  }

  async ngOnInit(): Promise<void> {
    await this.load();
  }

  kindLabel(kind: string): string {
    return forgeKindLabel(kind);
  }

  sourceLabel(source: string, kind: string): string {
    return credentialSourceLabel(source, kind);
  }

  isStored(source: string): boolean {
    return hasStoredToken(source);
  }

  deviceUnavailableText(kind: string): string {
    if (kind === 'forgejo') {
      return 'Token auth only (device sign-in not supported)';
    }
    return 'Device sign-in needs an OAuth app configured';
  }

  deviceUnavailableTooltip(kind: string): string {
    if (kind === 'forgejo') {
      return 'Forgejo does not support OAuth device sign-in; paste a personal access token instead.';
    }
    return 'Configure forges.github_client_id or forges.gitlab_client_id on the hub to enable device sign-in.';
  }

  async load(): Promise<void> {
    this.error.set('');
    try {
      const res = await this.api.listForges();
      this.forges.set(res.forges ?? []);
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : 'Could not load forges');
    } finally {
      this.loading.set(false);
    }
  }

  async addForge(): Promise<void> {
    const raw = this.addInput().trim();
    if (!raw || this.detecting()) return;
    this.detecting.set(true);
    this.error.set('');
    this.notice.set('');
    try {
      const detected = await this.api.detectForge(buildDetectPayload(raw));
      this.addInput.set('');
      await this.load();
      this.notice.set(`Added ${detected.host} (${forgeKindLabel(detected.kind)}).`);
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : 'Could not detect forge');
    } finally {
      this.detecting.set(false);
    }
  }

  openPaste(host: string): void {
    this.error.set('');
    this.notice.set('');
    this.confirmRemoveHost.set(null);
    if (this.pasteHost() === host) {
      this.cancelPaste();
      return;
    }
    this.tokenDraft.set('');
    this.pasteHost.set(host);
  }

  cancelPaste(): void {
    this.tokenDraft.set('');
    this.pasteHost.set(null);
  }

  async saveToken(host: ForgeHost): Promise<void> {
    const token = this.tokenDraft().trim();
    if (!token || this.savingToken()) return;
    this.savingToken.set(true);
    this.error.set('');
    this.notice.set('');
    try {
      await this.api.putForgeToken(host.host, token, host.kind);
      // Clear token immediately after saving; never retain or display it.
      this.tokenDraft.set('');
      this.pasteHost.set(null);
      await this.load();
      this.notice.set(`Saved token for ${host.host}.`);
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : 'Could not save token');
    } finally {
      this.savingToken.set(false);
    }
  }

  askRemove(host: string): void {
    this.error.set('');
    this.notice.set('');
    this.cancelPaste();
    this.confirmRemoveHost.set(this.confirmRemoveHost() === host ? null : host);
  }

  async confirmRemove(host: string): Promise<void> {
    if (this.removingHost()) return;
    this.removingHost.set(host);
    this.error.set('');
    this.notice.set('');
    try {
      await this.api.deleteForgeToken(host);
      this.confirmRemoveHost.set(null);
      await this.load();
      this.notice.set(`Removed stored token for ${host}.`);
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : 'Could not remove token');
    } finally {
      this.removingHost.set(null);
    }
  }

  async startDevice(host: ForgeHost): Promise<void> {
    if (!host.device_flow_available || this.startingDeviceHost()) return;
    this.stopPolling();
    this.cancelPaste();
    this.confirmRemoveHost.set(null);
    this.startingDeviceHost.set(host.host);
    this.error.set('');
    this.notice.set('');
    try {
      const started: ForgeDeviceStart = await this.api.startForgeDevice(host.host, host.kind);
      this.deviceFlow.set({
        id: started.id,
        host: started.host,
        kind: started.kind,
        userCode: started.user_code,
        verificationUri: started.verification_uri,
        verificationUriComplete: started.verification_uri_complete,
        intervalSec: Math.max(1, started.interval || 5),
        status: 'pending',
      });
      this.schedulePoll();
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : 'Could not start device sign-in');
    } finally {
      this.startingDeviceHost.set(null);
    }
  }

  cancelDevice(): void {
    this.stopPolling();
    this.deviceFlow.set(null);
  }

  async copyDeviceCode(code: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(code);
      this.copiedCode.set(true);
      if (this.copyTimer) clearTimeout(this.copyTimer);
      this.copyTimer = setTimeout(() => this.copiedCode.set(false), 1800);
    } catch {
      // Clipboard may be unavailable in non-secure contexts; user can still select the code.
    }
  }

  private schedulePoll(): void {
    this.stopPolling();
    const cur = this.deviceFlow();
    if (!cur || cur.status !== 'pending') return;
    this.pollTimer = setTimeout(() => void this.pollOnce(), cur.intervalSec * 1000);
  }

  private async pollOnce(): Promise<void> {
    const cur = this.deviceFlow();
    if (!cur || cur.status !== 'pending') return;
    try {
      const res = await this.api.pollForgeDevice(cur.host, cur.id);
      if (this.deviceFlow()?.id !== cur.id) return;
      switch (res.status) {
        case 'authorized':
          this.stopPolling();
          this.deviceFlow.set({ ...cur, status: 'authorized' });
          await this.load();
          return;
        case 'expired':
          this.stopPolling();
          this.deviceFlow.set({ ...cur, status: 'expired', message: 'Sign-in code expired. Try again.' });
          return;
        case 'denied':
          this.stopPolling();
          this.deviceFlow.set({ ...cur, status: 'denied', message: res.error || 'Sign-in was cancelled.' });
          return;
        case 'slow_down':
        case 'pending':
        default: {
          const nextInterval = Math.max(1, res.interval || cur.intervalSec);
          this.deviceFlow.set({ ...cur, intervalSec: nextInterval });
          this.schedulePoll();
          return;
        }
      }
    } catch (e) {
      if (this.deviceFlow()?.id !== cur.id) return;
      this.stopPolling();
      this.deviceFlow.set({
        ...cur,
        status: 'error',
        message: e instanceof Error ? e.message : 'Sign-in failed. Try again.',
      });
    }
  }

  private stopPolling(): void {
    if (this.pollTimer) {
      clearTimeout(this.pollTimer);
      this.pollTimer = null;
    }
  }
}

import { PushSettingsComponent } from './push-settings.component';
import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  OnInit,
  computed,
  effect,
  inject,
  input,
  output,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { McpCredential, PairingCode } from '../../core/models/device.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { DevicesStore } from '../../core/state/devices.store';
import { IconComponent } from '../../shared/icon.component';
import { ModalDialogComponent } from '../../shared/modal-dialog.component';
import { formatCountdown } from './device-name';
import {
  KIND_ICON,
  KIND_LABEL,
  ago,
  claudeMcpCommand,
  expiresIn,
  mcpJsonSnippet,
  whereFrom,
} from './devices-logic';

export type DevicesView = 'list' | 'add' | 'mcp';

/**
 * Settings → Devices: every signed-in browser, phone, CLI and MCP client, with Rename and
 * Revoke; "Add a device" (QR + link + code) and "Connect an MCP client".
 */
@Component({
  selector: 'sy-devices-dialog',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [FormsModule, IconComponent, ModalDialogComponent, PushSettingsComponent],
  template: `
    <app-modal-dialog
      labelledBy="devices-title"
      panelClass="w-full max-w-xl max-h-[88vh] flex flex-col rounded-[var(--sy-radius-xl)] bg-[var(--sy-surface-2)] overflow-hidden"
      (close)="close.emit()"
    >
      <div class="flex items-center gap-2 px-4 h-12 shrink-0 border-b border-[var(--sy-hairline)]">
        @if (view() !== 'list') {
          <button
            type="button"
            class="sy-icon-btn"
            aria-label="Back to devices"
            (click)="view.set('list')"
          >
            <app-icon name="chevron-right" [size]="14" class="rotate-180" />
          </button>
        }
        <h2 id="devices-title" class="m-0 flex-1 text-[14px] font-semibold">{{ title() }}</h2>
        <button type="button" class="sy-icon-btn" aria-label="Close" (click)="close.emit()">
          <app-icon name="close" [size]="15" />
        </button>
      </div>

      <div class="flex-1 min-h-0 overflow-y-auto p-4 text-[12.5px]">
        @switch (view()) {
          @case ('list') {
            @if (!store.authRequired()) {
              <p class="mb-3 rounded-md bg-[#f0c895]/10 px-3 py-2 text-[#f0c895]">
                Authentication is off (--no-auth): anyone who can reach this hub is signed in.
              </p>
            }
            <sy-push-settings />
            <div class="flex flex-wrap gap-2 mb-4">
              <button
                type="button"
                class="sy-btn sy-btn-primary"
                data-testid="add-device"
                (click)="openAdd()"
              >
                <app-icon name="plus" [size]="13" /> Add a device
              </button>
              <button
                type="button"
                class="sy-btn"
                data-testid="connect-mcp"
                (click)="view.set('mcp')"
              >
                <app-icon name="key" [size]="13" /> Connect an MCP client
              </button>
            </div>
            @if (store.error()) {
              <p role="alert" class="mb-3 rounded-md bg-[#e05d5d]/10 px-3 py-2 text-[#f09595]">
                {{ store.error() }}
              </p>
            }
            <ul
              class="list-none m-0 p-0 divide-y divide-[var(--sy-hairline)]"
              aria-label="Signed-in devices"
              data-testid="device-list"
            >
              @for (d of store.devices(); track d.id) {
                <li class="flex items-start gap-3 py-3" [attr.data-device]="d.sid">
                  <span class="mt-0.5 text-[var(--sy-text-muted)]"
                    ><app-icon [name]="kindIcon(d.kind)" [size]="16"
                  /></span>
                  <div class="flex-1 min-w-0">
                    @if (editingId() === d.id) {
                      <form class="flex gap-2" (ngSubmit)="saveRename(d.id)">
                        <input
                          name="rename"
                          [ngModel]="editName()"
                          (ngModelChange)="editName.set($event)"
                          maxlength="60"
                          aria-label="Device name"
                          autofocus
                          class="sy-field flex-1 min-w-0"
                        />
                        <button
                          type="submit"
                          class="sy-btn sy-btn-sm sy-btn-primary"
                          [disabled]="!editName().trim()"
                        >
                          Save
                        </button>
                        <button type="button" class="sy-btn sy-btn-sm" (click)="editingId.set('')">
                          Cancel
                        </button>
                      </form>
                    } @else {
                      <div class="flex items-center gap-2 min-w-0">
                        <span class="truncate font-medium text-[var(--sy-text)]">{{ d.name }}</span>
                        @if (d.current) {
                          <span
                            class="shrink-0 rounded-full bg-[#4a668a]/25 px-2 py-0.5 text-[10.5px] text-[#a9c1e0]"
                            >This device</span
                          >
                        }
                        @if (d.expired) {
                          <span
                            class="shrink-0 rounded-full bg-[#e05d5d]/15 px-2 py-0.5 text-[10.5px] text-[#f09595]"
                            >Expired</span
                          >
                        }
                      </div>
                    }
                    <div class="mt-0.5 text-[11.5px] text-[var(--sy-text-muted)] truncate">
                      {{ kindLabel(d.kind) }} · {{ d.sid }} · used {{ ago(d.last_used_at) }}
                      @if (whereFrom(d)) {
                        · {{ whereFrom(d) }}
                      }
                    </div>
                    @if (!d.expired) {
                      <div class="text-[11px] text-[var(--sy-text-faint,#6b6d75)]">
                        Signed out if unused {{ expiresIn(d.expires_at) }}
                      </div>
                    }
                    @if (confirmId() === d.id) {
                      <div
                        class="mt-2 flex flex-wrap items-center gap-2 rounded-md bg-[#e05d5d]/10 px-3 py-2"
                        role="group"
                        aria-label="Confirm revoke"
                      >
                        <span class="text-[#f09595]">{{
                          d.current
                            ? 'Sign this browser out now?'
                            : 'Revoke ' + d.name + '? It is signed out at once.'
                        }}</span>
                        <span class="flex-1"></span>
                        <button
                          type="button"
                          class="sy-btn sy-btn-sm bg-[#e05d5d]/80 text-white"
                          data-testid="confirm-revoke"
                          (click)="revoke(d.id)"
                        >
                          Revoke
                        </button>
                        <button type="button" class="sy-btn sy-btn-sm" (click)="confirmId.set('')">
                          Cancel
                        </button>
                      </div>
                    }
                  </div>
                  @if (editingId() !== d.id && confirmId() !== d.id) {
                    <div class="flex shrink-0 gap-1">
                      <button
                        type="button"
                        class="sy-btn sy-btn-sm"
                        [attr.aria-label]="'Rename ' + d.name"
                        (click)="startRename(d.id, d.name)"
                      >
                        Rename
                      </button>
                      <button
                        type="button"
                        class="sy-btn sy-btn-sm text-[#f09595]"
                        [attr.aria-label]="'Revoke ' + d.name"
                        data-testid="revoke"
                        (click)="confirmId.set(d.id)"
                      >
                        Revoke
                      </button>
                    </div>
                  }
                </li>
              } @empty {
                @if (store.loaded()) {
                  <li class="py-6 text-center text-[var(--sy-text-muted)]">
                    No signed-in devices yet.
                  </li>
                }
              }
            </ul>
            <p class="mt-4 text-[11.5px] leading-snug text-[var(--sy-text-muted)]">
              Devices stay signed in while they are used at least every {{ store.idleDays() }} days
              (change it in Settings). Revoking signs a device out immediately.
            </p>
          }

          @case ('add') {
            <div
              class="flex flex-col items-center text-center gap-3"
              data-testid="add-device-panel"
            >
              @if (pairError()) {
                <p role="alert" class="rounded-md bg-[#e05d5d]/10 px-3 py-2 text-[#f09595]">
                  {{ pairError() }}
                </p>
              }
              @if (paired(); as p) {
                <div
                  role="status"
                  class="flex items-center gap-2 max-w-full whitespace-nowrap rounded-lg bg-[#69b879]/10 px-4 py-3 text-[#8fd49d]"
                  data-testid="paired-notice"
                >
                  <app-icon name="check-circle" [size]="15" />
                  <span class="min-w-0 truncate"><b>{{ p.name }}</b> is signed in.</span>
                </div>
                <div class="flex gap-2">
                  <button type="button" class="sy-btn" (click)="openAdd()">Add another</button>
                  <button type="button" class="sy-btn sy-btn-primary" (click)="view.set('list')">
                    Done
                  </button>
                </div>
              } @else if (pc(); as c) {
                <p class="text-[var(--sy-text-muted)] max-w-sm">
                  Scan with the phone's camera, or open the link on the other device. The code works
                  once.
                </p>
                <svg
                  role="img"
                  aria-label="QR code for the sign-in link"
                  class="w-52 h-52 rounded-lg bg-white p-1"
                  [class.opacity-30]="expired()"
                  [attr.viewBox]="'0 0 ' + c.qr.size + ' ' + c.qr.size"
                  shape-rendering="crispEdges"
                >
                  <path [attr.d]="c.qr.path" fill="#000" />
                </svg>
                <div
                  class="font-mono-ide text-[22px] tracking-[0.25em] text-[var(--sy-text)]"
                  data-testid="pair-code-display"
                >
                  {{ c.code }}
                </div>
                <div class="flex items-center gap-2 max-w-full">
                  <code
                    class="truncate rounded-md bg-black/30 px-2.5 py-1.5 font-mono-ide text-[11.5px]"
                    data-testid="pair-link"
                    >{{ c.url }}</code
                  >
                  <button type="button" class="sy-btn sy-btn-sm" (click)="copy(c.url, 'link')">
                    {{ copied() === 'link' ? 'Copied' : 'Copy' }}
                  </button>
                </div>
                @if (expired()) {
                  <p class="text-[#f0c895]">This code expired.</p>
                  <button type="button" class="sy-btn sy-btn-primary" (click)="openAdd()">
                    New code
                  </button>
                } @else {
                  <p class="text-[11.5px] text-[var(--sy-text-muted)]" aria-live="off">
                    Expires in
                    <span class="font-mono-ide" data-testid="pair-countdown">{{
                      countdown()
                    }}</span>
                    · waiting for the device…
                  </p>
                }
              } @else {
                <p class="text-[var(--sy-text-muted)]" role="status">Creating a code…</p>
              }
            </div>
          }

          @case ('mcp') {
            <div class="space-y-3" data-testid="mcp-panel">
              @if (!mcp()) {
                <p class="text-[var(--sy-text-muted)]">
                  Creates a credential that works only on this hub's
                  <code class="font-mono-ide">/mcp</code> endpoint (it cannot change the LLM API
                  key). It appears in Devices, where you can revoke it.
                </p>
                <form class="flex gap-2" (ngSubmit)="createMcp()">
                  <label for="mcp-name" class="sr-only">Client name</label>
                  <input
                    id="mcp-name"
                    name="mcpName"
                    class="sy-field flex-1 min-w-0"
                    [ngModel]="mcpName()"
                    (ngModelChange)="mcpName.set($event)"
                    maxlength="60"
                    placeholder="Claude Code"
                  />
                  <button type="submit" class="sy-btn sy-btn-primary" [disabled]="mcpBusy()">
                    Create credential
                  </button>
                </form>
                @if (mcpError()) {
                  <p role="alert" class="rounded-md bg-[#e05d5d]/10 px-3 py-2 text-[#f09595]">
                    {{ mcpError() }}
                  </p>
                }
              } @else {
                <p class="rounded-md bg-[#f0c895]/10 px-3 py-2 text-[#f0c895]">
                  Copy it now: the credential is shown only once.
                </p>
                <div>
                  <div class="flex items-center justify-between mb-1">
                    <span
                      class="text-[11px] font-semibold uppercase tracking-wider text-[var(--sy-text-muted)]"
                      >Claude Code</span
                    >
                    <button
                      type="button"
                      class="sy-btn sy-btn-sm"
                      (click)="copy(claudeCommand(), 'claude')"
                    >
                      {{ copied() === 'claude' ? 'Copied' : 'Copy' }}
                    </button>
                  </div>
                  <pre
                    class="m-0 whitespace-pre-wrap break-all rounded-md bg-black/30 p-2.5 font-mono-ide text-[11.5px]"
                    data-testid="mcp-claude-command"
                    >{{ claudeCommand() }}</pre>
                </div>
                <div>
                  <div class="flex items-center justify-between mb-1">
                    <span
                      class="text-[11px] font-semibold uppercase tracking-wider text-[var(--sy-text-muted)]"
                      >Other clients (JSON)</span
                    >
                    <button
                      type="button"
                      class="sy-btn sy-btn-sm"
                      (click)="copy(jsonSnippet(), 'json')"
                    >
                      {{ copied() === 'json' ? 'Copied' : 'Copy' }}
                    </button>
                  </div>
                  <pre
                    class="m-0 whitespace-pre-wrap break-all rounded-md bg-black/30 p-2.5 font-mono-ide text-[11.5px]"
                    >{{ jsonSnippet() }}</pre>
                </div>
                <button type="button" class="sy-btn" (click)="mcp.set(null); view.set('list')">
                  Done
                </button>
              }
            </div>
          }
        }
      </div>
    </app-modal-dialog>
  `,
})
export class DevicesDialogComponent implements OnInit {
  readonly close = output<void>();
  readonly initialView = input<DevicesView>('list');

  readonly store = inject(DevicesStore);
  private readonly api = inject(ApiClientService);

  readonly view = signal<DevicesView>('list');
  readonly editingId = signal('');
  readonly editName = signal('');
  readonly confirmId = signal('');
  readonly copied = signal('');

  readonly pc = signal<PairingCode | null>(null);
  readonly pairError = signal('');
  readonly paired = signal<{ name: string } | null>(null);
  private readonly now = signal(Date.now());
  readonly expired = computed(() => {
    const c = this.pc();
    return !!c && this.now() >= Date.parse(c.expires_at);
  });
  readonly countdown = computed(() =>
    formatCountdown((this.pc() ? Date.parse(this.pc()!.expires_at) : 0) - this.now()),
  );

  readonly mcp = signal<McpCredential | null>(null);
  readonly mcpName = signal('Claude Code');
  readonly mcpBusy = signal(false);
  readonly mcpError = signal('');
  readonly claudeCommand = computed(() =>
    this.mcp() ? claudeMcpCommand(this.mcp()!.mcp_url, this.mcp()!.token) : '',
  );
  readonly jsonSnippet = computed(() =>
    this.mcp() ? mcpJsonSnippet(this.mcp()!.mcp_url, this.mcp()!.token) : '',
  );

  readonly title = computed(
    () => ({ list: 'Devices', add: 'Add a device', mcp: 'Connect an MCP client' })[this.view()],
  );

  readonly ago = ago;
  readonly expiresIn = expiresIn;
  readonly whereFrom = whereFrom;

  constructor() {
    const timer = setInterval(() => this.now.set(Date.now()), 1000);
    inject(DestroyRef).onDestroy(() => clearInterval(timer));
    // The device that redeems the shown code appears live (fleet.update "device").
    effect(() => {
      const d = this.store.lastAdded();
      const c = untracked(() => this.pc());
      if (d && c && d.created_by === `pairing code ${c.sid}`)
        untracked(() => this.paired.set({ name: d.name }));
    });
  }

  ngOnInit(): void {
    void this.store.load();
    const v = this.initialView();
    if (v === 'add') this.openAdd();
    else this.view.set(v);
  }

  kindIcon(kind: string): string {
    return KIND_ICON[kind as keyof typeof KIND_ICON] ?? 'monitor';
  }

  kindLabel(kind: string): string {
    return KIND_LABEL[kind as keyof typeof KIND_LABEL] ?? kind;
  }

  async openAdd(): Promise<void> {
    this.view.set('add');
    this.pc.set(null);
    this.paired.set(null);
    this.pairError.set('');
    try {
      this.pc.set(await this.api.createPairingCode());
      this.now.set(Date.now());
    } catch (err) {
      this.pairError.set(err instanceof Error ? err.message : 'Could not create a code');
    }
  }

  startRename(id: string, name: string): void {
    this.confirmId.set('');
    this.editingId.set(id);
    this.editName.set(name);
  }

  async saveRename(id: string): Promise<void> {
    const name = this.editName().trim();
    if (!name) return;
    try {
      await this.store.rename(id, name);
      this.editingId.set('');
    } catch (err) {
      this.store.error.set(err instanceof Error ? err.message : 'Rename failed');
    }
  }

  async revoke(id: string): Promise<void> {
    this.confirmId.set('');
    try {
      await this.store.revoke(id);
    } catch (err) {
      this.store.error.set(err instanceof Error ? err.message : 'Revoke failed');
    }
  }

  async createMcp(): Promise<void> {
    this.mcpBusy.set(true);
    this.mcpError.set('');
    try {
      this.mcp.set(await this.api.createMcpCredential(this.mcpName().trim() || 'MCP client'));
    } catch (err) {
      this.mcpError.set(err instanceof Error ? err.message : 'Could not create a credential');
    } finally {
      this.mcpBusy.set(false);
    }
  }

  async copy(text: string, which: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(text);
      this.copied.set(which);
      setTimeout(() => this.copied() === which && this.copied.set(''), 1500);
    } catch {
      // Clipboard unavailable (insecure context): the text stays selectable.
    }
  }
}

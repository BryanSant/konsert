import { Component, ElementRef, OnInit, computed, effect, inject, input, output, signal } from '@angular/core';
import type { DevicesView } from '../features/auth/devices-dialog.component';
import { FormsModule } from '@angular/forms';
import { PermissionMode } from '../core/models/syre.models';
import { DELIVERY_MODE_OPTIONS, DeliveryMode, MERGE_STYLE_OPTIONS, MergeStyle } from '../core/models/review.models';
import { ApiClientService } from '../core/services/api-client.service';
import { AuthService } from '../core/services/auth.service';
import { NotificationService } from '../core/services/notification.service';
import { FleetStore } from '../core/state/fleet.store';
import { ModalDialogComponent } from '../shared/modal-dialog.component';
import { AgentAccountsStore } from '../core/state/agent-accounts.store';
import { defaultModel, effortsFor, findModel } from '../features/chat/composer/composer-logic';
import { QuestionInsightsComponent } from '../features/questions/question-insights.component';
import { ForgeSettingsComponent } from './forge-settings.component';
import { StorageSettingsComponent } from './storage-settings.component';
import { ShellPolicySettingsComponent } from './shell-policy-settings.component';
import { MachineCredentialsSecurityComponent } from './machine-credentials-security.component';

@Component({
  selector: 'app-settings-modal',
  standalone: true,
  imports: [FormsModule, ModalDialogComponent, QuestionInsightsComponent, MachineCredentialsSecurityComponent, ForgeSettingsComponent, StorageSettingsComponent, ShellPolicySettingsComponent],
  template: `
    <app-modal-dialog labelledBy="settings-title" panelClass="w-full max-w-lg rounded-[var(--sy-radius-xl)] bg-[var(--sy-surface-2)] backdrop-blur-xl p-6 space-y-5 max-h-[88vh] overflow-y-auto" (close)="close.emit()">
      <div class="flex items-center justify-between">
        <div>
          <h3 class="text-[15px] font-semibold text-[#ececed]" id="settings-title">Settings</h3>
          <p class="text-[12px] text-[#8e9098]">
            Saved on the hub and applied right away.
          </p>
        </div>
        <button type="button" (click)="close.emit()" aria-label="Esc: close dialog" class="sy-kbd cursor-pointer hover:text-[#ececed]">Esc</button>
      </div>

      <form (ngSubmit)="save()" class="space-y-4 text-[12px]">
        <!-- Session, Terminal & Fleet Defaults -->
        <div class="rounded-[var(--sy-radius-lg)] bg-black/20 p-4 space-y-3.5">
          <div class="text-[11px] font-semibold uppercase tracking-wider text-[#8e9098]">
            Session Defaults &amp; Notifications
          </div>

          <div class="grid grid-cols-2 gap-3">
            <div>
              <label for="settings-modal-default-account" class="block text-[11px] text-[#8e9098] mb-1">Default agent account</label>
              <select id="settings-modal-default-account"
                name="defaultAccount"
                [ngModel]="defaultAccountKey()"
                (ngModelChange)="onAccountChange($event)"
                class="sy-field w-full"
              >
                <option value="">Auto (last used)</option>
                @for (o of accountOptions(); track o.key) {
                  <option [value]="o.key">{{ o.label }}</option>
                }
              </select>
            </div>

            <div>
              <label for="settings-modal-default-model" class="block text-[11px] text-[#8e9098] mb-1">Default model</label>
              <select id="settings-modal-default-model"
                name="defaultModel"
                [ngModel]="defaultModel()"
                (ngModelChange)="onModelChange($event)"
                [disabled]="modelOptions().length === 0"
                class="sy-field w-full disabled:opacity-50"
              >
                <option value="">Account default</option>
                @for (m of modelOptions(); track m.id) {
                  <option [value]="m.id">{{ m.display_name }}</option>
                }
              </select>
            </div>
          </div>

          @if (effortOptions().length > 0) {
            <div class="grid grid-cols-2 gap-3">
              <div>
                <label for="settings-modal-default-effort" class="block text-[11px] text-[#8e9098] mb-1">Default effort</label>
                <select id="settings-modal-default-effort"
                  name="defaultEffort"
                  [ngModel]="defaultEffort()"
                  (ngModelChange)="defaultEffort.set($event)"
                  class="sy-field w-full"
                >
                  <option value="">Model default</option>
                  @for (e of effortOptions(); track e.id) {
                    <option [value]="e.id">{{ e.label }}</option>
                  }
                </select>
              </div>
            </div>
          }

          <div class="grid grid-cols-2 gap-3">
            <div>
              <label for="settings-modal-default-permission-mode" class="block text-[11px] text-[#8e9098] mb-1">Default Permission Mode</label>
              <select id="settings-modal-default-permission-mode"
                name="perm"
                [ngModel]="defaultPerm()"
                (ngModelChange)="defaultPerm.set($event)"
                class="sy-field w-full"
              >
                <option value="full_access">Full Access (Default)</option>
                <option value="sandboxed">Sandboxed (Opt-In)</option>
              </select>
            </div>

            <div>
              <label for="settings-modal-terminal-font-size-fonts" class="block text-[11px] text-[#8e9098] mb-1">Terminal Font Size ({{ fontSize() }}px)</label>
              <input id="settings-modal-terminal-font-size-fonts"
                type="range"
                name="fontSize"
                min="11"
                max="18"
                [ngModel]="fontSize()"
                (ngModelChange)="fontSize.set(+$event)"
                class="w-full accent-[#4a668a] mt-2"
              />
            </div>
          </div>

          <div class="flex items-center gap-5 pt-1">
            <label class="inline-flex items-center gap-2 text-[#d4d4d8] cursor-pointer">
              <input type="checkbox" name="notifySound" [ngModel]="notifySound()" (ngModelChange)="notifySound.set($event)" class="accent-[#4a668a]" />
              <span>Subtle sound alerts</span>
            </label>
            <label class="inline-flex items-center gap-2 text-[#d4d4d8] cursor-pointer">
              <input type="checkbox" name="notifyDesktop" [ngModel]="notifyDesktop()" (ngModelChange)="onToggleDesktop($event)" class="accent-[#4a668a]" />
              <span>Desktop notifications</span>
            </label>
          </div>
        </div>

        <!-- Delivery Defaults (SYR-H50YW) -->
        <div class="rounded-[var(--sy-radius-lg)] bg-black/20 p-4 space-y-3">
          <div class="text-[11px] font-semibold uppercase tracking-wider text-[#8e9098]">
            Delivery Defaults
          </div>
          <p class="m-0 text-[11.5px] text-[#8e9098]">
            What Approve does with finished work. Projects and repos can override these in Projects &amp; repos.
          </p>
          <div class="grid grid-cols-1 sm:grid-cols-[1.7fr_1fr] gap-3">
            <div>
              <label for="settings-modal-delivery-mode" class="block text-[11px] text-[#8e9098] mb-1">Default delivery mode</label>
              <select id="settings-modal-delivery-mode"
                name="defaultDeliveryMode"
                data-testid="settings-delivery-mode"
                [ngModel]="defaultDeliveryMode()"
                (ngModelChange)="defaultDeliveryMode.set($event)"
                class="sy-field w-full"
              >
                @for (opt of deliveryModeOptions; track opt.value) {
                  <option [value]="opt.value">{{ opt.label }}</option>
                }
              </select>
            </div>
            <div>
              <label for="settings-modal-merge-style" class="block text-[11px] text-[#8e9098] mb-1">Default merge style</label>
              <select id="settings-modal-merge-style"
                name="defaultMergeStyle"
                data-testid="settings-merge-style"
                [ngModel]="defaultMergeStyle()"
                (ngModelChange)="defaultMergeStyle.set($event)"
                class="sy-field w-full"
              >
                @for (opt of mergeStyleOptions; track opt.value) {
                  <option [value]="opt.value">{{ opt.label }}</option>
                }
              </select>
            </div>
          </div>
        </div>

        <!-- Forges (SYR-CK7S2) -->
        <sy-forge-settings />

        <!-- Sign-in & devices -->
        <div class="rounded-[var(--sy-radius-lg)] bg-black/20 p-4 space-y-3">
          <div class="text-[11px] font-semibold uppercase tracking-wider text-[#8e9098]">Sign-in &amp; devices</div>
          <div class="flex flex-wrap gap-2">
            <button type="button" class="sy-btn sy-btn-sm" data-testid="settings-devices" (click)="openDevices.emit('list')">Devices…</button>
            <button type="button" class="sy-btn sy-btn-sm" (click)="openDevices.emit('add')">Add a device…</button>
            <button type="button" class="sy-btn sy-btn-sm" (click)="openDevices.emit('mcp')">Connect an MCP client…</button>
          </div>
          <div class="flex items-center gap-2">
            <label for="settings-modal-device-idle-days" class="text-[#d4d4d8]">Keep devices signed in while used every</label>
            <input id="settings-modal-device-idle-days" type="number" name="deviceIdleDays" min="1" max="3650"
              [ngModel]="deviceIdleDays()" (ngModelChange)="deviceIdleDays.set(+$event)"
              class="sy-field w-20 font-mono-ide" />
            <span class="text-[#d4d4d8]">days</span>
          </div>
        </div>

        <!-- Commander policy writes (SYR-P3H92) -->
        <div class="rounded-[var(--sy-radius-lg)] bg-black/20 p-4 space-y-2">
          <div class="text-[11px] font-semibold uppercase tracking-wider text-[#8e9098]">Commander policy</div>
          <label for="settings-commander-policy-writes" class="block text-[12px] text-[#d4d4d8]">When the Commander sets policy</label>
          <select
            id="settings-commander-policy-writes"
            name="commanderPolicyWrites"
            data-testid="settings-commander-policy-writes"
            [ngModel]="commanderPolicyWrites()"
            (ngModelChange)="commanderPolicyWrites.set($event)"
            class="sy-field w-full"
          >
            <option value="tighten">Tighten immediately (default)</option>
            <option value="confirm">Ask me to approve every change</option>
            <option value="none">Read-only</option>
          </select>
          <p class="m-0 text-[11.5px] text-[#8e9098]">
            Tightening (pause, throttle, drain, disable a provider) applies at once, attributed to you via Commander, with Undo on the receipt. Ending a directive or raising a cap still needs your approval. Confirm turns every tightening change into a Needs you card.
          </p>
        </div>

        <!-- Shells left running by finished sessions (SYR-DCEZV) -->
        <sy-shell-policy-settings />

        <!-- Storage: size, breakdown, retention, Compact now (SYR-CSCWP) -->
        <sy-storage-settings />

        <!-- Commander LLM Section -->
        <details class="rounded-[var(--sy-radius-lg)] bg-black/20 group" [open]="apiKeySet()">
          <summary class="cursor-pointer list-none px-3.5 py-3 text-[12px] text-[#a1a3ab] hover:text-[#ececed] flex items-center justify-between">
            <span><span class="font-semibold">Advanced:</span> API-key commander (optional)</span>
            <span class="text-[10.5px] text-[#86888f]">{{ apiKeySet() ? 'Configured' : 'Not needed for subscription agents' }}</span>
          </summary>
          <div class="px-3.5 pb-3.5 space-y-3">
          <p class="text-[11.5px] text-[#8e9098]">
            Chats run on your signed-in agent CLIs by default. Configure a provider API key only if you want the API-key commander as an extra backend.
          </p>

          <div class="grid grid-cols-2 gap-3">
            <div>
              <label for="settings-modal-provider" class="block text-[11px] text-[#8e9098] mb-1">Provider</label>
              <select id="settings-modal-provider"
                name="provider"
                [ngModel]="provider()"
                (ngModelChange)="onProviderChange($event)"
                class="sy-field w-full"
              >
                <option value="openai">OpenAI / Compatible</option>
                <option value="anthropic">Anthropic</option>
                <option value="gemini">Google Gemini</option>
              </select>
            </div>

            <div>
              <label for="settings-modal-model" class="block text-[11px] text-[#8e9098] mb-1">Model</label>
              <input id="settings-modal-model"
                type="text"
                name="model"
                [ngModel]="model()"
                (ngModelChange)="model.set($event)"
                placeholder="model id (required)"
                class="sy-field w-full font-mono-ide"
              />
            </div>
          </div>

          <div>
            <label for="settings-modal-custom-base-url-optional" class="block text-[11px] text-[#8e9098] mb-1">Custom Base URL (optional — OpenRouter, Ollama, vLLM)</label>
            <input id="settings-modal-custom-base-url-optional"
              type="text"
              name="baseUrl"
              [ngModel]="baseUrl()"
              (ngModelChange)="baseUrl.set($event)"
              placeholder="https://api.openai.com/v1 or http://127.0.0.1:11434/v1"
              class="sy-field w-full font-mono-ide"
            />
          </div>

          <div>
            <div class="flex items-center justify-between mb-1">
              <label for="settings-modal-api-key" class="text-[11px] text-[#8e9098]">API Key</label>
              @if (apiKeySet()) {
                <span class="inline-flex items-center gap-2 text-[10px]">
                  <span class="text-[#69b879]">● Configured</span>
                  <button type="button" (click)="clearKey()" class="text-[#8e9098] hover:text-[#f09595] underline cursor-pointer">
                    Clear
                  </button>
                </span>
              }
            </div>
            <input id="settings-modal-api-key"
              type="password"
              name="apiKey"
              [ngModel]="apiKey()"
              (ngModelChange)="apiKey.set($event)"
              [placeholder]="apiKeySet() ? '•••••••••••••••• (enter new key to replace)' : 'sk-...'"
              class="sy-field w-full font-mono-ide"
            />
          </div>
        </div>
        </details>

        <sy-question-insights />

        <app-machine-credentials-security />

        @if (error()) {
          <div class="rounded-md bg-[#e05d5d]/10 px-3 py-2.5 text-[#f09595]">
            {{ error() }}
          </div>
        }
        @if (savedNotice()) {
          <div class="rounded-md bg-[#69b879]/10 px-3 py-2.5 text-[#8fd49d]">
            Settings saved and applied.
          </div>
        }

        <div class="flex justify-end gap-2 pt-1">
          <button
            type="button"
            (click)="close.emit()"
            class="sy-btn sy-btn-sm"
          >
            Close
          </button>
          <button
            type="submit"
            [disabled]="saving()"
            class="sy-btn sy-btn-primary"
          >
            {{ saving() ? 'Saving...' : 'Save Settings' }}
          </button>
        </div>
      </form>
    </app-modal-dialog>
  `,
})
export class SettingsModalComponent implements OnInit {
  readonly focusSection = input<'forges' | null>(null);
  readonly close = output<void>();
  /** Opens Settings → Devices on a view. */
  readonly openDevices = output<DevicesView>();
  /** How long machine vitals and heat are kept (fixed tiers on the hub). */

  private readonly hostEl = inject<ElementRef<HTMLElement>>(ElementRef);
  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);
  private readonly auth = inject(AuthService);
  private readonly notifications = inject(NotificationService);
  private readonly agentAccounts = inject(AgentAccountsStore);

  constructor() {
    effect(() => {
      if (this.focusSection() === 'forges') {
        queueMicrotask(() => {
          this.hostEl.nativeElement.querySelector('#settings-forges')?.scrollIntoView({ block: 'start' });
        });
      }
    });
  }

  readonly provider = signal('openai');
  readonly model = signal<string>('');
  readonly baseUrl = signal('');
  readonly apiKey = signal('');
  readonly apiKeySet = signal(false);
  readonly clearApiKey = signal(false);
  readonly defaultAgent = signal('');
  readonly defaultAccountId = signal('');
  readonly defaultModel = signal('');
  readonly defaultEffort = signal('');

  /** Accounts from capabilities, one entry per agent/account id across machines. */
  readonly accountOptions = computed(() => {
    const seen = new Map<string, { key: string; label: string; agent: string; id: string }>();
    for (const { workerId, account } of this.agentAccounts.all()) {
      const key = `${account.agent}:${account.id}`;
      if (seen.has(key) || account.chat_supported === false) continue;
      const host = this.fleet.workersById()[workerId]?.name;
      seen.set(key, { key, label: `${account.label}${host ? ' · ' + host : ''}`, agent: account.agent, id: account.id });
    }
    return [...seen.values()];
  });
  readonly defaultAccountKey = computed(() =>
    this.defaultAccountId() ? `${this.defaultAgent()}:${this.defaultAccountId()}` : '',
  );
  private readonly selectedAccount = computed(() => {
    const id = this.defaultAccountId();
    if (!id) return null;
    return this.agentAccounts.all().find((r) => r.account.id === id && r.account.agent === this.defaultAgent())?.account ?? null;
  });
  readonly modelOptions = computed(() => this.selectedAccount()?.models ?? []);
  readonly effortOptions = computed(() =>
    effortsFor(findModel(this.selectedAccount(), this.defaultModel()) ?? defaultModel(this.selectedAccount())),
  );
  readonly defaultPerm = signal<PermissionMode>('full_access');
  readonly fontSize = signal(13);
  readonly notifySound = signal(true);
  readonly notifyDesktop = signal(false);
  readonly deviceIdleDays = signal(90);
  readonly defaultDeliveryMode = signal<DeliveryMode>('');
  readonly defaultMergeStyle = signal<MergeStyle>('squash');
  readonly commanderPolicyWrites = signal('tighten');
  readonly deliveryModeOptions = DELIVERY_MODE_OPTIONS;
  readonly mergeStyleOptions = MERGE_STYLE_OPTIONS;

  readonly saving = signal(false);
  readonly error = signal('');
  readonly savedNotice = signal(false);

  async ngOnInit(): Promise<void> {
    try {
      const s = await this.api.getSettings();
      this.fleet.settings.set(s);
      this.provider.set(s.llm_provider || 'openai');
      this.model.set(s.llm_model || '');
      this.baseUrl.set(s.llm_base_url || '');
      this.apiKeySet.set(!!s.llm_api_key_set);
      this.defaultAgent.set(s.default_agent || '');
      this.defaultAccountId.set(s.default_account_id || '');
      this.defaultModel.set(s.default_model || '');
      this.defaultEffort.set(s.default_effort || '');
      this.defaultPerm.set(s.default_permission_mode || 'full_access');
      this.fontSize.set(s.terminal_font_size || 13);
      this.notifySound.set(!!s.notify_sound);
      this.notifyDesktop.set(!!s.notify_desktop);
      this.deviceIdleDays.set(s.device_idle_days || 90);
      this.defaultDeliveryMode.set(s.default_delivery_mode || '');
      this.defaultMergeStyle.set(s.default_merge_style || 'squash');
      this.commanderPolicyWrites.set(s.commander_policy_writes || 'tighten');
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Failed to load settings');
    }
  }

  onProviderChange(next: string): void {
    this.provider.set(next);
  }

  onAccountChange(key: string): void {
    const opt = this.accountOptions().find((o) => o.key === key);
    this.defaultAgent.set(opt?.agent ?? '');
    this.defaultAccountId.set(opt?.id ?? '');
    if (!this.modelOptions().some((m) => m.id === this.defaultModel())) this.defaultModel.set('');
    this.defaultEffort.set('');
  }

  onModelChange(id: string): void {
    this.defaultModel.set(id);
    if (!this.effortOptions().some((e) => e.id === this.defaultEffort())) this.defaultEffort.set('');
  }

  onToggleDesktop(enabled: boolean): void {
    this.notifyDesktop.set(enabled);
    if (enabled) {
      void this.notifications.requestDesktopPermission();
    }
  }

  clearKey(): void {
    this.clearApiKey.set(true);
    this.apiKeySet.set(false);
    this.apiKey.set('');
  }

  async save(): Promise<void> {
    this.saving.set(true);
    this.error.set('');
    this.savedNotice.set(false);
    try {
      const updated = await this.api.updateSettings({
        llm_provider: this.provider(),
        llm_model: this.model().trim(),
        llm_base_url: this.baseUrl().trim(),
        llm_api_key: this.apiKey().trim() || undefined,
        clear_api_key: this.clearApiKey(),
        default_agent: this.defaultAgent().trim(),
        default_account_id: this.defaultAccountId(),
        default_model: this.defaultModel().trim(),
        default_effort: this.defaultEffort(),
        default_permission_mode: this.defaultPerm(),
        terminal_font_size: this.fontSize(),
        notify_sound: this.notifySound(),
        notify_desktop: this.notifyDesktop(),
        device_idle_days: Math.min(3650, Math.max(1, Math.round(this.deviceIdleDays() || 90))),
        default_delivery_mode: this.defaultDeliveryMode(),
        default_merge_style: this.defaultMergeStyle(),
        commander_policy_writes: this.commanderPolicyWrites(),
      });
      this.fleet.settings.set(updated);
      this.auth.provider.set(updated.llm_provider);
      this.auth.model.set(updated.llm_model);
      this.apiKeySet.set(updated.llm_api_key_set);
      this.apiKey.set('');
      this.clearApiKey.set(false);
      this.savedNotice.set(true);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Failed to save settings');
    } finally {
      this.saving.set(false);
    }
  }
}

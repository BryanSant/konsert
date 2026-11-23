import { Component, OnInit, inject, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { PermissionMode } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { AuthService } from '../core/services/auth.service';
import { NotificationService } from '../core/services/notification.service';
import { FleetStore } from '../core/state/fleet.store';
import { ModalDialogComponent } from '../shared/modal-dialog.component';

@Component({
  selector: 'app-settings-modal',
  standalone: true,
  imports: [FormsModule, ModalDialogComponent],
  template: `
    <app-modal-dialog labelledBy="settings-title" panelClass="w-full max-w-lg rounded-xl border border-[#30323c] bg-[#18191d]/95 backdrop-blur-xl p-5 space-y-4 max-h-[88vh] overflow-y-auto" (close)="close.emit()">
      <div class="flex items-center justify-between">
        <div>
          <h3 class="text-[15px] font-semibold text-[#ececed]" id="settings-title">Settings</h3>
          <p class="text-[12px] text-[#8e9098]">
            Changes persist in SQLite and hot-reload the Orchestrator immediately.
          </p>
        </div>
        <button type="button" (click)="close.emit()" aria-label="Close dialog" class="ks-kbd cursor-pointer hover:text-[#ececed]">Esc</button>
      </div>

      <form (ngSubmit)="save()" class="space-y-4 text-[12px]">
        <!-- Orchestrator LLM Section -->
        <div class="rounded-lg border border-[#26282e] bg-[#131417] p-3.5 space-y-3">
          <div class="text-[11px] font-semibold uppercase tracking-wider text-[#8e9098]">
            Orchestrator LLM Provider
          </div>

          <div class="grid grid-cols-2 gap-3">
            <div>
              <label for="settings-modal-provider" class="block text-[11px] text-[#8e9098] mb-1">Provider</label>
              <select id="settings-modal-provider"
                name="provider"
                [ngModel]="provider()"
                (ngModelChange)="onProviderChange($event)"
                class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed]"
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
                placeholder="gpt-4o / claude-sonnet-4-20250514"
                class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 font-mono-ide text-[#ececed]"
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
              class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 font-mono-ide text-[#ececed]"
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
              class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 font-mono-ide text-[#ececed]"
            />
          </div>
        </div>

        <!-- Session, Terminal & Fleet Defaults -->
        <div class="rounded-lg border border-[#26282e] bg-[#131417] p-3.5 space-y-3">
          <div class="text-[11px] font-semibold uppercase tracking-wider text-[#8e9098]">
            Session Defaults &amp; Notifications
          </div>

          <div class="grid grid-cols-2 gap-3">
            <div>
              <label for="settings-modal-default-chat-agent-optio" class="block text-[11px] text-[#8e9098] mb-1">Default Chat Agent (optional)</label>
              <select id="settings-modal-default-chat-agent-optio"
                name="defaultAgent"
                [ngModel]="defaultAgent()"
                (ngModelChange)="defaultAgent.set($event)"
                class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed]"
              >
                <option value="">Auto (last used → detected)</option>
                <option value="agy">agy (Antigravity)</option>
                <option value="claude">claude (Claude Code)</option>
                <option value="codex">codex (OpenAI Codex)</option>
                <option value="grok">grok (Grok CLI)</option>
                <option value="mock">mock</option>
              </select>
            </div>

            <div>
              <label for="settings-modal-default-chat-model-optio" class="block text-[11px] text-[#8e9098] mb-1">Default Chat Model (optional)</label>
              <input id="settings-modal-default-chat-model-optio"
                type="text"
                name="defaultModel"
                [ngModel]="defaultModel()"
                (ngModelChange)="defaultModel.set($event)"
                placeholder="Auto (last used → guess)"
                class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 font-mono-ide text-[#ececed]"
              />
            </div>
          </div>

          <div class="grid grid-cols-2 gap-3">
            <div>
              <label for="settings-modal-default-permission-mode" class="block text-[11px] text-[#8e9098] mb-1">Default Permission Mode</label>
              <select id="settings-modal-default-permission-mode"
                name="perm"
                [ngModel]="defaultPerm()"
                (ngModelChange)="defaultPerm.set($event)"
                class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed]"
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

        @if (error()) {
          <div class="rounded-md border border-[#e05d5d]/30 bg-[#e05d5d]/10 px-3 py-2 text-[#f09595]">
            {{ error() }}
          </div>
        }
        @if (savedNotice()) {
          <div class="rounded-md border border-[#69b879]/30 bg-[#69b879]/10 px-3 py-2 text-[#8fd49d]">
            Settings saved and applied.
          </div>
        }

        <div class="flex justify-end gap-2 pt-1">
          <button
            type="button"
            (click)="close.emit()"
            class="rounded-md border border-[#2c2e36] px-3 py-1.5 text-[#a1a3ab] hover:text-[#ececed] transition cursor-pointer"
          >
            Close
          </button>
          <button
            type="submit"
            [disabled]="saving()"
            class="rounded-md bg-[#ececed] hover:bg-white active:scale-[0.98] disabled:opacity-40 px-4 py-1.5 font-medium text-[#111214] transition cursor-pointer"
          >
            {{ saving() ? 'Saving...' : 'Save Settings' }}
          </button>
        </div>
      </form>
    </app-modal-dialog>
  `,
})
export class SettingsModalComponent implements OnInit {
  readonly close = output<void>();

  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);
  private readonly auth = inject(AuthService);
  private readonly notifications = inject(NotificationService);

  readonly provider = signal('openai');
  readonly model = signal('gpt-4o');
  readonly baseUrl = signal('');
  readonly apiKey = signal('');
  readonly apiKeySet = signal(false);
  readonly clearApiKey = signal(false);
  readonly defaultAgent = signal('');
  readonly defaultModel = signal('');
  readonly defaultPerm = signal<PermissionMode>('full_access');
  readonly fontSize = signal(13);
  readonly notifySound = signal(true);
  readonly notifyDesktop = signal(false);

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
      this.defaultModel.set(s.default_model || '');
      this.defaultPerm.set(s.default_permission_mode || 'full_access');
      this.fontSize.set(s.terminal_font_size || 13);
      this.notifySound.set(!!s.notify_sound);
      this.notifyDesktop.set(!!s.notify_desktop);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Failed to load settings');
    }
  }

  onProviderChange(next: string): void {
    this.provider.set(next);
    if (!this.model().trim() || ['gpt-4o', 'claude-sonnet-4-20250514', 'gemini-2.5-pro'].includes(this.model())) {
      if (next === 'anthropic') this.model.set('claude-sonnet-4-20250514');
      else if (next === 'gemini') this.model.set('gemini-2.5-pro');
      else this.model.set('gpt-4o');
    }
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
        default_model: this.defaultModel().trim(),
        default_permission_mode: this.defaultPerm(),
        terminal_font_size: this.fontSize(),
        notify_sound: this.notifySound(),
        notify_desktop: this.notifyDesktop(),
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

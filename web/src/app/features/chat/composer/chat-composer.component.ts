import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  ElementRef,
  computed,
  inject,
  input,
  model,
  output,
  signal,
  viewChild,
} from '@angular/core';
import { AgentAccount, AgentApprovalMode } from '../../../core/models/agent-chat.models';
import { accountHealth, alternativeAccounts, bindingWindow, formatCountdown, formatResetTime, windowName } from '../../../core/models/agent-usage';
import { ApiClientService } from '../../../core/services/api-client.service';
import { FleetStore } from '../../../core/state/fleet.store';
import { IconComponent } from '../../../shared/icon.component';
import { TooltipDirective } from '../../../shared/tooltip.directive';
import { AccountPickerComponent } from './account-picker.component';
import { ApprovalPickerComponent } from './approval-picker.component';
import { ComposerSelection, findModel, normalizeSelection, selectModel } from './composer-logic';
import { ContextBarComponent, ContextChange } from './context-bar.component';
import { ModelPickerComponent } from './model-picker.component';
import { SpeechRecognitionLike, joinDictation, speechErrorMessage, speechRecognitionCtor, transcriptOf } from './speech-dictation';

export interface UploadedAttachment {
  filename: string;
  remotePath: string;
  size: number;
  workerId: string;
}

export interface ComposerSubmit {
  text: string;
  attachments: UploadedAttachment[];
}


/**
 * The chat composer (Orca pattern): one roomy container with the prompt on top and a
 * control row (attach, approval chip, account chip, model·effort, send); a quiet context
 * bar (machine · checkout · branch) underneath. Everything is driven by the selected
 * account's capabilities.
 */
@Component({
  selector: 'sy-chat-composer',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective, AccountPickerComponent, ApprovalPickerComponent, ModelPickerComponent, ContextBarComponent],
  template: `
    <form
      class="relative rounded-[var(--sy-radius-xl)] bg-[var(--sy-surface-1)] transition-shadow"
      [class.shadow-[var(--sy-shadow-composer)]]="!dragOver()"
      [class.shadow-[0_0_0_1.5px_#6d8fbf]]="dragOver()"
      (submit)="$event.preventDefault(); submit()"
      (dragover)="allowAttachments() && onDragOver($event)"
      (dragleave)="dragOver.set(false)"
      (drop)="onDrop($event)"
      (keydown.escape)="onEscape($event)"
    >
      @if (dragOver()) {
        <div class="absolute inset-0 z-10 rounded-[var(--sy-radius-xl)] bg-[var(--sy-surface-1)]/95 flex items-center justify-center pointer-events-none">
          <span class="inline-flex items-center gap-2 text-[12.5px] text-[#a9c9f7]">
            <app-icon name="paperclip" [size]="14" />
            Drop files to attach them on {{ machineLabel() }}
          </span>
        </div>
      }

      <!-- Inline account warnings (before sending) -->
      @if (warning(); as w) {
        <div
          class="flex flex-wrap items-center gap-2 mx-2 mt-2 rounded-[var(--sy-radius-lg)] px-3 py-2 text-[12px]"
          [class.bg-[var(--sy-red-soft)]]="w.kind === 'logged_out'"
          [class.text-[#f6c0c0]]="w.kind === 'logged_out'"
          [class.bg-[var(--sy-amber-soft)]]="w.kind !== 'logged_out'"
          [class.text-[#f3d29b]]="w.kind !== 'logged_out'"
          role="status"
          data-testid="composer-warning"
        >
          <app-icon [name]="w.kind === 'logged_out' ? 'key' : 'clock'" [size]="13" />
          <span class="flex-1 min-w-[180px]">{{ w.text }}</span>
          @if (w.alternative; as alt) {
            <button type="button" class="sy-btn sy-btn-sm sy-btn-soft" data-testid="switch-account" (click)="chooseAccount(alt)">
              <app-icon [name]="alt.agent" [size]="12" />
              Use {{ alt.label }}
            </button>
          }
          @if (w.kind === 'logged_out') {
            <button type="button" class="sy-btn sy-btn-sm sy-btn-soft" (click)="showRelogin.emit(selectedAccount()!)">How to sign in</button>
          }
        </div>
      }

      <!-- Attachments -->
      @if (speechError()) {
        <div role="alert" class="flex items-center gap-2 px-4 pt-3 text-[11.5px] text-[#f3a5a5]" data-testid="speech-error">
          <app-icon name="mic" [size]="12" />
          <span class="flex-1">{{ speechError() }}</span>
          <button type="button" class="sy-icon-btn sy-icon-btn-sm !w-5 !h-5" aria-label="Dismiss" (click)="speechError.set('')">
            <app-icon name="close" [size]="10" />
          </button>
        </div>
      }

      @if (attachments().length > 0 || uploadingCount() > 0 || uploadError()) {
        <div class="flex flex-wrap items-center gap-1.5 px-4 pt-3">
          @for (att of attachments(); track att.remotePath) {
            <span class="inline-flex items-center gap-1.5 rounded-full bg-[var(--sy-raised)] pl-2.5 pr-1 py-0.5 text-[11.5px] text-[var(--sy-text)]">
              <app-icon name="paperclip" [size]="11" />
              <span class="max-w-[180px] truncate" [syTooltip]="att.remotePath">{{ att.filename }}</span>
              <span class="font-mono-ide text-[10.5px] text-[var(--sy-text-muted)]">{{ formatBytes(att.size) }}</span>
              <button type="button" class="sy-icon-btn sy-icon-btn-sm !w-5 !h-5" [attr.aria-label]="'Remove ' + att.filename" (click)="removeAttachment(att.remotePath)">
                <app-icon name="close" [size]="10" />
              </button>
            </span>
          }
          @if (uploadingCount() > 0) {
            <span class="inline-flex items-center gap-1.5 text-[11.5px] text-[var(--sy-text-secondary)]">
              <span class="sy-spinner" style="width: 10px; height: 10px"></span>
              Uploading {{ uploadingCount() }}…
            </span>
          }
          @if (uploadError()) {
            <span role="alert" class="text-[11.5px] text-[#f3a5a5]">{{ uploadError() }}</span>
          }
        </div>
      }

      <label class="sy-sr-only" [attr.for]="inputId">Message</label>
      <textarea
        #textarea
        [id]="inputId"
        rows="2"
        data-testid="composer-input"
        class="block w-full resize-none bg-transparent px-4 pt-3.5 pb-2 text-[13.5px] leading-relaxed text-[var(--sy-text)] placeholder-[#80828a] outline-none max-h-[40vh]"
        [placeholder]="placeholder()"
        [value]="draft()"
        (input)="onInput($event)"
        (keydown)="onKeydown($event)"
      ></textarea>

      <input #fileInput type="file" multiple class="hidden" tabindex="-1" aria-hidden="true" (change)="onFilesPicked($event)" />

      <!-- Control row -->
      <div class="flex flex-wrap items-center gap-1 px-2.5 pb-2.5 pt-0.5">
        @if (allowAttachments()) {
          <button type="button" class="sy-icon-btn" aria-label="Attach file" syTooltip="Attach file" (click)="fileInput.click()">
            <app-icon name="plus" [size]="16" />
          </button>
        }

        @if (mode() === 'agent') {
          <sy-approval-picker
            [modes]="approvalModes()"
            [selectedId]="selection().approvalMode"
            [note]="approvalNote()"
            (selected)="chooseApproval($event)"
          />
        } @else {
          <span class="inline-flex items-center gap-1.5 px-2 text-[11.5px] text-[var(--sy-text-muted)]">
            <app-icon name="key" [size]="12" />
            API-key commander
          </span>
        }

        <span class="flex-1"></span>

        @if (mode() === 'agent') {
          <sy-account-picker
            [accounts]="accounts()"
            [selectedId]="selection().accountId"
            (selected)="chooseAccount($event)"
            (refresh)="refreshAccounts.emit()"
          />
          <sy-model-picker
            [models]="models()"
            [selectedId]="selection().model"
            [effort]="selection().effort"
            (modelChange)="chooseModel($event)"
            (effortChange)="chooseEffort($event)"
          />
        }

        @if (speechSupported) {
          <button
            type="button"
            class="sy-icon-btn"
            data-testid="mic-button"
            [class.sy-mic-recording]="dictating()"
            [attr.aria-pressed]="dictating()"
            [attr.aria-label]="dictating() ? 'Stop dictation' : 'Dictate'"
            [syTooltip]="dictating() ? 'Stop dictation (Esc cancels)' : 'Dictate'"
            (click)="toggleDictation()"
          >
            <app-icon name="mic" [size]="15" />
          </button>
          <span class="sy-sr-only" role="status" aria-live="polite">{{ dictating() ? 'Listening. Press Escape to cancel.' : '' }}</span>
        }

        @if (busy()) {
          <button type="button" class="sy-send-btn ml-auto" data-testid="stop-button" aria-label="Stop" syTooltip="Stop" (click)="stop.emit()">
            <app-icon name="stop" [size]="14" />
          </button>
        } @else if (!hideSend()) {
          <button
            type="submit"
            class="sy-send-btn ml-auto"
            data-testid="send-button"
            [disabled]="!canSend()"
            [attr.aria-label]="sendLabel()"
            [syTooltip]="sendBlockedReason() || sendLabel()"
          >
            <app-icon name="send-up" [size]="15" />
          </button>
        }
      </div>
    </form>

    @if (mode() === 'agent') {
      <sy-context-bar
        [workerId]="selection().workerId"
        [cwd]="selection().cwd"
        [branch]="selection().branch"
        [locked]="contextLocked()"
        (changed)="onContextChange($event)"
      />
    }
  `,
})
export class ChatComposerComponent {
  readonly accounts = input<AgentAccount[]>([]);
  readonly selection = model.required<ComposerSelection>();
  readonly mode = input<'agent' | 'api'>('agent');
  /** A turn is running: the send button becomes Stop. */
  readonly busy = input(false);
  readonly contextLocked = input(false);
  /** Shown under the approval list, e.g. "Applies from the next turn". */
  readonly approvalNote = input('');
  readonly sessionId = input('');
  readonly placeholder = input('Ask anything, @ to mention files, / for commands');
  readonly blockedReason = input('');
  /** Session dispatch: an empty prompt starts an interactive session. */
  readonly allowEmpty = input(false);
  /** Accessible name and tooltip of the send button. */
  readonly sendLabel = input('Send message');
  /** Hide the round send button when the host provides its own primary action (Enter still submits). */
  readonly hideSend = input(false);
  readonly allowAttachments = input(true);

  readonly submitted = output<ComposerSubmit>();
  readonly stop = output<void>();
  /** Only the fields the user changed (for live chat.control on existing chats). */
  readonly controlChange = output<Partial<ComposerSelection>>();
  readonly refreshAccounts = output<void>();
  readonly showRelogin = output<AgentAccount>();

  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);
  private readonly textarea = viewChild<ElementRef<HTMLTextAreaElement>>('textarea');

  /** Web Speech API available (hidden otherwise, e.g. Firefox or plain HTTP). */
  readonly speechSupported = !!speechRecognitionCtor();
  readonly dictating = signal(false);
  readonly speechError = signal('');
  private recognition: SpeechRecognitionLike | null = null;
  /** The prompt before dictation started (restored on Esc). */
  private dictationBase = '';
  readonly inputId = `sy-composer-${Math.random().toString(36).slice(2, 8)}`;
  readonly draft = signal('');
  readonly attachments = signal<UploadedAttachment[]>([]);
  readonly uploadingCount = signal(0);
  readonly uploadError = signal('');
  readonly dragOver = signal(false);

  readonly selectedAccount = computed(() => this.accounts().find((a) => a.id === this.selection().accountId) ?? null);
  readonly models = computed(() => this.selectedAccount()?.models ?? []);
  readonly approvalModes = computed(() => this.selectedAccount()?.approval_modes ?? []);
  readonly machineLabel = computed(() => this.fleet.workersById()[this.selection().workerId]?.name || 'the machine');

  /** Exhausted / logged-out warnings, with a one-click switch when possible. */
  readonly warning = computed<{ kind: 'logged_out' | 'exhausted'; text: string; alternative: AgentAccount | null } | null>(() => {
    const a = this.selectedAccount();
    if (!a || this.mode() !== 'agent') return null;
    const health = accountHealth(a);
    const alt = alternativeAccounts(a, this.accounts())[0] ?? null;
    if (health === 'logged_out') {
      return { kind: 'logged_out', text: `${a.label} needs you to sign in again.`, alternative: alt };
    }
    if (health === 'exhausted') {
      const w = bindingWindow(a.usage);
      const name = windowName(w?.window);
      const until = formatResetTime(w?.resets_at);
      const countdown = formatCountdown(w?.resets_at);
      let text = `${a.label} is out of ${name ? name + ' ' : ''}usage`;
      if (until) text += ` until ${until}${countdown && countdown !== 'soon' ? ` (${countdown})` : ''}`;
      text += alt ? `. Use ${alt.label} instead?` : '. You can still send; it will likely be rejected.';
      return { kind: 'exhausted', text, alternative: alt };
    }
    return null;
  });

  readonly sendBlockedReason = computed(() => {
    if (this.blockedReason()) return this.blockedReason();
    if (this.uploadingCount() > 0) return 'Waiting for uploads';
    if (this.mode() === 'agent') {
      const a = this.selectedAccount();
      if (!a) return 'Choose an agent account';
      if (accountHealth(a) === 'logged_out') return `${a.label} is signed out`;
    }
    return '';
  });

  readonly canSend = computed(
    () =>
      !this.sendBlockedReason() &&
      (this.allowEmpty() || this.draft().trim().length > 0 || this.attachments().length > 0),
  );

  constructor() {
    inject(DestroyRef).onDestroy(() => this.recognition?.abort());
  }

  toggleDictation(): void {
    if (this.dictating()) this.stopDictation();
    else this.startDictation();
  }

  /** Starts dictating into the prompt; interim results show live and are replaced by finals. */
  startDictation(): void {
    const Ctor = speechRecognitionCtor();
    if (!Ctor || this.recognition) return;
    this.speechError.set('');
    this.dictationBase = this.draft();
    const rec = new Ctor();
    rec.continuous = true;
    rec.interimResults = true;
    rec.lang = typeof navigator !== 'undefined' ? navigator.language || 'en-US' : 'en-US';
    rec.onresult = (ev) => {
      if (this.recognition !== rec) return;
      const { final, interim } = transcriptOf(ev);
      this.setDraftText(joinDictation(this.dictationBase, final + interim));
    };
    rec.onerror = (ev) => {
      const msg = speechErrorMessage(ev.error);
      if (msg) this.speechError.set(msg);
    };
    rec.onend = () => {
      if (this.recognition === rec) this.endDictation();
    };
    this.recognition = rec;
    this.dictating.set(true);
    try {
      rec.start();
    } catch {
      this.endDictation();
      this.speechError.set('Dictation could not start.');
    }
  }

  /** Stops listening and keeps what was dictated. */
  stopDictation(): void {
    const rec = this.recognition;
    if (!rec) return;
    // Final results for the last phrase still arrive before onend.
    rec.stop();
    this.dictating.set(false);
  }

  /** Esc: discards this dictation and restores the prompt as it was. */
  cancelDictation(): void {
    const rec = this.recognition;
    if (!rec) return;
    this.endDictation();
    rec.abort();
    this.setDraftText(this.dictationBase);
  }

  onEscape(ev: Event): void {
    if (!this.recognition) return;
    ev.preventDefault();
    ev.stopPropagation(); // don't also close a surrounding dialog
    this.cancelDictation();
  }

  private endDictation(): void {
    this.recognition = null;
    this.dictating.set(false);
  }

  private setDraftText(text: string): void {
    this.draft.set(text);
    const el = this.textarea()?.nativeElement;
    if (el) {
      el.value = text;
      el.style.height = 'auto';
      el.style.height = `${Math.min(el.scrollHeight, window.innerHeight * 0.4)}px`;
    }
  }

  /** Prefills the prompt (empty-state suggestions) and focuses it. */
  setDraft(text: string): void {
    this.draft.set(text);
    this.focus();
  }

  focus(): void {
    this.textarea()?.nativeElement.focus();
  }

  chooseAccount(a: AgentAccount): void {
    const next = normalizeSelection(this.selection(), a);
    this.selection.set(next);
    this.controlChange.emit({ accountId: a.id, model: next.model, effort: next.effort, approvalMode: next.approvalMode });
  }

  chooseApproval(m: AgentApprovalMode): void {
    this.selection.update((s) => ({ ...s, approvalMode: m.id }));
    this.controlChange.emit({ approvalMode: m.id });
  }

  chooseModel(id: string): void {
    const next = selectModel(this.selection(), this.selectedAccount(), id);
    this.selection.set(next);
    const change: Partial<ComposerSelection> = { model: next.model };
    if (next.effort) change.effort = next.effort;
    this.controlChange.emit(change);
  }

  chooseEffort(id: string): void {
    const model = findModel(this.selectedAccount(), this.selection().model);
    if (!model?.efforts?.some((e) => e.id === id)) return;
    this.selection.update((s) => ({ ...s, effort: id }));
    this.controlChange.emit({ effort: id });
  }

  onContextChange(c: ContextChange): void {
    this.selection.update((s) => ({
      ...s,
      workerId: c.workerId ?? s.workerId,
      cwd: c.cwd ?? s.cwd,
      branch: c.branch ?? (c.cwd !== undefined ? '' : s.branch),
    }));
  }

  onInput(ev: Event): void {
    const el = ev.target as HTMLTextAreaElement;
    this.draft.set(el.value);
    el.style.height = 'auto';
    el.style.height = `${Math.min(el.scrollHeight, window.innerHeight * 0.4)}px`;
  }

  onKeydown(ev: KeyboardEvent): void {
    if (ev.key === 'Enter' && !ev.shiftKey && !ev.isComposing) {
      ev.preventDefault();
      this.submit();
    }
  }

  submit(): void {
    if (!this.canSend() || this.busy()) return;
    if (this.recognition) this.cancelDictationKeepText();
    this.submitted.emit({ text: this.draft().trim(), attachments: this.attachments() });
  }

  /** Sending mid-dictation keeps the text but stops listening (no late results). */
  private cancelDictationKeepText(): void {
    const rec = this.recognition;
    this.endDictation();
    rec?.abort();
  }

  /** Called by the parent once the message was actually handed to the hub. */
  clear(): void {
    this.draft.set('');
    this.attachments.set([]);
    this.uploadError.set('');
    const el = this.textarea()?.nativeElement;
    if (el) el.style.height = '';
  }

  onFilesPicked(ev: Event): void {
    const input = ev.target as HTMLInputElement;
    if (input.files?.length) {
      void this.uploadFiles(Array.from(input.files));
      input.value = '';
    }
  }

  onDragOver(ev: DragEvent): void {
    ev.preventDefault();
    this.dragOver.set(true);
  }

  onDrop(ev: DragEvent): void {
    ev.preventDefault();
    this.dragOver.set(false);
    const files = ev.dataTransfer?.files;
    if (files?.length) void this.uploadFiles(Array.from(files));
  }

  private async uploadFiles(files: File[]): Promise<void> {
    this.uploadError.set('');
    this.uploadingCount.update((n) => n + files.length);
    for (const file of files) {
      try {
        const res = await this.api.uploadAttachment(file, {
          workerId: this.selection().workerId || undefined,
          sessionId: this.sessionId() || undefined,
        });
        this.attachments.update((l) => [
          ...l,
          { filename: res.filename || file.name, remotePath: res.remote_path, size: res.size || file.size, workerId: res.worker_id },
        ]);
      } catch (err) {
        this.uploadError.set(err instanceof Error ? err.message : `Upload failed for ${file.name}`);
      } finally {
        this.uploadingCount.update((n) => Math.max(0, n - 1));
      }
    }
  }

  removeAttachment(path: string): void {
    this.attachments.update((l) => l.filter((a) => a.remotePath !== path));
  }

  formatBytes(size: number): string {
    if (size < 1024) return `${size}B`;
    if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)}KB`;
    return `${(size / (1024 * 1024)).toFixed(1)}MB`;
  }
}

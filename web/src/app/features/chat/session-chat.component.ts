import { ChangeDetectionStrategy, Component, DestroyRef, ElementRef, computed, effect, inject, input, signal, untracked, viewChild } from '@angular/core';
import { AgentAccount } from '../../core/models/agent-chat.models';
import { alternativeAccounts } from '../../core/models/agent-usage';
import { KonsertSocketService } from '../../core/services/konsert-socket.service';
import { AgentAccountsStore } from '../../core/state/agent-accounts.store';
import { AgentChatStore } from '../../core/state/agent-chat.store';
import { OrchestratorStore } from '../../core/state/orchestrator.store';
import { IconComponent } from '../../shared/icon.component';
import { ElicitationAnswer } from './transcript/elicitation-card.component';
import { ChatTranscriptComponent } from './transcript/chat-transcript.component';

/**
 * One agent chat outside the main chat view (a Command tab, a Wall tile): its live
 * transcript and a reply box. It watches the chat's events only while it is on screen.
 */
@Component({
  selector: 'ks-session-chat',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [ChatTranscriptComponent, IconComponent],
  host: { class: 'flex flex-col min-h-0' },
  template: `
    @if (session(); as s) {
      <ks-chat-transcript
        class="flex-1"
        [items]="transcript().items"
        [status]="transcript().status"
        [subtitle]="transcript().subtitle"
        [agent]="s.agent || ''"
        [accountLabel]="account()?.label || s.account_id || ''"
        [workerId]="s.worker_id || ''"
        [accountId]="s.account_id || ''"
        [relogin]="account()?.auth?.relogin"
        [alternatives]="alternatives()"
        [compactedAt]="s.compacted_at"
        [compactedBefore]="s.compacted_before"
        (answer)="respond($event)"
        (retry)="agentChat.retry(s)"
        (keepWaiting)="agentChat.dismissError(s.id, $event)"
        (stop)="agentChat.cancel(s.id)"
      />
      <form class="shrink-0" [class.px-4]="!compact()" [class.pb-3]="!compact()" [class.px-2.5]="compact()" [class.pb-2]="compact()" (submit)="$event.preventDefault(); send()">
        <div class="mx-auto flex items-end gap-2 rounded-[var(--ks-radius-xl)] bg-[var(--ks-surface-2)] px-3 py-2 shadow-[var(--ks-shadow-composer)]" [class.max-w-3xl]="!compact()">
          <label class="ks-sr-only" [attr.for]="inputId">Reply to {{ s.title || 'this agent' }}</label>
          <textarea
            #box
            [id]="inputId"
            rows="1"
            data-testid="session-reply"
            class="flex-1 resize-none bg-transparent text-[13px] leading-relaxed text-[var(--ks-text)] placeholder:text-[var(--ks-text-muted)] outline-none max-h-32"
            [placeholder]="placeholder()"
            [disabled]="blocked()"
            [value]="draft()"
            (input)="draft.set($any($event.target).value)"
            (keydown)="onKey($event)"
          ></textarea>
          @if (busy()) {
            <button type="button" class="ks-icon-btn ks-icon-btn-sm ks-btn-danger" aria-label="Stop the turn" (click)="agentChat.cancel(s.id)">
              <app-icon name="stop" [size]="11" />
            </button>
          }
          <button type="submit" class="ks-send-btn !h-7 !w-7" aria-label="Send reply" [disabled]="!draft().trim() || blocked()">
            <app-icon name="send-up" [size]="13" />
          </button>
        </div>
        @if (error()) {
          <p role="alert" class="mt-1 text-[11.5px] text-[#f3a5a5]">{{ error() }}</p>
        }
      </form>
    } @else {
      <p class="p-4 text-[12.5px] text-[var(--ks-text-muted)]">This chat no longer exists.</p>
    }
  `,
})
export class SessionChatComponent {
  readonly chatId = input.required<string>();
  /** Tight padding for wall tiles. */
  readonly compact = input(false);

  readonly agentChat = inject(AgentChatStore);
  private readonly orch = inject(OrchestratorStore);
  private readonly accounts = inject(AgentAccountsStore);
  private readonly socket = inject(KonsertSocketService);
  private readonly box = viewChild<ElementRef<HTMLTextAreaElement>>('box');

  private static seq = 0;
  readonly inputId = `ks-session-reply-${++SessionChatComponent.seq}`;
  readonly draft = signal('');

  readonly session = computed(() => this.orch.sessions().find((s) => s.id === this.chatId()) ?? null);
  readonly transcript = computed(() => this.agentChat.transcript(this.chatId()));
  readonly busy = computed(() => ['working', 'starting'].includes(this.transcript().status));
  readonly blocked = computed(() => this.transcript().status === 'suspended');
  readonly placeholder = computed(() =>
    this.blocked() ? 'Suspended: resume it to reply' : `Reply to ${this.session()?.agent || 'this agent'}…`,
  );
  readonly error = computed(() => this.agentChat.error());
  readonly account = computed<AgentAccount | null>(() => {
    const s = this.session();
    return s?.worker_id ? (this.accounts.find(s.worker_id, s.account_id || '') ?? null) : null;
  });
  readonly alternatives = computed(() => {
    const s = this.session();
    const a = this.account();
    return s?.worker_id && a ? alternativeAccounts(a, this.accounts.accountsFor(s.worker_id)) : [];
  });

  constructor() {
    // Follow the chat's live events while shown; load what we missed first.
    let stop: (() => void) | null = null;
    effect(() => {
      const id = this.chatId();
      untracked(() => {
        stop?.();
        stop = this.socket.watchChat(id);
        void this.agentChat.open(id);
      });
    });
    inject(DestroyRef).onDestroy(() => stop?.());
  }

  focus(): void {
    this.box()?.nativeElement.focus();
  }

  onKey(ev: KeyboardEvent): void {
    if (ev.key === 'Enter' && !ev.shiftKey && !ev.isComposing) {
      ev.preventDefault();
      this.send();
    }
  }

  send(): void {
    const s = this.session();
    const text = this.draft().trim();
    if (!s || !text || this.blocked()) return;
    if (this.agentChat.send(s, text)) this.draft.set('');
  }

  respond(a: ElicitationAnswer): void {
    this.agentChat.respond(this.chatId(), a.elicitationId, { optionId: a.optionId, text: a.text, label: a.label });
  }
}

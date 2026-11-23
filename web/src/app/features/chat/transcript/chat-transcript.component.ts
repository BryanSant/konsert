import {
  ChangeDetectionStrategy,
  Component,
  ElementRef,
  afterRenderEffect,
  computed,
  input,
  output,
  signal,
  untracked,
  viewChild,
} from '@angular/core';
import { AgentAccount, AgentRelogin, AgentTokenUsage, ChatStatus } from '../../../core/models/agent-chat.models';
import { ElicitationItem, ErrorItem, TranscriptItem } from '../../../core/state/chat-transcript';
import { IconComponent } from '../../../shared/icon.component';
import { MarkdownViewComponent } from '../../../shared/markdown-view.component';
import { ElicitationAnswer, ElicitationCardComponent } from './elicitation-card.component';
import { ErrorCardComponent } from './error-card.component';
import { ToolCallCardComponent } from './tool-call-card.component';

/** Distance from the bottom (px) that still counts as "following" the conversation. */
export const STICK_THRESHOLD_PX = 64;

export function formatTokens(n: number | undefined): string {
  if (!n) return '0';
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return String(n);
}

export function usageLine(u: AgentTokenUsage | undefined): string {
  if (!u) return '';
  const parts = [`${formatTokens(u.input_tokens)} in`, `${formatTokens(u.output_tokens)} out`];
  if (u.cache_read_tokens) parts.push(`${formatTokens(u.cache_read_tokens)} cached`);
  if (u.cost_usd) parts.push(`$${u.cost_usd < 0.01 ? u.cost_usd.toFixed(3) : u.cost_usd.toFixed(2)}`);
  return parts.join(' · ');
}

/**
 * The conversation: user bubbles, streaming assistant text, collapsible thinking, tool
 * cards, plans, usage, error cards, and pending elicitations at the bottom. Follows new
 * output unless the user scrolled up, then offers "Jump to latest".
 */
@Component({
  selector: 'ks-chat-transcript',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, MarkdownViewComponent, ToolCallCardComponent, ElicitationCardComponent, ErrorCardComponent],
  host: {
    class: 'block relative min-h-0',
  },
  template: `
    <div
      #scroller
      class="h-full overflow-y-auto px-4 sm:px-6 py-6"
      role="log"
      aria-label="Conversation"
      aria-live="polite"
      aria-relevant="additions"
      [attr.aria-busy]="status() === 'working'"
      (scroll)="onScroll()"
    >
      <div class="max-w-3xl mx-auto space-y-4">
        @for (it of flowItems(); track it.key) {
          @switch (it.type) {
            @case ('user') {
              <div class="flex justify-end">
                <div
                  class="max-w-[85%] rounded-[18px] rounded-br-[6px] bg-[#25272d] px-4 py-2.5 text-[13.5px] leading-relaxed text-[#f4f4f5]"
                  [class.opacity-70]="it.pending"
                  data-testid="user-bubble"
                >
                  @if (it.text) {
                    <div class="whitespace-pre-wrap break-words">{{ it.text }}</div>
                  }
                  @if (it.files.length) {
                    <div class="flex flex-wrap gap-1.5 pt-1.5">
                      @for (f of it.files; track f) {
                        <span class="inline-flex items-center gap-1 rounded-full bg-black/25 px-2 py-0.5 text-[11px] font-mono-ide text-[#c8cad0]">
                          <app-icon name="paperclip" [size]="10" />
                          <span class="max-w-[200px] truncate">{{ basename(f) }}</span>
                        </span>
                      }
                    </div>
                  }
                </div>
              </div>
            }
            @case ('assistant') {
              <div
                class="text-[13.5px] leading-relaxed"
                [class.pl-3]="questionTurns().has(it.turnId)"
                [class.border-l-2]="questionTurns().has(it.turnId)"
                [class.border-[var(--ks-amber)]]="questionTurns().has(it.turnId)"
                data-testid="assistant-message"
                [attr.data-streaming]="it.streaming"
              >
                <app-markdown-view [content]="it.text" />
                @if (it.streaming) {
                  <span class="inline-block h-3.5 w-1.5 translate-y-0.5 rounded-sm bg-[var(--ks-text-secondary)] ks-pulse" aria-hidden="true"></span>
                }
              </div>
            }
            @case ('thought') {
              <div class="text-[12px]">
                <button
                  type="button"
                  class="ks-btn ks-btn-sm -ml-2 gap-1.5 text-[var(--ks-text-muted)]"
                  [attr.aria-expanded]="openThoughts().has(it.key)"
                  (click)="toggleThought(it.key)"
                >
                  <app-icon name="brain" [size]="12" />
                  {{ it.streaming ? 'Thinking…' : 'Thought' }}
                  <span class="transition-transform" [class.rotate-90]="openThoughts().has(it.key)"><app-icon name="chevron-right" [size]="10" /></span>
                </button>
                @if (openThoughts().has(it.key)) {
                  <div class="mt-1 border-l-2 border-[var(--ks-hairline)] pl-3 text-[12.5px] leading-relaxed text-[var(--ks-text-secondary)] whitespace-pre-wrap break-words">{{ it.text }}</div>
                }
              </div>
            }
            @case ('tool') {
              <ks-tool-call-card [call]="it.call" />
            }
            @case ('plan') {
              <div class="rounded-[var(--ks-radius-lg)] bg-[var(--ks-surface-1)] px-3.5 py-3" data-testid="plan">
                <div class="mb-2 flex items-center gap-1.5 text-[11px] font-semibold uppercase tracking-wide text-[var(--ks-text-muted)]">
                  <app-icon name="list-todo" [size]="12" />
                  Plan
                </div>
                <ul class="space-y-1.5">
                  @for (p of it.items; track $index) {
                    <li class="flex items-start gap-2 text-[12.5px]">
                      <span class="mt-0.5 shrink-0">
                        @switch (p.status) {
                          @case ('completed') {
                            <span class="text-[var(--ks-green)]" role="img" aria-label="Done"><app-icon name="check-circle" [size]="13" /></span>
                          }
                          @case ('in_progress') {
                            <span class="ks-spinner text-[var(--ks-blue)] mt-0.5" style="width: 11px; height: 11px" role="img" aria-label="In progress"></span>
                          }
                          @default {
                            <span class="mt-[3px] block h-2.5 w-2.5 rounded-full border border-[var(--ks-text-muted)]" role="img" aria-label="To do"></span>
                          }
                        }
                      </span>
                      <span [class.text-[var(--ks-text-muted)]]="p.status === 'completed'" [class.line-through]="p.status === 'completed'">{{ p.text }}</span>
                    </li>
                  }
                </ul>
              </div>
            }
            @case ('elicitation') {
              <ks-elicitation-card
                [item]="it"
                [accountLabel]="accountLabel()"
                [relogin]="relogin()"
                [workerId]="workerId()"
                [accountId]="accountId()"
                (answer)="answer.emit($event)"
              />
            }
            @case ('error') {
              @if (!it.dismissed) {
                <ks-error-card
                  [error]="it.error"
                  [agent]="agent()"
                  [accountLabel]="accountLabel()"
                  [workerId]="workerId()"
                  [accountId]="accountId()"
                  [relogin]="relogin()"
                  [alternatives]="alternatives()"
                  (retry)="retry.emit()"
                  (switchAccount)="switchAccount.emit($event)"
                  (keepWaiting)="keepWaiting.emit(it.key)"
                  (stop)="stop.emit()"
                />
              }
            }
            @case ('turn_end') {
              @if (it.repliedWithQuestion || it.usage || it.stopReason !== 'end_turn') {
                <div class="flex flex-wrap items-center gap-2 text-[11px] text-[var(--ks-text-muted)]" data-testid="turn-end">
                  @if (it.repliedWithQuestion) {
                    <span class="inline-flex items-center gap-1 rounded-full bg-[var(--ks-amber-soft)] px-2 py-0.5 font-medium text-[#f0c27a]" data-testid="replied-question">
                      <app-icon name="question" [size]="11" />
                      Replied with a question
                    </span>
                  }
                  @if (it.stopReason === 'cancelled') {
                    <span>Stopped</span>
                  } @else if (it.stopReason === 'max_tokens') {
                    <span>Stopped at the output limit</span>
                  } @else if (it.stopReason === 'refusal') {
                    <span>The agent declined</span>
                  }
                  @if (it.usage) {
                    <span class="font-mono-ide" data-testid="usage">{{ usage(it.usage) }}</span>
                  }
                </div>
              }
            }
          }
        }

        @if (status() === 'working' && !hasStreaming()) {
          <div class="flex items-center gap-2 text-[12px] text-[var(--ks-text-muted)]" data-testid="working-indicator">
            <span class="ks-spinner" style="width: 10px; height: 10px"></span>
            {{ subtitle() || 'Working…' }}
          </div>
        } @else if (status() === 'starting') {
          <div class="flex items-center gap-2 text-[12px] text-[var(--ks-text-muted)]">
            <span class="ks-spinner" style="width: 10px; height: 10px"></span>
            Starting {{ accountLabel() || agent() || 'the agent' }}…
          </div>
        }

        @for (el of pendingCards(); track el.key) {
          <ks-elicitation-card
            [item]="el"
            [accountLabel]="accountLabel()"
            [relogin]="relogin()"
            [workerId]="workerId()"
            [accountId]="accountId()"
            (answer)="answer.emit($event)"
          />
        }
      </div>
    </div>

    @if (!stuck()) {
      <button
        type="button"
        class="absolute bottom-3 left-1/2 -translate-x-1/2 ks-btn ks-btn-sm bg-[var(--ks-surface-3)] text-[var(--ks-text)] shadow-[var(--ks-shadow-popover)] rounded-full"
        data-testid="jump-latest"
        (click)="jumpToLatest()"
      >
        <app-icon name="arrow-down" [size]="12" />
        Jump to latest
      </button>
    }
  `,
})
export class ChatTranscriptComponent {
  readonly items = input<TranscriptItem[]>([]);
  readonly status = input<ChatStatus>('idle');
  readonly subtitle = input('');
  readonly agent = input('');
  readonly accountLabel = input('');
  readonly workerId = input('');
  readonly accountId = input('');
  readonly relogin = input<AgentRelogin | undefined>(undefined);
  readonly alternatives = input<AgentAccount[]>([]);

  readonly answer = output<ElicitationAnswer>();
  readonly retry = output<void>();
  readonly switchAccount = output<AgentAccount>();
  readonly keepWaiting = output<string>();
  readonly stop = output<void>();

  readonly stuck = signal(true);
  readonly openThoughts = signal<Set<string>>(new Set());

  private readonly scrollerRef = viewChild<ElementRef<HTMLElement>>('scroller');
  private scroller(): HTMLElement | null {
    return this.scrollerRef()?.nativeElement ?? null;
  }

  /** Pending elicitations render at the bottom; everything else in order. */
  readonly pendingCards = computed(() =>
    this.items().filter((i): i is ElicitationItem => i.type === 'elicitation' && (i.state === 'pending' || i.state === 'answering')),
  );
  readonly flowItems = computed(() =>
    this.items().filter((i) => !(i.type === 'elicitation' && (i.state === 'pending' || i.state === 'answering'))),
  );
  readonly questionTurns = computed(
    () => new Set(this.items().filter((i) => i.type === 'turn_end' && i.repliedWithQuestion).map((i) => i.turnId)),
  );
  readonly hasStreaming = computed(() =>
    this.items().some((i) => (i.type === 'assistant' || i.type === 'thought') && i.streaming),
  );

  constructor() {
    // Follow new content while the user is at the bottom.
    afterRenderEffect(() => {
      this.items();
      this.status();
      if (untracked(() => this.stuck())) this.scrollToBottom();
    });
  }

  onScroll(): void {
    const el = this.scroller();
    if (!el) return;
    const distance = el.scrollHeight - el.scrollTop - el.clientHeight;
    this.stuck.set(distance <= STICK_THRESHOLD_PX);
  }

  jumpToLatest(): void {
    this.stuck.set(true);
    this.scrollToBottom();
  }

  private scrollToBottom(): void {
    const el = this.scroller();
    if (el) el.scrollTop = el.scrollHeight;
  }

  toggleThought(key: string): void {
    this.openThoughts.update((s) => {
      const next = new Set(s);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }

  usage(u: AgentTokenUsage): string {
    return usageLine(u);
  }

  basename(p: string): string {
    const base = p.split(/[\\/]/).pop() || p;
    const parts = base.split('-');
    if (parts.length >= 3 && /^\d{9,}$/.test(parts[0])) return parts.slice(2).join('-');
    return base;
  }

  isError(i: TranscriptItem): i is ErrorItem {
    return i.type === 'error';
  }
}

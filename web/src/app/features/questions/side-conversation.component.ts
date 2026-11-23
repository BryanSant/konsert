import {
  ChangeDetectionStrategy,
  Component,
  ElementRef,
  afterRenderEffect,
  computed,
  inject,
  input,
  output,
  signal,
  viewChild,
} from '@angular/core';
import { QuestionOption, QuestionView, isPending } from '../../core/models/question.models';
import { QuestionsStore } from '../../core/state/questions.store';
import { sentMessage } from '../../core/state/questions-logic';
import { DictateButtonComponent } from '../../shared/dictate-button.component';
import { IconComponent } from '../../shared/icon.component';
import { MarkdownViewComponent } from '../../shared/markdown-view.component';

/**
 * "Ask about this" (PRD §5.8b): a side conversation with a read-only fork of the asking
 * agent (or the orchestrator when it can't fork). Shows who answers and on which account,
 * the thread, the answerer's draft answer ("Use this answer"), a follow-up input with
 * dictation, and the ORIGINAL OPTIONS pinned at the bottom (still answerable).
 */
@Component({
  selector: 'ks-side-conversation',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, MarkdownViewComponent, DictateButtonComponent],
  host: { class: 'flex flex-col min-h-0 h-full' },
  template: `
    @let q = question();
    <header class="flex items-start gap-2 px-4 pt-3 pb-2 shrink-0">
      <div class="flex-1 min-w-0">
        <h3 class="text-[12.5px] font-semibold text-[var(--ks-text)]">Side conversation</h3>
        <p class="text-[11px] text-[var(--ks-text-muted)]" data-testid="thread-answerer">{{ answererText() }}</p>
      </div>
      <button type="button" class="ks-icon-btn ks-icon-btn-sm" aria-label="Close side conversation" data-testid="thread-close" (click)="closed.emit()">
        <app-icon name="close" [size]="13" />
      </button>
    </header>

    <div #scroller class="flex-1 min-h-0 overflow-y-auto px-4 space-y-2.5 pb-3" data-testid="thread-messages" aria-live="polite">
      @if (!q.messages.length) {
        <p class="text-[12px] text-[var(--ks-text-muted)] pt-2">
          Ask what you need to know before answering — “What is {{ firstRef() || 'this' }}, why now, and why do you recommend it?”
          The agent stays paused; a read-only copy of it answers.
        </p>
      }
      @for (m of q.messages; track m.seq) {
        @if (m.role === 'user') {
          <div class="ml-8 rounded-[var(--ks-radius-lg)] bg-[var(--ks-raised)] px-3 py-2 text-[12.5px] text-[var(--ks-text)]" data-testid="thread-user">{{ m.text }}</div>
        } @else {
          <div class="mr-4 text-[12.5px] text-[var(--ks-text-secondary)]" data-testid="thread-answer">
            <app-markdown-view [content]="m.text" />
          </div>
        }
      }
      @if (q.thread?.status === 'working') {
        <div class="flex items-center gap-2 text-[11.5px] text-[var(--ks-text-muted)]" role="status" data-testid="thread-working">
          <span class="ks-spinner" style="width: 10px; height: 10px"></span>
          {{ q.thread?.answerer === 'orchestrator' ? 'The orchestrator is answering…' : 'The agent is answering…' }}
        </div>
      }
      @if (q.thread?.status === 'failed') {
        <p class="text-[11.5px] text-[var(--ks-red)]" role="alert">{{ q.thread?.note || 'The side conversation failed.' }}</p>
      }
      @if (draft(); as d) {
        @if (pending()) {
          <div class="rounded-[var(--ks-radius-md)] bg-[var(--ks-blue-soft)] px-3 py-2 space-y-1.5" data-testid="thread-draft">
            <div class="text-[10px] font-semibold uppercase tracking-wider text-[var(--ks-blue)]">Suggested answer</div>
            <div class="text-[12.5px] text-[var(--ks-text)]">{{ draftText() }}</div>
            <button type="button" class="ks-btn ks-btn-sm ks-btn-primary" data-testid="thread-use-draft" [disabled]="store.isBusy(q.id)" (click)="useDraft()">Use this answer</button>
          </div>
        }
      }
      @if (q.answer && !pending()) {
        <p class="flex items-start gap-1.5 text-[12px] text-[var(--ks-green)]" data-testid="thread-receipt" role="status">
          <app-icon name="check" [size]="13" /><span>{{ receipt() }}</span>
        </p>
      }
    </div>

    @if (pending()) {
      <form class="shrink-0 flex items-end gap-1.5 px-3 pb-2" (submit)="$event.preventDefault(); send()">
        <label class="ks-sr-only" for="ks-thread-input">Ask about this question</label>
        <textarea
          #input
          id="ks-thread-input"
          rows="1"
          data-testid="thread-input"
          class="ks-field flex-1 min-w-0 resize-none text-[12.5px] py-2 max-h-32"
          [placeholder]="q.messages.length ? 'Ask a follow-up…' : 'What would you like to know?'"
          [value]="draftInput()"
          (input)="draftInput.set($any($event.target).value)"
          (keydown.enter)="onEnter($event)"
          [disabled]="q.thread?.status === 'working'"
        ></textarea>
        <ks-dictate-button [base]="draftInput()" (text)="draftInput.set($event)" />
        <button type="submit" class="ks-send-btn" aria-label="Ask" data-testid="thread-send" [disabled]="!draftInput().trim() || q.thread?.status === 'working' || store.isBusy(q.id)">
          <app-icon name="send-up" [size]="14" />
        </button>
      </form>

      <!-- The original options stay pinned and answerable. -->
      <section class="shrink-0 border-t border-[var(--ks-hairline)] px-3 pt-2 pb-3 space-y-1.5" aria-label="Original options" data-testid="thread-pinned-options">
        <div class="text-[10px] font-semibold uppercase tracking-wider text-[var(--ks-text-muted)]">Original options</div>
        <div class="flex flex-wrap gap-1.5">
          @for (o of q.options ?? []; track o.id) {
            <button
              type="button"
              class="ks-btn ks-btn-sm"
              [class.ks-btn-soft]="!o.recommended"
              [class.bg-[var(--ks-green-soft)]]="o.recommended"
              [attr.data-testid]="'thread-option-' + o.id"
              [disabled]="store.isBusy(q.id)"
              (click)="choose(o)"
            >
              {{ o.label }}
              @if (o.recommended) {
                <span class="text-[10px] text-[var(--ks-green)]">Recommended</span>
              }
            </button>
          }
        </div>
      </section>
    }
  `,
})
export class SideConversationComponent {
  readonly store = inject(QuestionsStore);
  readonly question = input.required<QuestionView>();
  readonly closed = output<void>();

  readonly draftInput = signal('');
  private readonly scroller = viewChild<ElementRef<HTMLElement>>('scroller');
  private readonly inputEl = viewChild<ElementRef<HTMLTextAreaElement>>('input');

  readonly pending = computed(() => isPending(this.question()));
  readonly draft = computed(() => this.question().thread?.draft);
  readonly draftText = computed(() => {
    const d = this.draft();
    if (!d) return '';
    return d.label && d.text ? `${d.label} — ${d.text}` : d.label || d.text || '';
  });
  readonly receipt = computed(() => sentMessage(this.question()));
  readonly firstRef = computed(() => this.question().refs?.find((r) => r.kind === 'ticket')?.ref ?? '');
  readonly answererText = computed(() => {
    const q = this.question();
    const t = q.thread;
    const where = [q.thread_account_label || t?.account_id || q.account_label, q.thread_machine_name || q.machine_name].filter(Boolean).join(' on ');
    if (!t || t.discarded) {
      return `Answered by a read-only copy of the asking agent${where ? ` (${where})` : ''}, or the orchestrator if it can't be copied.`;
    }
    if (t.answerer === 'fork') return `Read-only copy of ${q.agent || 'the agent'}'s session · ${where}`;
    return `The orchestrator (${t.agent || 'agent'} · ${where})${t.note ? ` — ${t.note}` : ''}`;
  });

  constructor() {
    let lastCount = -1;
    afterRenderEffect(() => {
      const n = this.question().messages.length + (this.question().thread?.status === 'working' ? 1 : 0);
      const el = this.scroller()?.nativeElement;
      if (el && n !== lastCount) {
        el.scrollTop = el.scrollHeight;
        lastCount = n;
      }
    });
    afterRenderEffect(() => {
      // Focus the input when the conversation opens with nothing asked yet.
      if (this.question().messages.length === 0) this.inputEl()?.nativeElement.focus();
    });
  }

  onEnter(ev: Event): void {
    const k = ev as KeyboardEvent;
    if (k.shiftKey || k.isComposing) return;
    ev.preventDefault();
    this.send();
  }

  send(): void {
    const t = this.draftInput().trim();
    if (!t) return;
    void this.store.ask(this.question(), t).then((ok) => {
      if (ok) this.draftInput.set('');
    });
  }

  useDraft(): void {
    const d = this.draft();
    if (!d) return;
    void this.store.answer(this.question(), d.option_id ? { option_id: d.option_id, text: d.text } : { text: d.text });
  }

  choose(o: QuestionOption): void {
    void this.store.answer(this.question(), { option_id: o.id });
  }
}

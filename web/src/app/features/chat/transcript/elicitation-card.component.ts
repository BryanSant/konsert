import { ChangeDetectionStrategy, Component, computed, inject, input, output, signal } from '@angular/core';
import { isPending } from '../../../core/models/question.models';
import { QuestionsStore } from '../../../core/state/questions.store';
import { QuestionCardComponent } from '../../questions/question-card.component';
import { AgentElicitationOption, AgentRelogin } from '../../../core/models/agent-chat.models';
import { ElicitationItem } from '../../../core/state/chat-transcript';
import { IconComponent } from '../../../shared/icon.component';
import { MarkdownViewComponent } from '../../../shared/markdown-view.component';
import { ReloginHelpComponent } from '../relogin-help.component';
import { ToolCallCardComponent } from './tool-call-card.component';

export interface ElicitationAnswer {
  elicitationId: string;
  optionId?: string;
  text?: string;
  label: string;
}

/** Inline "the agent needs you" card: permission, question or sign-in. */
@Component({
  selector: 'ks-elicitation-card',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, ToolCallCardComponent, ReloginHelpComponent, MarkdownViewComponent, QuestionCardComponent],
  template: `
    @let el = item().elicitation;
    @if (linked(); as q) {
      <!-- A Konsert question: the same card as the Questions panel (stays until answered). -->
      <div [attr.data-elicitation-kind]="el.kind" [attr.data-elicitation-state]="item().state" class="space-y-2">
        @if (el.tool_call) {
          <ks-tool-call-card [call]="el.tool_call" />
        }
        <ks-question-card [question]="q" [compact]="true" />
      </div>
    } @else if (item().state === 'resolved' || item().state === 'expired') {
      <div class="flex items-center gap-2 px-1 text-[11.5px] text-[var(--ks-text-muted)]" [attr.data-elicitation-state]="item().state">
        <app-icon [name]="icon()" [size]="12" />
        <span class="truncate">{{ el.title }}</span>
        <span>·</span>
        <span>{{ item().state === 'expired' ? 'No longer needed' : item().answer || 'Answered' }}</span>
      </div>
    } @else {
      <section
        class="rounded-[var(--ks-radius-lg)] bg-[var(--ks-amber-soft)] p-3.5 space-y-3"
        [attr.aria-labelledby]="titleId"
        [attr.data-elicitation-kind]="el.kind"
        [attr.data-elicitation-state]="item().state"
      >
        <div class="flex items-start gap-2.5">
          <span class="mt-0.5 text-[var(--ks-amber)] shrink-0"><app-icon [name]="icon()" [size]="15" /></span>
          <div class="flex-1 min-w-0">
            <div class="text-[10.5px] font-semibold uppercase tracking-wide text-[#e9bd76]">{{ kicker() }}</div>
            <h2 [id]="titleId" class="text-[13px] font-semibold text-[var(--ks-text)] leading-snug">{{ headline() }}</h2>
            @if (el.detail) {
              <div class="mt-1 text-[12px] text-[var(--ks-text-secondary)]">
                <app-markdown-view [content]="el.detail" />
              </div>
            }
          </div>
        </div>

        @if (el.tool_call) {
          <ks-tool-call-card [call]="el.tool_call" />
        }

        @if (el.kind === 'auth') {
          <ks-relogin-help [relogin]="relogin()" [workerId]="workerId()" [accountId]="accountId()" />
        }

        @if (el.options?.length) {
          <div class="flex flex-wrap gap-1.5" role="group" [attr.aria-label]="'Answer: ' + el.title">
            @for (o of el.options; track o.id) {
              <button
                type="button"
                class="ks-btn ks-btn-sm"
                [class.ks-btn-primary]="o.kind === 'allow_once' || (el.kind === 'question' && $first)"
                [class.ks-btn-soft]="o.kind !== 'allow_once' && !isReject(o) && !(el.kind === 'question' && $first)"
                [class.ks-btn-danger]="isReject(o)"
                [disabled]="item().state === 'answering'"
                [attr.data-option]="o.id"
                (click)="choose(o)"
              >
                {{ o.label }}
              </button>
            }
          </div>
        }

        @if (el.allow_text) {
          <form class="flex items-center gap-1.5" (submit)="$event.preventDefault(); sendText()">
            <label class="ks-sr-only" [attr.for]="textId">Your answer</label>
            <input
              [id]="textId"
              type="text"
              data-testid="elicitation-text"
              class="flex-1 min-w-0 h-8 rounded-[var(--ks-radius-md)] bg-black/30 px-3 text-[12.5px] text-[var(--ks-text)] placeholder-[#8a8c94] outline-none focus-visible:shadow-[var(--ks-focus-ring)]"
              placeholder="Type an answer…"
              [value]="text()"
              (input)="text.set($any($event.target).value)"
              [disabled]="item().state === 'answering'"
            />
            <button type="submit" class="ks-btn ks-btn-sm ks-btn-soft" [disabled]="!text().trim() || item().state === 'answering'">Send</button>
          </form>
        }

        @if (item().state === 'answering') {
          <div class="flex items-center gap-2 text-[11.5px] text-[var(--ks-text-secondary)]" role="status">
            <span class="ks-spinner" style="width: 10px; height: 10px"></span>
            Sending "{{ item().answer }}"…
          </div>
        }
      </section>
    }
  `,
})
export class ElicitationCardComponent {
  readonly item = input.required<ElicitationItem>();
  /** For auth elicitations: who and where. */
  readonly accountLabel = input('');
  readonly relogin = input<AgentRelogin | undefined>(undefined);
  readonly workerId = input('');
  readonly accountId = input('');
  readonly answer = output<ElicitationAnswer>();

  readonly text = signal('');
  private readonly questions = inject(QuestionsStore);
  /** The hub question this card shows, while it is pending (or just answered here). */
  readonly linked = computed(() => {
    const q = this.questions.question(this.item().elicitation.question_id);
    if (!q) return null;
    return isPending(q) || this.item().state === 'pending' || this.item().state === 'answering' ? q : null;
  });
  private static n = 0;
  private readonly uid = ++ElicitationCardComponent.n;
  readonly titleId = `ks-elicit-title-${this.uid}`;
  readonly textId = `ks-elicit-text-${this.uid}`;

  readonly icon = computed(() => {
    switch (this.item().elicitation.kind) {
      case 'permission':
        return 'hand';
      case 'auth':
        return 'key';
      default:
        return 'question';
    }
  });
  readonly kicker = computed(() => {
    switch (this.item().elicitation.kind) {
      case 'permission':
        return 'Permission needed';
      case 'auth':
        return 'Sign-in needed';
      default:
        return 'Question';
    }
  });
  readonly headline = computed(() => {
    const el = this.item().elicitation;
    if (el.kind === 'auth') return `${this.accountLabel() || 'This account'} needs you to sign in again`;
    return el.title;
  });

  isReject(o: AgentElicitationOption): boolean {
    return (o.kind ?? '').startsWith('reject') || /^(deny|reject)/i.test(o.label);
  }

  choose(o: AgentElicitationOption): void {
    this.answer.emit({ elicitationId: this.item().elicitation.id, optionId: o.id, label: o.label });
  }

  sendText(): void {
    const t = this.text().trim();
    if (!t) return;
    this.answer.emit({ elicitationId: this.item().elicitation.id, text: t, label: t });
    this.text.set('');
  }
}

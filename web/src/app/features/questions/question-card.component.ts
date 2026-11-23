import { ChangeDetectionStrategy, Component, computed, inject, input, signal } from '@angular/core';
import { QuestionOption, QuestionRef, QuestionView, isPending } from '../../core/models/question.models';
import { QuestionsStore } from '../../core/state/questions.store';
import { answerLabel, formatSince, isSnoozed, sentMessage } from '../../core/state/questions-logic';
import { IconComponent } from '../../shared/icon.component';
import { MarkdownViewComponent } from '../../shared/markdown-view.component';
import { PopoverComponent, PopoverTriggerDirective } from '../../shared/popover.component';
import { TooltipDirective } from '../../shared/tooltip.directive';

/**
 * One question (PRD §5.8b): who asks and what is waiting, the question, its CONTEXT · WHY
 * NOW, reference cards, the options with what each one does (the recommendation marked),
 * Other… free text, Ask about this, Let the orchestrator decide and Snooze. Answering
 * shows the receipt; nothing is dismissed until it is answered. Used in the Questions
 * panel and inline in the asking chat (`compact`).
 */
@Component({
  selector: 'ks-question-card',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, MarkdownViewComponent, TooltipDirective, PopoverComponent, PopoverTriggerDirective],
  host: { class: 'block' },
  template: `
    @let q = question();
    <article
      class="rounded-[var(--ks-radius-lg)] bg-[var(--ks-surface-2)] p-4 space-y-3.5"
      [class.ring-1]="pending()"
      [class.ring-[var(--ks-amber-soft)]]="pending()"
      data-testid="question-card"
      [attr.data-question]="q.sid"
      [attr.data-status]="q.status"
      [attr.aria-labelledby]="titleId"
    >
      @if (!compact()) {
        <header class="flex flex-wrap items-center gap-x-2 gap-y-1 text-[11.5px] text-[var(--ks-text-muted)]">
          <span class="inline-flex items-center gap-1.5 text-[var(--ks-text-secondary)]">
            <app-icon [name]="q.orchestrator ? 'orchestrator' : q.agent || 'question'" [size]="13" />
            <span class="font-medium">{{ q.orchestrator ? 'Orchestrator' : agentName() }}</span>
          </span>
          @if (q.chat_title) {
            <span>·</span><span class="truncate max-w-[220px]" [ksTooltip]="q.chat_sid || ''">{{ q.chat_title }}</span>
          }
          @if (q.machine_name) {
            <span>·</span><span>{{ q.machine_name }}</span>
          }
          @if (q.account_label && q.account_label !== q.agent) {
            <span>·</span><span>{{ q.account_label }}</span>
          }
          <span class="flex-1"></span>
          @if (pending()) {
            <span class="inline-flex items-center gap-1 text-[var(--ks-amber)]" data-testid="question-blocked">
              <app-icon name="clock" [size]="11" />
              blocked {{ blockedFor() }}
            </span>
          }
        </header>
      }

      <div class="space-y-1">
        <div class="flex items-center gap-1.5 text-[10.5px] font-semibold uppercase tracking-wide text-[#e9bd76]">
          <app-icon [name]="q.kind === 'permission' ? 'hand' : 'question'" [size]="12" />
          {{ q.kind === 'permission' ? 'Permission needed' : 'Question' }}
          <span class="font-mono-ide normal-case tracking-normal text-[var(--ks-text-muted)]">{{ q.sid }}</span>
          @if (q.ask_count > 1) {
            <span class="normal-case tracking-normal font-normal text-[var(--ks-text-muted)]" ksTooltip="The agent asked this again; it's the same question">asked {{ q.ask_count }}×</span>
          }
          @if (snoozed()) {
            <span class="normal-case tracking-normal font-normal text-[var(--ks-text-muted)]">· snoozed</span>
          }
        </div>
        <h2 [id]="titleId" class="text-[15px] font-semibold leading-snug text-[var(--ks-text)]" data-testid="question-title">{{ q.prompt }}</h2>
        @if (waiting().length) {
          <p class="text-[11.5px] text-[var(--ks-text-muted)]" data-testid="question-waiting">Waiting: {{ waiting().join(' · ') }}</p>
        }
      </div>

      @if (q.context || q.why_now) {
        <section class="rounded-[var(--ks-radius-md)] bg-black/20 px-3 py-2.5 space-y-2 text-[12.5px] text-[var(--ks-text-secondary)]" data-testid="question-context">
          <div class="text-[10px] font-semibold uppercase tracking-wider text-[var(--ks-text-muted)]">Context · Why now</div>
          @if (q.context) {
            <app-markdown-view [content]="q.context" />
          }
          @if (q.why_now) {
            <div class="flex gap-1.5"><span class="shrink-0 font-medium text-[var(--ks-text)]">Why now:</span><app-markdown-view [content]="q.why_now" /></div>
          }
        </section>
      }

      @if (refs().length) {
        <ul class="flex flex-wrap gap-1.5" data-testid="question-refs" aria-label="References">
          @for (r of refs(); track r.kind + r.ref) {
            <li>
              <button
                type="button"
                class="ks-btn ks-btn-sm max-w-full !h-auto min-h-[26px] py-1 text-left"
                [class.ks-btn-soft]="r.resolved"
                [attr.aria-expanded]="openRef() === r.ref"
                [attr.data-ref]="r.ref"
                (click)="openRef.set(openRef() === r.ref ? '' : r.ref)"
              >
                <app-icon [name]="refIcon(r)" [size]="11" />
                <span class="font-mono-ide">{{ r.ref }}</span>
                @if (r.title && r.title !== r.ref) {
                  <span class="truncate max-w-[240px]">{{ r.title }}</span>
                }
                @if (r.status) {
                  <span class="text-[10px] text-[var(--ks-text-muted)]">{{ r.status }}</span>
                }
              </button>
              @if (openRef() === r.ref && (r.excerpt || r.url)) {
                <div class="mt-1 max-w-[440px] rounded-[var(--ks-radius-md)] bg-[var(--ks-surface-3)] px-2.5 py-2 text-[11.5px] text-[var(--ks-text-secondary)]" data-testid="question-ref-detail">
                  {{ r.excerpt }}
                  @if (r.url) {
                    <a class="block mt-1 text-[var(--ks-blue)] underline" [href]="r.url" target="_blank" rel="noopener">Open</a>
                  }
                </div>
              }
            </li>
          }
        </ul>
      }

      @if (delegationText(); as d) {
        <p class="flex items-start gap-1.5 rounded-[var(--ks-radius-md)] bg-[var(--ks-violet-soft)] px-3 py-2 text-[12px] text-[var(--ks-text)]" data-testid="question-delegation" role="status">
          <app-icon name="orchestrator" [size]="13" />
          <span>{{ d }}</span>
        </p>
      }

      @if (q.options?.length) {
        <div class="space-y-1.5" role="group" [attr.aria-label]="'Answer: ' + q.prompt">
          @for (o of q.options; track o.id; let i = $index) {
            <button
              type="button"
              class="ks-focusable w-full text-left rounded-[var(--ks-radius-md)] px-3 py-2 transition-colors duration-100 hover:bg-[var(--ks-raised-strong)] disabled:opacity-50 disabled:hover:bg-transparent"
              [class.bg-[var(--ks-green-soft)]]="o.recommended"
              [class.bg-[var(--ks-raised)]]="!o.recommended"
              [class.ring-2]="chosen(o)"
              [class.ring-[var(--ks-blue)]]="chosen(o)"
              [disabled]="!canAnswer() || store.isBusy(q.id)"
              [attr.data-option]="o.id"
              [attr.data-testid]="'question-option-' + o.id"
              (click)="choose(o)"
            >
              <span class="flex items-center gap-2">
                @if (i < 9 && !compact()) {
                  <span class="ks-kbd">{{ i + 1 }}</span>
                }
                <span class="text-[13px] font-medium text-[var(--ks-text)]">{{ o.label }}</span>
                @if (o.recommended) {
                  <span class="rounded-full bg-[var(--ks-green-soft)] px-1.5 py-px text-[10px] font-semibold text-[var(--ks-green)]">Recommended</span>
                }
              </span>
              @if (o.consequence) {
                <span class="mt-0.5 block text-[11.5px] text-[var(--ks-text-secondary)]">{{ o.consequence }}</span>
              }
              @if (o.recommended && q.rationale) {
                <span class="mt-0.5 block text-[11.5px] text-[var(--ks-green)]/90">Why: {{ q.rationale }}</span>
              }
            </button>
          }
        </div>
      }

      @if (q.allow_text || !q.options?.length) {
        @if (otherOpen() || !q.options?.length) {
          <form class="flex items-center gap-1.5" (submit)="$event.preventDefault(); sendText()">
            <label class="ks-sr-only" [attr.for]="textId">Your answer</label>
            <input
              [id]="textId"
              type="text"
              data-testid="question-other-input"
              class="ks-field flex-1 min-w-0 h-8 text-[12.5px]"
              placeholder="Type your own answer…"
              [value]="text()"
              (input)="text.set($any($event.target).value)"
              [disabled]="!canAnswer() || store.isBusy(q.id)"
            />
            <button type="submit" class="ks-btn ks-btn-sm ks-btn-soft" data-testid="question-other-send" [disabled]="!text().trim() || !canAnswer() || store.isBusy(q.id)">Send</button>
          </form>
        }
      }

      @if (pending() || canAnswer()) {
        <div class="flex flex-wrap items-center gap-1" data-testid="question-actions">
          @if (q.allow_text && q.options?.length && !otherOpen()) {
            <button type="button" class="ks-btn ks-btn-sm" data-testid="question-other" (click)="otherOpen.set(true)">Other…</button>
          }
          <button type="button" class="ks-btn ks-btn-sm" data-testid="question-ask" (click)="askAbout()" ksTooltip="Ask the agent (a read-only copy of it) about this first (?)">
            <app-icon name="question" [size]="12" />
            Ask about this
          </button>
          @if (q.status === 'open') {
            <button type="button" class="ks-btn ks-btn-sm" data-testid="question-delegate" [disabled]="store.isBusy(q.id)" (click)="store.delegate(q)" ksTooltip="The orchestrator decides and tells you why; you can override it (o)">
              <app-icon name="orchestrator" [size]="12" />
              Let the orchestrator decide
            </button>
          }
          @if (pending()) {
            <ks-popover label="Snooze" placement="bottom-start" [(open)]="snoozeOpen">
              <button ksPopoverTrigger type="button" class="ks-btn ks-btn-sm" data-testid="question-snooze">
                <app-icon name="clock" [size]="12" />
                {{ snoozed() ? 'Snoozed' : 'Snooze' }}
              </button>
              @for (s of snoozeChoices; track s.minutes) {
                <button role="menuitem" type="button" class="ks-menu-item" [attr.data-snooze]="s.minutes" (click)="store.snooze(q, s.minutes)">{{ s.label }}</button>
              }
            </ks-popover>
          }
          @if (compact()) {
            <span class="flex-1"></span>
            <button type="button" class="ks-btn ks-btn-sm" data-testid="question-open-queue" (click)="store.open(q.id)">Open in Questions</button>
          }
        </div>
      }

      @if (q.answer && !pending()) {
        <p class="flex items-start gap-1.5 text-[12px] text-[var(--ks-green)]" data-testid="question-receipt" role="status">
          <app-icon name="check" [size]="13" />
          <span>{{ receipt() }}</span>
        </p>
      } @else if (!pending()) {
        <p class="text-[12px] text-[var(--ks-text-muted)]" data-testid="question-receipt">{{ closedText() }}</p>
      }
    </article>
  `,
})
export class QuestionCardComponent {
  readonly store = inject(QuestionsStore);
  readonly question = input.required<QuestionView>();
  /** Inline in the asking chat: no asker header, no key hints. */
  readonly compact = input(false);

  readonly text = signal('');
  readonly otherOpen = signal(false);
  readonly openRef = signal('');
  snoozeOpen = false;
  readonly snoozeChoices = [
    { minutes: 15, label: 'For 15 minutes' },
    { minutes: 60, label: 'For 1 hour' },
    { minutes: 240, label: 'For 4 hours' },
    { minutes: 0, label: 'Unsnooze' },
  ];

  private static n = 0;
  private readonly uid = ++QuestionCardComponent.n;
  readonly titleId = `ks-question-title-${this.uid}`;
  readonly textId = `ks-question-text-${this.uid}`;

  readonly pending = computed(() => isPending(this.question()));
  readonly canAnswer = computed(() => this.store.canAnswer(this.question()));
  readonly snoozed = computed(() => isSnoozed(this.question(), this.store.now()));
  readonly blockedFor = computed(() => formatSince(this.question().blocked_since, this.store.now()));
  readonly refs = computed(() => this.question().refs ?? []);
  readonly agentName = computed(() => {
    const a = this.question().agent || 'agent';
    return a.charAt(0).toUpperCase() + a.slice(1);
  });
  readonly waiting = computed(() => this.question().blocks ?? []);
  readonly receipt = computed(() => {
    const q = this.question();
    const base = sentMessage(q);
    if (q.answer?.by?.actor === 'orchestrator' && q.delegation?.rationale) return `${base}. Its reason: ${q.delegation.rationale}`;
    return base;
  });
  readonly closedText = computed(() => {
    switch (this.question().status) {
      case 'cancelled':
        return 'Cancelled: the turn was stopped.';
      case 'expired':
        return 'No longer needed: the agent moved on.';
    }
    return '';
  });
  readonly delegationText = computed(() => {
    const q = this.question();
    const d = q.delegation;
    if (!d) return '';
    const choice = d.label || d.text;
    switch (d.state) {
      case 'pending':
        return d.sent_at ? 'The orchestrator is deciding…' : 'Waiting for the orchestrator to finish its current turn…';
      case 'decided': {
        const secs = d.deliver_at ? Math.max(0, Math.round((Date.parse(d.deliver_at) - this.store.now()) / 1000)) : 0;
        return `The orchestrator chose “${choice}”: ${d.rationale ?? ''} It goes to the agent${secs ? ` in about ${secs}s` : ' shortly'}; pick another option to override.`;
      }
      case 'failed':
        return d.note || 'The orchestrator could not decide; the question is back with you.';
      case 'overridden':
        return `You overrode the orchestrator's choice${choice ? ` (“${choice}”)` : ''}.`;
    }
    return '';
  });

  chosen(o: QuestionOption): boolean {
    const a = this.question().answer;
    return !!a && a.option_id === o.id && !isPending(this.question());
  }

  refIcon(r: QuestionRef): string {
    switch (r.kind) {
      case 'ticket':
        return 'list-todo';
      case 'pr':
        return 'git-branch';
      case 'file':
        return 'file';
      case 'machine':
        return 'server';
      case 'question':
        return 'question';
      default:
        return 'tasks';
    }
  }

  choose(o: QuestionOption): void {
    void this.store.answer(this.question(), { option_id: o.id });
  }

  sendText(): void {
    const t = this.text().trim();
    if (!t) return;
    void this.store.answer(this.question(), { text: t }).then((ok) => {
      if (ok) this.text.set('');
    });
  }

  askAbout(): void {
    const q = this.question();
    if (!this.store.panelOpen() || this.store.selectedId() !== q.id) this.store.open(q.id);
    this.store.threadOpen.set(true);
  }

  protected readonly answerLabel = answerLabel;
}

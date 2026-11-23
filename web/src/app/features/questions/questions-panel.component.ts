import { DOCUMENT } from '@angular/common';
import { ChangeDetectionStrategy, Component, OnDestroy, OnInit, computed, inject, signal } from '@angular/core';
import { QuestionView, isPending } from '../../core/models/question.models';
import { QuestionsStore } from '../../core/state/questions.store';
import { formatSince, fromTextField, isSnoozed, queueKey } from '../../core/state/questions-logic';
import { IconComponent } from '../../shared/icon.component';
import { QuestionCardComponent } from './question-card.component';
import { SideConversationComponent } from './side-conversation.component';

/**
 * The Questions panel (PRD §5.8b): every open question across the fleet in one place,
 * sorted by blocking impact and grouped by ticket/chat, the selected question's card, and
 * its side conversation. Keyboard: j/k move, 1–9 pick an option, ? ask about it, o let
 * the orchestrator decide, Esc closes. On a phone: the inbox, a full-screen question, and
 * the side conversation as a bottom sheet with the options pinned.
 */
@Component({
  selector: 'ks-questions-panel',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, QuestionCardComponent, SideConversationComponent],
  template: `
    <div class="fixed inset-0 z-40 flex" role="dialog" aria-modal="true" aria-labelledby="ks-questions-heading" data-testid="questions-panel">
      <div class="ks-backdrop absolute inset-0 hidden md:block" (click)="store.close()"></div>
      <div
        class="ks-modal relative z-10 flex h-full w-full flex-col bg-[var(--ks-bg-chrome)] md:m-auto md:h-[min(860px,92vh)] md:w-[min(1320px,95vw)] md:rounded-[var(--ks-radius-xl)] overflow-hidden"
      >
        <header class="flex items-center gap-2 h-11 px-3 shrink-0 border-b border-[var(--ks-hairline)]">
          @if (mobileView() === 'question') {
            <button type="button" class="ks-icon-btn md:hidden" aria-label="Back to questions" data-testid="questions-back" (click)="mobileView.set('inbox')">
              <span class="inline-flex rotate-180"><app-icon name="chevron-right" [size]="15" /></span>
            </button>
          }
          <app-icon name="question" [size]="15" />
          <h2 id="ks-questions-heading" class="text-[13px] font-semibold text-[var(--ks-text)]">Questions</h2>
          <span class="font-mono-ide text-[11px] text-[var(--ks-text-muted)]" data-testid="questions-count">{{ store.pendingCount() }} open</span>
          <span class="flex-1"></span>
          <span class="hidden md:inline text-[10.5px] text-[var(--ks-text-muted)]">
            <span class="ks-kbd">j</span>/<span class="ks-kbd">k</span> move · <span class="ks-kbd">1</span>–<span class="ks-kbd">9</span> answer ·
            <span class="ks-kbd">?</span> ask · <span class="ks-kbd">o</span> orchestrator decides
          </span>
          <button type="button" class="ks-icon-btn" aria-label="Close questions" data-testid="questions-close" (click)="store.close()">
            <app-icon name="close" [size]="15" />
          </button>
        </header>

        @if (store.error()) {
          <div role="alert" class="px-4 py-2 text-[12px] text-[#f3a5a5] bg-[var(--ks-red-soft)] shrink-0" data-testid="questions-error">{{ store.error() }}</div>
        }

        <div class="flex flex-1 min-h-0">
          <!-- Queue -->
          <nav
            aria-label="Question queue"
            class="flex-col min-h-0 w-full md:flex md:w-[320px] md:shrink-0 md:border-r md:border-[var(--ks-hairline)] overflow-y-auto"
            [class.flex]="mobileView() === 'inbox'"
            [class.hidden]="mobileView() !== 'inbox'"
            data-testid="questions-queue"
          >
            @if (store.groups().length === 0) {
              <p class="p-5 text-[12.5px] text-[var(--ks-text-muted)]" data-testid="questions-empty">
                No open questions. When an agent anywhere in the fleet needs a decision, it shows up here.
              </p>
            }
            @for (g of store.groups(); track g.key) {
              <section class="py-1.5" [attr.data-group]="g.key">
                <h3 class="px-3 pt-1.5 pb-1 text-[10.5px] font-semibold uppercase tracking-wider text-[var(--ks-text-muted)] truncate">{{ g.label }}</h3>
                <ul>
                  @for (q of g.items; track q.id) {
                    <li>
                      <button
                        type="button"
                        class="ks-focusable w-full text-left px-3 py-2 flex flex-col gap-0.5 border-l-2 transition-colors duration-100 hover:bg-[var(--ks-raised)]"
                        [class.border-[var(--ks-amber)]]="isPending(q) && store.selectedId() === q.id"
                        [class.border-transparent]="store.selectedId() !== q.id"
                        [class.bg-[var(--ks-raised)]]="store.selectedId() === q.id"
                        [class.opacity-60]="!isPending(q) || snoozed(q)"
                        [attr.aria-current]="store.selectedId() === q.id ? 'true' : null"
                        [attr.data-queue-item]="q.sid"
                        (click)="openQuestion(q)"
                      >
                        <span class="flex items-center gap-1.5 text-[11px] text-[var(--ks-text-muted)]">
                          <app-icon [name]="q.kind === 'permission' ? 'hand' : q.orchestrator ? 'orchestrator' : q.agent || 'question'" [size]="11" />
                          <span class="truncate">{{ q.chat_title || q.agent }}</span>
                          @if (q.machine_name) {
                            <span>· {{ q.machine_name }}</span>
                          }
                          <span class="flex-1"></span>
                          @if (isPending(q)) {
                            <span class="text-[var(--ks-amber)] shrink-0">{{ since(q) }}</span>
                          } @else {
                            <app-icon name="check" [size]="11" />
                          }
                        </span>
                        <span class="text-[12.5px] leading-snug text-[var(--ks-text)] line-clamp-2">{{ q.prompt }}</span>
                        @if (q.status === 'delegated') {
                          <span class="text-[10.5px] text-[var(--ks-violet)]">Orchestrator deciding</span>
                        } @else if (q.thread && !q.thread.discarded) {
                          <span class="text-[10.5px] text-[var(--ks-blue)]">Side conversation · {{ q.messages.length }} message{{ q.messages.length === 1 ? '' : 's' }}</span>
                        }
                      </button>
                    </li>
                  }
                </ul>
              </section>
            }
          </nav>

          <!-- Selected question -->
          <main
            class="flex-1 min-w-0 min-h-0 overflow-y-auto p-3 md:block md:p-5"
            [class.hidden]="mobileView() !== 'question'"
            aria-label="Question"
            data-testid="questions-detail"
          >
            @if (store.selected(); as q) {
              <ks-question-card [question]="q" />
              @if (!store.threadOpen() && q.messages.length) {
                <button type="button" class="ks-btn ks-btn-sm mt-2" data-testid="thread-reopen" (click)="store.threadOpen.set(true)">
                  Show side conversation ({{ q.messages.length }})
                </button>
              }
            } @else if (store.groups().length) {
              <p class="text-[12.5px] text-[var(--ks-text-muted)]">Pick a question.</p>
            }
          </main>

          <!-- Side conversation: a column on desktop, a bottom sheet on a phone -->
          @if (store.threadOpen() && store.selected(); as q) {
            <div class="fixed inset-0 z-50 flex flex-col justify-end md:static md:z-auto md:w-[400px] md:shrink-0 md:border-l md:border-[var(--ks-hairline)]">
              <div class="ks-backdrop flex-1 md:hidden" (click)="store.threadOpen.set(false)"></div>
              <aside
                class="ks-drawer-bottom md:animate-none h-[80vh] md:h-full flex flex-col bg-[var(--ks-surface-1)] rounded-t-[var(--ks-radius-xl)] md:rounded-none shadow-2xl md:shadow-none"
                aria-label="Side conversation"
                data-testid="side-conversation"
              >
                <ks-side-conversation [question]="q" (closed)="store.threadOpen.set(false)" />
              </aside>
            </div>
          }
        </div>
      </div>
    </div>
  `,
})
export class QuestionsPanelComponent implements OnInit, OnDestroy {
  readonly store = inject(QuestionsStore);
  private readonly doc = inject(DOCUMENT);
  /** Phone layout: the inbox or the selected question (desktop shows both). */
  readonly mobileView = signal<'inbox' | 'question'>('inbox');
  protected readonly isPending = isPending;
  readonly count = computed(() => this.store.pendingCount());

  private readonly onKey = (ev: KeyboardEvent) => this.handleKey(ev);

  ngOnInit(): void {
    this.doc.addEventListener('keydown', this.onKey);
    // Opened on a specific question (from a chat card or a notification): show it.
    if (this.store.selectedId() && this.isPhone()) this.mobileView.set('question');
  }

  ngOnDestroy(): void {
    this.doc.removeEventListener('keydown', this.onKey);
  }

  private isPhone(): boolean {
    return typeof window !== 'undefined' && window.matchMedia?.('(max-width: 767px)').matches === true;
  }

  since(q: QuestionView): string {
    return formatSince(q.blocked_since, this.store.now());
  }

  snoozed(q: QuestionView): boolean {
    return isSnoozed(q, this.store.now());
  }

  openQuestion(q: QuestionView): void {
    this.store.select(q.id);
    this.mobileView.set('question');
  }

  handleKey(ev: KeyboardEvent): void {
    if (ev.key === 'Escape') {
      if (fromTextField(ev.target) && (ev.target as HTMLInputElement).value) return;
      ev.preventDefault();
      if (this.store.threadOpen()) this.store.threadOpen.set(false);
      else if (this.isPhone() && this.mobileView() === 'question') this.mobileView.set('inbox');
      else this.store.close();
      return;
    }
    if (fromTextField(ev.target) || ev.defaultPrevented) return;
    const k = queueKey(ev);
    if (!k) return;
    const q = this.store.selected();
    switch (k.kind) {
      case 'move':
        ev.preventDefault();
        this.store.move(k.delta);
        return;
      case 'pick':
        if (q && this.store.canAnswer(q) && q.options?.[k.index]) {
          ev.preventDefault();
          void this.store.pick(q, k.index);
        }
        return;
      case 'ask':
        if (q && isPending(q)) {
          ev.preventDefault();
          this.store.threadOpen.set(true);
        }
        return;
      case 'delegate':
        if (q && q.status === 'open' && !q.orchestrator) {
          ev.preventDefault();
          void this.store.delegate(q);
        }
        return;
    }
  }
}

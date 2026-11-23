import { DOCUMENT } from '@angular/common';
import { ChangeDetectionStrategy, Component, DestroyRef, computed, effect, inject, signal, untracked } from '@angular/core';
import { Router } from '@angular/router';
import { isPending } from '../../core/models/question.models';
import { ControlStore } from '../../core/state/control.store';
import { NeedsYouEntry, NeedsYouStore, vetoDetails } from '../../core/state/needs-you.store';
import { QuestionsStore } from '../../core/state/questions.store';
import { fromTextField, queueKey } from '../../core/state/questions-logic';
import { WorkStore } from '../../core/state/work.store';
import { ReviewPanelComponent } from '../../modes/work/review-panel.component';
import { IconComponent } from '../../shared/icon.component';
import { ModalDialogComponent } from '../../shared/modal-dialog.component';
import { QuestionCardComponent } from '../questions/question-card.component';
import { SideConversationComponent } from '../questions/side-conversation.component';

/**
 * The one Needs-you dialog every page opens: a carousel over the whole queue that starts on
 * the item you picked, shows where you are ("2 of 5" and a strip of dots), and moves on to
 * the next unanswered item once you answer (or snooze, or let the orchestrator decide).
 * Questions get the full card with Ask about this inside it; requests, vetoes and reviews
 * their own body in the same shell. j/k or arrows (swipe on a phone) move, 1–9 pick an
 * option, ? asks, o hands it to the orchestrator; Esc or × close and focus goes back to
 * where you came from. On a phone it is a full-screen sheet.
 */
@Component({
  selector: 'ks-needs-you-dialog',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [ModalDialogComponent, IconComponent, QuestionCardComponent, SideConversationComponent, ReviewPanelComponent],
  template: `
    <app-modal-dialog
      labelledBy="ks-needs-you-heading"
      [panelClass]="panelClass()"
      (close)="onEscape()"
    >
      <div class="flex flex-col flex-1 min-h-0" data-testid="questions-panel">
        <header class="flex items-center gap-2 h-12 px-3 shrink-0 border-b border-[var(--ks-hairline)]" data-testid="needs-you-dialog">
          <app-icon name="bell" [size]="15" />
          <h2 id="ks-needs-you-heading" class="m-0 text-[13px] font-semibold text-[var(--ks-text)]">Needs you</h2>
          @if (pos().total > 0 && !store.caughtUp()) {
            <span class="font-mono-ide text-[11.5px] text-[var(--ks-text-muted)]" aria-live="polite" data-testid="needs-you-position">{{ pos().index + 1 }} of {{ pos().total }}</span>
          }
          <div class="ks-ny-dots flex-1 min-w-0 flex items-center gap-1 overflow-x-auto px-2 py-2" role="group" aria-label="Queue">
            @for (e of store.queue(); track e.key; let i = $index) {
              <button
                type="button"
                class="ks-ny-dot"
                [attr.data-tag]="e.tag"
                [class.ks-ny-dot-current]="e.key === store.currentKey()"
                [class.ks-ny-dot-done]="!e.pending"
                [attr.aria-current]="e.key === store.currentKey() ? 'step' : null"
                [attr.aria-label]="(i + 1) + ': ' + e.title"
                [attr.data-queue-item]="e.sid"
                (click)="go(e)"
              ></button>
            }
          </div>
          <span class="hidden lg:inline text-[10.5px] text-[var(--ks-text-muted)]">
            <span class="ks-kbd">j</span>/<span class="ks-kbd">k</span> move · <span class="ks-kbd">1</span>–<span class="ks-kbd">9</span> answer · <span class="ks-kbd">?</span> ask
          </span>
          <button type="button" class="ks-icon-btn" aria-label="Previous" data-testid="needs-you-prev" [disabled]="pos().index <= 0" (click)="move(-1)">
            <span class="inline-flex rotate-180"><app-icon name="chevron-right" [size]="14" /></span>
          </button>
          <button type="button" class="ks-icon-btn" aria-label="Next" data-testid="needs-you-next" [disabled]="pos().index >= pos().total - 1" (click)="move(1)">
            <app-icon name="chevron-right" [size]="14" />
          </button>
          <button type="button" class="ks-icon-btn" aria-label="Close" data-testid="questions-close" (click)="store.close()">
            <app-icon name="close" [size]="15" />
          </button>
        </header>

        @if (questions.error() || control.error()) {
          <div role="alert" class="px-4 py-2 text-[12px] text-[#f3a5a5] bg-[var(--ks-red-soft)] shrink-0" data-testid="questions-error">{{ questions.error() || control.error() }}</div>
        }

        <div class="flex flex-1 min-h-0" (touchstart)="onTouchStart($event)" (touchend)="onTouchEnd($event)">
          <main class="flex-1 min-w-0 min-h-0 overflow-y-auto" aria-live="polite" data-testid="questions-detail">
            @if (store.caughtUp()) {
              <div class="ks-ny-slide h-full min-h-[220px] flex flex-col items-center justify-center gap-2 text-center p-6" role="status" data-testid="needs-you-caught-up">
                <span class="text-[var(--ks-green)]"><app-icon name="check-circle" [size]="28" /></span>
                <p class="m-0 text-[15px] font-semibold text-[var(--ks-text)]">All caught up</p>
                <p class="m-0 text-[12.5px] text-[var(--ks-text-muted)]">Nothing else needs you right now.</p>
              </div>
            } @else {
              @for (e of currentList(); track e.key) {
                <div class="ks-ny-slide p-3 md:p-5" [class.ks-ny-from-left]="dir() < 0" [attr.data-entry]="e.key">
                  @switch (e.kind) {
                    @case ('question') {
                      <ks-question-card [question]="e.question!" />
                      @if (!questions.threadOpen() && e.question!.messages.length) {
                        <button type="button" class="ks-btn ks-btn-sm mt-2" data-testid="thread-reopen" (click)="questions.threadOpen.set(true)">
                          Show side conversation ({{ e.question!.messages.length }})
                        </button>
                      }
                    }
                    @case ('request') {
                      <article class="rounded-[var(--ks-radius-lg)] bg-[var(--ks-surface-2)] p-4 space-y-3" data-testid="needs-you-request">
                        <div class="text-[10.5px] font-semibold uppercase tracking-wide text-[#e9bd76]">An agent asks you to do something</div>
                        <h3 class="m-0 text-[17px] font-semibold leading-snug text-[var(--ks-text)]">{{ e.request!.description }}</h3>
                        <p class="m-0 text-[12.5px] text-[var(--ks-text-secondary)]">{{ e.meta }}</p>
                        <p class="m-0 text-[12.5px] text-[var(--ks-text-muted)]">Mark it done once you've done it and the agent carries on; decline and it is told you won't.</p>
                        @if (e.pending) {
                          <div class="flex flex-wrap gap-2">
                            <button type="button" class="ks-btn ks-btn-primary h-10" [attr.data-testid]="'request-done-' + e.sid" [disabled]="control.isBusy(e.request!.id)" (click)="resolve(e, 'done')">
                              <app-icon name="check" [size]="12" />
                              Mark done
                            </button>
                            <button type="button" class="ks-btn ks-btn-soft h-10" [attr.data-testid]="'request-decline-' + e.sid" [disabled]="control.isBusy(e.request!.id)" (click)="resolve(e, 'declined')">Decline</button>
                          </div>
                        } @else {
                          <p class="m-0 text-[12.5px] text-[#8fcb9b]" role="status">Done: the agent was told.</p>
                        }
                      </article>
                    }
                    @case ('veto') {
                      <article class="rounded-[var(--ks-radius-lg)] bg-[var(--ks-red-soft)] p-4 space-y-3" data-testid="needs-you-veto">
                        <div class="text-[10.5px] font-semibold uppercase tracking-wide text-[#f3a3a3]">Veto · {{ veto(e).reason }}</div>
                        <h3 class="m-0 text-[17px] font-semibold leading-snug text-[var(--ks-text)]">{{ e.title }}</h3>
                        @if (veto(e).note) {
                          <p class="m-0 text-[13px] text-[var(--ks-text-secondary)]">“{{ veto(e).note }}”</p>
                        }
                        <ol class="m-0 p-0 list-none space-y-1 text-[12.5px]">
                          <li><span class="text-[#8fcb9b]" aria-hidden="true">✓</span> Sent and enforced: its processes were stopped and it is on hold</li>
                          <li><span class="text-[#8fcb9b]" aria-hidden="true">✓</span> The orchestrator was told and remembers not to retry it this way</li>
                        </ol>
                        @if (e.pending) {
                          <div class="flex flex-wrap gap-2">
                            <button type="button" class="ks-btn ks-btn-primary h-10" data-testid="veto-ack" (click)="ack(e)">Got it</button>
                            @if (e.sessionId && work.byId()[e.sessionId]) {
                              <button type="button" class="ks-btn ks-btn-soft h-10" (click)="openSession(e)">Open the session</button>
                            }
                          </div>
                        } @else {
                          <p class="m-0 text-[12.5px] text-[var(--ks-text-muted)]" role="status">Acknowledged.</p>
                        }
                      </article>
                    }
                    @case ('review') {
                      <div class="rounded-[var(--ks-radius-lg)] bg-[var(--ks-surface-1)] min-h-[320px] flex flex-col" data-testid="needs-you-review">
                        <ks-review-panel class="flex-1" [item]="reviewItem(e)" [embedded]="true" (decided)="store.advance(e.key)" />
                      </div>
                    }
                  }
                </div>
              } @empty {
                <p class="p-5 text-[12.5px] text-[var(--ks-text-muted)]" data-testid="questions-empty">
                  Nothing needs you. When an agent anywhere in the fleet needs a decision, it shows up here.
                </p>
              }
            }
          </main>

          @if (questions.threadOpen() && store.current()?.question; as q) {
            <div class="fixed inset-0 z-50 flex flex-col justify-end md:static md:z-auto md:w-[400px] md:shrink-0 md:border-l md:border-[var(--ks-hairline)]">
              <div class="ks-backdrop flex-1 md:hidden" (click)="questions.threadOpen.set(false)"></div>
              <aside
                class="ks-drawer-bottom md:animate-none h-[80vh] md:h-full flex flex-col bg-[var(--ks-surface-1)] rounded-t-[var(--ks-radius-xl)] md:rounded-none shadow-2xl md:shadow-none"
                aria-label="Side conversation"
                data-testid="side-conversation"
              >
                <ks-side-conversation [question]="q" (closed)="questions.threadOpen.set(false)" />
              </aside>
            </div>
          }
        </div>
      </div>
    </app-modal-dialog>
  `,
  styles: `
    .ks-ny-dots {
      scrollbar-width: none;
      justify-content: safe center;
    }
    .ks-ny-dot {
      flex-shrink: 0;
      width: 8px;
      height: 8px;
      padding: 0;
      border: 0;
      border-radius: 9999px;
      background: rgba(224, 168, 76, 0.45);
      cursor: pointer;
      position: relative;
    }
    .ks-ny-dot::after {
      /* A taller hit area that never overlaps the neighbouring dots. */
      content: '';
      position: absolute;
      inset: -10px -2px;
    }
    .ks-ny-dot[data-tag='P1'] {
      background: rgba(232, 107, 107, 0.6);
    }
    .ks-ny-dot[data-tag='P3'] {
      background: rgba(255, 255, 255, 0.25);
    }
    .ks-ny-dot-done {
      background: rgba(105, 184, 121, 0.55) !important;
    }
    .ks-ny-dot-current {
      width: 20px;
      background: var(--ks-text) !important;
    }
    .ks-ny-dot:focus-visible {
      outline: none;
      box-shadow: var(--ks-focus-ring);
    }
    .ks-ny-slide {
      animation: ks-ny-in 220ms var(--ks-ease-out) both;
    }
    .ks-ny-slide.ks-ny-from-left {
      animation-name: ks-ny-in-left;
    }
    @keyframes ks-ny-in {
      from {
        opacity: 0;
        transform: translateX(28px);
      }
      to {
        opacity: 1;
        transform: none;
      }
    }
    @keyframes ks-ny-in-left {
      from {
        opacity: 0;
        transform: translateX(-28px);
      }
      to {
        opacity: 1;
        transform: none;
      }
    }
    @keyframes ks-ny-fade {
      from {
        opacity: 0;
      }
      to {
        opacity: 1;
      }
    }
    @media (prefers-reduced-motion: reduce) {
      .ks-ny-slide,
      .ks-ny-slide.ks-ny-from-left {
        animation: ks-ny-fade 160ms linear both;
      }
    }
  `,
})
export class NeedsYouDialogComponent {
  readonly store = inject(NeedsYouStore);
  readonly questions = inject(QuestionsStore);
  readonly control = inject(ControlStore);
  readonly work = inject(WorkStore);
  private readonly router = inject(Router);

  /** Direction of the last move, for the slide (1: forward, -1: back). */
  readonly dir = signal(1);
  readonly pos = this.store.position;
  /**
   * Sized to its content up to a max (short questions don't leave a tall empty panel); a
   * review or an open side conversation gets the full height since they scroll inside.
   */
  readonly panelClass = computed(() => {
    const tall = this.questions.threadOpen() || this.store.current()?.kind === 'review';
    return (
      'ks-ny-panel max-md:!fixed max-md:!inset-0 shrink-0 flex flex-col bg-[var(--ks-bg-chrome)] overflow-hidden rounded-none md:w-[min(1120px,95vw)] md:max-h-[min(820px,92vh)] md:rounded-[var(--ks-radius-xl)]' +
      (tall ? ' md:h-[min(820px,92vh)]' : '')
    );
  });
  readonly currentList = computed<NeedsYouEntry[]>(() => {
    const c = this.store.current();
    return c ? [c] : [];
  });

  private touch: { x: number; y: number } | null = null;

  constructor() {
    // Answering, snoozing or handing a question to the orchestrator moves on.
    effect(() => {
      const a = this.questions.acted();
      if (!a) return;
      untracked(() => {
        if (this.store.currentKey() === `q:${a.id}`) this.forward(() => this.store.advance(`q:${a.id}`));
      });
    });
    const doc = inject(DOCUMENT);
    const onKey = (ev: KeyboardEvent) => this.handleKey(ev);
    doc.addEventListener('keydown', onKey);
    const timer = setInterval(() => this.store.now.set(Date.now()), 15_000);
    inject(DestroyRef).onDestroy(() => {
      doc.removeEventListener('keydown', onKey);
      clearInterval(timer);
    });
  }

  private forward(fn: () => void): void {
    this.dir.set(1);
    fn();
  }

  move(delta: number): void {
    this.dir.set(delta < 0 ? -1 : 1);
    this.store.move(delta);
  }

  go(e: NeedsYouEntry): void {
    const target = this.store.queue().findIndex((x) => x.key === e.key);
    this.dir.set(target < this.pos().index ? -1 : 1);
    this.store.go(e.key);
  }

  veto(e: NeedsYouEntry): { reason: string; note: string } {
    return e.veto ? vetoDetails(e.veto) : { reason: '', note: '' };
  }

  reviewItem(e: NeedsYouEntry) {
    return (e.sessionId && this.work.byId()[e.sessionId]) || e.review || null;
  }

  async resolve(e: NeedsYouEntry, status: 'done' | 'declined'): Promise<void> {
    if (e.request && (await this.control.resolveRequest(e.request, status))) this.forward(() => this.store.advance(e.key));
  }

  ack(e: NeedsYouEntry): void {
    if (!e.veto) return;
    this.store.dismissVeto(e.veto);
    this.forward(() => this.store.advance(e.key, 400));
  }

  openSession(e: NeedsYouEntry): void {
    const it = e.sessionId ? this.work.byId()[e.sessionId] : undefined;
    if (!it) return;
    this.store.close();
    void this.router.navigate(['/command', it.sid]);
  }

  /** Esc: close the side conversation first, then the dialog. */
  onEscape(): void {
    if (this.questions.threadOpen()) this.questions.threadOpen.set(false);
    else this.store.close();
  }

  onTouchStart(ev: TouchEvent): void {
    const t = ev.touches[0];
    this.touch = t ? { x: t.clientX, y: t.clientY } : null;
  }

  onTouchEnd(ev: TouchEvent): void {
    const start = this.touch;
    this.touch = null;
    const t = ev.changedTouches[0];
    if (!start || !t || fromTextField(ev.target)) return;
    const dx = t.clientX - start.x;
    const dy = t.clientY - start.y;
    if (Math.abs(dx) > 60 && Math.abs(dy) < 40) this.move(dx < 0 ? 1 : -1);
  }

  handleKey(ev: KeyboardEvent): void {
    if (ev.key === 'Escape') {
      // A field with text keeps its Esc; otherwise the side conversation closes first, then
      // the dialog (handled here so the native dialog doesn't also close).
      if (fromTextField(ev.target) && (ev.target as HTMLInputElement).value) return;
      ev.preventDefault();
      this.onEscape();
      return;
    }
    if (fromTextField(ev.target) || ev.defaultPrevented) return;
    if (!ev.metaKey && !ev.ctrlKey && !ev.altKey && (ev.key === 'ArrowRight' || ev.key === 'ArrowLeft')) {
      ev.preventDefault();
      this.move(ev.key === 'ArrowRight' ? 1 : -1);
      return;
    }
    const k = queueKey(ev);
    if (!k) return;
    const cur = this.store.current();
    const q = cur?.question;
    switch (k.kind) {
      case 'move':
        ev.preventDefault();
        this.move(k.delta);
        return;
      case 'pick':
        if (q && this.questions.canAnswer(q) && q.options?.[k.index]) {
          ev.preventDefault();
          void this.questions.pick(q, k.index);
        }
        return;
      case 'ask':
        if (q && isPending(q)) {
          ev.preventDefault();
          this.questions.threadOpen.set(true);
        }
        return;
      case 'delegate':
        if (q && q.status === 'open') {
          ev.preventDefault();
          void this.questions.delegate(q);
        }
        return;
    }
  }
}

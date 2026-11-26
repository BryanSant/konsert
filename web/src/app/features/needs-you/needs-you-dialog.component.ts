import { DOCUMENT } from '@angular/common';
import { ChangeDetectionStrategy, Component, DestroyRef, computed, effect, inject, signal, untracked } from '@angular/core';
import { Router } from '@angular/router';
import { UserRequest, Veto, formatDuration, resourceLabel } from '../../core/models/control.models';
import { isPending } from '../../core/models/question.models';
import { ControlStore } from '../../core/state/control.store';
import { FleetStore } from '../../core/state/fleet.store';
import { NeedsYouEntry, NeedsYouStore, overrideScope, vetoDetails, vetoEscalated } from '../../core/state/needs-you.store';
import { QuestionsStore } from '../../core/state/questions.store';
import { formatSince, fromTextField, queueKey } from '../../core/state/questions-logic';
import { WorkStore } from '../../core/state/work.store';
import { ReviewPanelComponent } from '../../modes/work/review-panel.component';
import { IconComponent } from '../../shared/icon.component';
import { ModalDialogComponent } from '../../shared/modal-dialog.component';
import { QuestionCardComponent } from '../questions/question-card.component';
import { SideConversationComponent } from '../questions/side-conversation.component';
import { TooltipDirective } from '../../shared/tooltip.directive';

/**
 * The one Needs-you dialog every page opens: a carousel over the whole queue that starts on
 * the item you picked, shows where you are ("2 of 5" and a strip of dots), and moves on to
 * the next unanswered item once you answer (or snooze, or let the commander decide).
 * Questions get the full card with Ask about this inside it; requests, override requests,
 * vetoes and reviews their own body in the same shell. j/k or arrows (swipe on a phone) move, 1–9 pick an
 * option, ? asks, o hands it to the commander; Esc or × close and focus goes back to
 * where you came from. On a phone it is a full-screen sheet.
 */
@Component({
  selector: 'sy-needs-you-dialog',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [ModalDialogComponent, IconComponent, QuestionCardComponent, SideConversationComponent, ReviewPanelComponent, TooltipDirective],
  template: `
    <app-modal-dialog
      labelledBy="sy-needs-you-heading"
      [panelClass]="panelClass()"
      (close)="onEscape()"
    >
      <div class="flex flex-col flex-1 min-h-0" data-testid="questions-panel">
        <header class="flex items-center gap-2 h-12 px-3 shrink-0 border-b border-[var(--sy-hairline)]" data-testid="needs-you-dialog">
          <app-icon name="bell" [size]="15" />
          <h2 id="sy-needs-you-heading" class="m-0 text-[13px] font-semibold text-[var(--sy-text)]">Needs you</h2>
          @if (pos().total > 0 && !store.caughtUp()) {
            <span class="font-mono-ide text-[11.5px] text-[var(--sy-text-muted)]" aria-live="polite" data-testid="needs-you-position">{{ pos().index + 1 }} of {{ pos().total }}</span>
          }
          <div class="sy-ny-dots flex-1 min-w-0 flex items-center gap-1 overflow-x-auto px-2 py-2" role="group" aria-label="Queue">
            @for (e of store.queue(); track e.key; let i = $index) {
              <button
                type="button"
                class="sy-ny-dot"
                [attr.data-tag]="e.tag"
                [class.sy-ny-dot-current]="e.key === store.currentKey()"
                [class.sy-ny-dot-done]="!e.pending"
                [attr.aria-current]="e.key === store.currentKey() ? 'step' : null"
                [attr.aria-label]="(i + 1) + ': ' + e.title"
                [attr.data-queue-item]="e.sid"
                (click)="go(e)"
              ></button>
            }
          </div>
          <span class="hidden lg:inline text-[10.5px] text-[var(--sy-text-muted)]">
            <span class="sy-kbd">j</span>/<span class="sy-kbd">k</span> move · <span class="sy-kbd">1</span>–<span class="sy-kbd">9</span> answer · <span class="sy-kbd">?</span> ask
          </span>
          <button type="button" class="sy-icon-btn" aria-label="Previous" data-testid="needs-you-prev" [disabled]="pos().index <= 0" (click)="move(-1)">
            <span class="inline-flex rotate-180"><app-icon name="chevron-right" [size]="14" /></span>
          </button>
          <button type="button" class="sy-icon-btn" aria-label="Next" data-testid="needs-you-next" [disabled]="pos().index >= pos().total - 1" (click)="move(1)">
            <app-icon name="chevron-right" [size]="14" />
          </button>
          <button type="button" class="sy-icon-btn" aria-label="Close" data-testid="questions-close" (click)="store.close()">
            <app-icon name="close" [size]="15" />
          </button>
        </header>

        @if (questions.error() || control.error()) {
          <div role="alert" class="px-4 py-2 text-[12px] text-[#f3a5a5] bg-[var(--sy-red-soft)] shrink-0" data-testid="questions-error">{{ questions.error() || control.error() }}</div>
        }

        <div class="flex flex-1 min-h-0" (touchstart)="onTouchStart($event)" (touchend)="onTouchEnd($event)">
          <main class="flex-1 min-w-0 min-h-0 overflow-y-auto" aria-live="polite" data-testid="questions-detail">
            @if (store.caughtUp()) {
              <div class="sy-ny-slide h-full min-h-[220px] flex flex-col items-center justify-center gap-2 text-center p-6" role="status" data-testid="needs-you-caught-up">
                <span class="text-[var(--sy-green)]"><app-icon name="check-circle" [size]="28" /></span>
                <p class="m-0 text-[15px] font-semibold text-[var(--sy-text)]">All caught up</p>
                <p class="m-0 text-[12.5px] text-[var(--sy-text-muted)]">Nothing else needs you right now.</p>
              </div>
            } @else {
              @for (e of currentList(); track e.key) {
                <div class="sy-ny-slide p-3 md:p-5" [class.sy-ny-from-left]="dir() < 0" [attr.data-entry]="e.key">
                  @switch (e.kind) {
                    @case ('question') {
                      <sy-question-card [question]="e.question!" />
                      @if (!questions.threadOpen() && e.question!.messages.length) {
                        <button type="button" class="sy-btn sy-btn-sm mt-2" data-testid="thread-reopen" (click)="questions.threadOpen.set(true)">
                          Show side conversation ({{ e.question!.messages.length }})
                        </button>
                      }
                    }
                    @case ('request') {
                      @let r = e.request!;
                      <article
                        class="rounded-[var(--sy-radius-lg)] bg-[var(--sy-surface-2)] p-4 space-y-3"
                        [class.ring-1]="e.pending"
                        [class.ring-[var(--sy-amber-soft)]]="e.pending"
                        data-testid="needs-you-request"
                        [attr.data-request]="e.sid"
                        [attr.data-impact]="e.impact ?? null"
                      >
                        <header class="flex flex-wrap items-center gap-x-2 gap-y-1 text-[11.5px] text-[var(--sy-text-muted)]">
                          <span class="inline-flex items-center gap-1.5 text-[var(--sy-text-secondary)]">
                            <app-icon [name]="r.commander ? 'commander' : 'hand'" [size]="13" />
                            <span class="font-medium">{{ r.commander ? 'Commander' : 'Agent' }}</span>
                          </span>
                          @if (r.chat_title) {
                            <span>·</span><span class="truncate max-w-[220px]" [syTooltip]="r.chat_sid || ''">{{ r.chat_title }}</span>
                          }
                          @if (requestMachine(r); as machine) {
                            <span>·</span><span>{{ machine }}</span>
                          }
                          <span class="flex-1"></span>
                          @if (e.pending) {
                            <span class="inline-flex items-center gap-1 text-[var(--sy-amber)]" data-testid="request-blocked">
                              <app-icon name="clock" [size]="11" />
                              blocked {{ since(r.created_at) }}
                            </span>
                          }
                        </header>
                        <div class="space-y-1">
                          <div class="flex items-center gap-1.5 text-[10.5px] font-semibold uppercase tracking-wide text-[#e9bd76]">
                            <app-icon name="hand" [size]="12" />
                            Action requested
                            <span class="font-mono-ide normal-case tracking-normal text-[var(--sy-text-muted)]">{{ e.sid }}</span>
                          </div>
                          <h3 class="m-0 text-[17px] font-semibold leading-snug text-[var(--sy-text)]">{{ r.description }}</h3>
                          <p class="m-0 text-[12px] text-[var(--sy-text-secondary)]" data-testid="request-impact">{{ requestImpactLine(r) }}</p>
                        </div>
                        <p class="m-0 text-[12.5px] text-[var(--sy-text-muted)]">Mark it done once you've done it and the agent carries on; decline and it is told you won't.</p>
                        @if (e.pending) {
                          <div class="flex flex-wrap gap-2">
                            <button type="button" class="sy-btn sy-btn-primary h-10" [attr.data-testid]="'request-done-' + e.sid" [disabled]="control.isBusy(r.id)" (click)="resolve(e, 'done')">
                              <app-icon name="check" [size]="12" />
                              Mark done
                            </button>
                            <button type="button" class="sy-btn sy-btn-soft h-10" [attr.data-testid]="'request-decline-' + e.sid" [disabled]="control.isBusy(r.id)" (click)="resolve(e, 'declined')">Decline</button>
                          </div>
                        } @else {
                          <p class="m-0 text-[12.5px] text-[#8fcb9b]" role="status" data-testid="request-resolved">{{ resolved()[e.key] === 'declined' ? 'Declined' : 'Done' }}: the agent was told.</p>
                        }
                      </article>
                    }
                    @case ('policy') {
                      @let p = e.policy!;
                      <article class="rounded-[var(--sy-radius-lg)] bg-[var(--sy-surface-2)] p-4 space-y-3" data-testid="needs-you-policy" [attr.data-policy]="e.sid">
                        <div class="text-[10.5px] font-semibold uppercase tracking-wide text-[#e9bd76]">The commander wants to change policy · {{ e.sid }}</div>
                        <h3 class="m-0 text-[17px] font-semibold leading-snug text-[var(--sy-text)]">{{ p.summary || e.title }}</h3>
                        @if (p.reason) {
                          <p class="m-0 text-[12.5px] text-[var(--sy-text-secondary)]" data-testid="policy-reason">{{ p.reason }}</p>
                        }
                        <p class="m-0 text-[12px] text-[var(--sy-text-muted)]">Approving applies it, attributed to you via Commander. You can undo it from Your actions.</p>
                        @if (e.pending) {
                          <div class="flex flex-wrap gap-2">
                            <button type="button" class="sy-btn sy-btn-primary h-10" data-testid="policy-approve" [disabled]="control.isBusy(p.id)" (click)="decidePolicy(e, 'approve')">
                              <app-icon name="check" [size]="12" />
                              Approve
                            </button>
                            <button type="button" class="sy-btn sy-btn-soft h-10" data-testid="policy-deny" [disabled]="control.isBusy(p.id)" (click)="decidePolicy(e, 'deny')">Deny</button>
                          </div>
                        } @else {
                          <p class="m-0 text-[12.5px] text-[#8fcb9b]" role="status" data-testid="policy-decided">{{ decided()[e.key] === 'deny' ? 'Denied' : 'Approved' }}: the change {{ decided()[e.key] === 'deny' ? 'was not applied' : 'is in effect' }}.</p>
                        }
                      </article>
                    }
                    @case ('override') {
                      @let o = e.override!;
                      <article class="rounded-[var(--sy-radius-lg)] bg-[var(--sy-surface-2)] p-4 space-y-3" data-testid="needs-you-override" [attr.data-override]="e.sid">
                        <div class="text-[10.5px] font-semibold uppercase tracking-wide text-[#e9bd76]">The commander asks for an override · {{ e.sid }}</div>
                        <h3 class="m-0 text-[17px] font-semibold leading-snug text-[var(--sy-text)]">{{ e.title }}</h3>
                        <dl class="sy-ny-facts m-0 text-[12.5px]">
                          <dt>Scope</dt>
                          <dd data-testid="override-scope">{{ scope(e) }}</dd>
                          <dt>Duration</dt>
                          <dd data-testid="override-duration">{{ duration(o.duration_seconds) }} from when you approve, then it expires by itself</dd>
                          <dt>Reason</dt>
                          <dd data-testid="override-reason">{{ o.reason }}</dd>
                          <dt>If you approve</dt>
                          <dd data-testid="override-consequences">{{ o.consequences }}</dd>
                        </dl>
                        @if (e.pending) {
                          <input
                            type="text"
                            class="sy-field w-full text-[12.5px]"
                            placeholder="Note for the commander (optional)"
                            aria-label="Note for the commander (optional)"
                            data-testid="override-note"
                            [value]="noteFor(e.key)"
                            (input)="setNote(e.key, $any($event.target).value)"
                          />
                          <div class="flex flex-wrap gap-2">
                            <button type="button" class="sy-btn sy-btn-primary h-10" data-testid="override-approve" [disabled]="control.isBusy(o.id)" (click)="decide(e, 'approve')">
                              <app-icon name="check" [size]="12" />
                              Approve for {{ duration(o.duration_seconds) }}
                            </button>
                            <button type="button" class="sy-btn sy-btn-soft h-10" data-testid="override-deny" [disabled]="control.isBusy(o.id)" (click)="decide(e, 'deny')">Deny</button>
                          </div>
                          <p class="m-0 text-[12px] text-[var(--sy-text-muted)]">Syre enforces an approved override until it expires, counts whether it was used, and tells the commander your decision either way.</p>
                        } @else {
                          <p class="m-0 text-[12.5px] text-[#8fcb9b]" role="status" data-testid="override-decided">{{ decided()[e.key] === 'deny' ? 'Denied' : 'Approved' }}: the commander was told.</p>
                        }
                      </article>
                    }
                    @case ('veto') {
                      @let v = e.veto!;
                      @let late = escalated(v);
                      <article class="rounded-[var(--sy-radius-lg)] bg-[var(--sy-red-soft)] p-4 space-y-3" data-testid="needs-you-veto" [attr.data-veto]="v.sid" [attr.data-veto-status]="late ? 'escalated' : v.status">
                        <div class="flex items-center gap-2">
                          <span class="text-[10.5px] font-semibold uppercase tracking-wide text-[#f3a3a3]">Veto · {{ veto(e).reason }}</span>
                          @if (late && e.pending) {
                            <span class="sy-badge sy-ny-late" data-testid="veto-escalated">Not acknowledged</span>
                          }
                        </div>
                        <h3 class="m-0 text-[17px] font-semibold leading-snug text-[var(--sy-text)]">{{ e.title }}</h3>
                        @if (veto(e).note) {
                          <p class="m-0 text-[13px] text-[var(--sy-text-secondary)]">“{{ veto(e).note }}”</p>
                        }
                        <ol class="m-0 p-0 list-none space-y-1 text-[12.5px]" data-testid="veto-steps">
                          <li><span class="text-[#8fcb9b]" aria-hidden="true">✓</span> Sent and enforced: its processes were stopped and it is on hold</li>
                          @if (v.delivered_at) {
                            <li><span class="text-[#8fcb9b]" aria-hidden="true">✓</span> Told to {{ v.delivered_to || 'the agent' }} at {{ clock(v.delivered_at) }}</li>
                          } @else {
                            <li><span class="text-[var(--sy-text-muted)]" aria-hidden="true">○</span> Waiting to tell the commander (or the vetoed chat) on its next turn</li>
                          }
                          @if (!e.pending) {
                            <li data-testid="veto-resolved"><span class="text-[#8fcb9b]" aria-hidden="true">✓</span> {{ dismissedHere()[e.key] ? 'Dismissed by you' : 'Acknowledged by the agent on its next turn' }}</li>
                          } @else if (late) {
                            <li><span class="text-[#f3a3a3]" aria-hidden="true">!</span> Not acknowledged by {{ clock(v.ack_deadline) }}: the agent has not finished a turn with the veto yet. Check the session, or dismiss this once you are sure it got the message.</li>
                          } @else {
                            <li data-testid="veto-waiting"><span class="text-[var(--sy-text-muted)]" aria-hidden="true">○</span> Waiting for the agent to acknowledge it on its next turn; this resolves by itself (escalates at {{ clock(v.ack_deadline) }})</li>
                          }
                        </ol>
                        @if (e.pending) {
                          <div class="flex flex-wrap gap-2">
                            @if (late) {
                              <button type="button" class="sy-btn sy-btn-primary h-10" data-testid="veto-dismiss" [disabled]="control.isBusy(v.id)" (click)="dismiss(e)">Dismiss</button>
                            }
                            @if (e.sessionId && work.byId()[e.sessionId]) {
                              <button type="button" class="sy-btn sy-btn-soft h-10" (click)="openSession(e)">Open the session</button>
                            }
                          </div>
                        }
                      </article>
                    }
                    @case ('shells') {
                      @let r = e.shells!;
                      <article class="rounded-[var(--sy-radius-lg)] bg-[var(--sy-surface-2)] p-4 space-y-3" data-testid="needs-you-shells" [attr.data-shells]="e.sid">
                        <div class="text-[10.5px] font-semibold uppercase tracking-wide text-[#e9bd76]">Shells left running</div>
                        <h3 class="m-0 text-[17px] font-semibold leading-snug text-[var(--sy-text)]">{{ e.title }}</h3>
                        <p class="m-0 text-[12.5px] text-[var(--sy-text-secondary)]">{{ e.meta }}</p>
                        @if (r.shell_list?.length) {
                          <ul class="m-0 p-0 list-none space-y-1 text-[12px]" data-testid="shells-list">
                            @for (s of r.shell_list; track s.pid) {
                              <li class="flex items-baseline gap-2 min-w-0">
                                <code class="font-mono-ide text-[11.5px] text-[var(--sy-text)] truncate" [syTooltip]="s.command">{{ s.command }}</code>
                                <span class="shrink-0 text-[var(--sy-text-muted)]">pid {{ s.pid }}</span>
                              </li>
                            }
                          </ul>
                        }
                        @if (e.pending) {
                          <div class="flex flex-wrap gap-2">
                            <button type="button" class="sy-btn sy-btn-primary h-10" data-testid="shells-kill" [disabled]="control.isBusy(r.id)" (click)="remedy(e, 'kill')">Kill all</button>
                            @if (r.can_ask) {
                              <button type="button" class="sy-btn sy-btn-soft h-10" data-testid="shells-ask" [disabled]="control.isBusy(r.id)" (click)="remedy(e, 'ask')">Ask agent to clean up</button>
                            }
                            <button type="button" class="sy-btn sy-btn-soft h-10" data-testid="shells-keep" [disabled]="control.isBusy(r.id)" (click)="remedy(e, 'keep')" syTooltip="Hides this for a day">Keep running</button>
                          </div>
                          <p class="m-0 text-[11.5px] text-[var(--sy-text-muted)]">Change what Syre does about shells left running in Settings → Shells left running.</p>
                        } @else {
                          <p class="m-0 text-[12.5px] text-[#8fcb9b]" role="status">Done.</p>
                        }
                      </article>
                    }
                    @case ('review') {
                      <div class="rounded-[var(--sy-radius-lg)] bg-[var(--sy-surface-1)] min-h-[320px] flex flex-col" data-testid="needs-you-review">
                        <sy-review-panel class="flex-1" [item]="reviewItem(e)" [embedded]="true" (decided)="store.advance(e.key)" />
                      </div>
                    }
                  }
                </div>
              } @empty {
                <p class="p-5 text-[12.5px] text-[var(--sy-text-muted)]" data-testid="questions-empty">
                  Nothing needs you. When an agent anywhere in the fleet needs a decision, it shows up here.
                </p>
              }
            }
          </main>

          @if (questions.threadOpen() && store.current()?.question; as q) {
            <div class="fixed inset-0 z-50 flex flex-col justify-end md:static md:z-auto md:w-[400px] md:shrink-0 md:border-l md:border-[var(--sy-hairline)]">
              <div class="sy-backdrop flex-1 md:hidden" (click)="questions.threadOpen.set(false)"></div>
              <aside
                class="sy-drawer-bottom md:animate-none h-[80vh] md:h-full flex flex-col bg-[var(--sy-surface-1)] rounded-t-[var(--sy-radius-xl)] md:rounded-none shadow-2xl md:shadow-none"
                aria-label="Side conversation"
                data-testid="side-conversation"
              >
                <sy-side-conversation [question]="q" (closed)="questions.threadOpen.set(false)" />
              </aside>
            </div>
          }
        </div>
      </div>
    </app-modal-dialog>
  `,
  styles: `
    .sy-ny-dots {
      scrollbar-width: none;
      justify-content: safe center;
    }
    .sy-ny-dot {
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
    .sy-ny-dot::after {
      /* A taller hit area that never overlaps the neighbouring dots. */
      content: '';
      position: absolute;
      inset: -10px -2px;
    }
    .sy-ny-dot[data-tag='P1'] {
      background: rgba(232, 107, 107, 0.6);
    }
    .sy-ny-dot[data-tag='P3'] {
      background: rgba(255, 255, 255, 0.25);
    }
    .sy-ny-dot-done {
      background: rgba(105, 184, 121, 0.55) !important;
    }
    .sy-ny-dot-current {
      width: 20px;
      background: var(--sy-text) !important;
    }
    .sy-ny-dot:focus-visible {
      outline: none;
      box-shadow: var(--sy-focus-ring);
    }
    .sy-ny-slide {
      animation: sy-ny-in 220ms var(--sy-ease-out) both;
    }
    .sy-ny-facts {
      display: grid;
      grid-template-columns: max-content 1fr;
      gap: 6px 14px;
    }
    .sy-ny-facts dt {
      color: var(--sy-text-muted);
    }
    .sy-ny-facts dd {
      margin: 0;
      color: var(--sy-text);
    }
    .sy-ny-late {
      background: var(--sy-red);
      color: #1e0b0b;
    }
    .sy-ny-slide.sy-ny-from-left {
      animation-name: sy-ny-in-left;
    }
    @keyframes sy-ny-in {
      from {
        opacity: 0;
        transform: translateX(28px);
      }
      to {
        opacity: 1;
        transform: none;
      }
    }
    @keyframes sy-ny-in-left {
      from {
        opacity: 0;
        transform: translateX(-28px);
      }
      to {
        opacity: 1;
        transform: none;
      }
    }
    @keyframes sy-ny-fade {
      from {
        opacity: 0;
      }
      to {
        opacity: 1;
      }
    }
    @media (prefers-reduced-motion: reduce) {
      .sy-ny-slide,
      .sy-ny-slide.sy-ny-from-left {
        animation: sy-ny-fade 160ms linear both;
      }
    }
  `,
})
export class NeedsYouDialogComponent {
  readonly store = inject(NeedsYouStore);
  readonly questions = inject(QuestionsStore);
  readonly control = inject(ControlStore);
  readonly work = inject(WorkStore);
  private readonly fleet = inject(FleetStore);
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
      'sy-ny-panel max-md:!fixed max-md:!inset-0 shrink-0 flex flex-col bg-[var(--sy-bg-chrome)] overflow-hidden rounded-none md:w-[min(1120px,95vw)] md:max-h-[min(820px,92vh)] md:rounded-[var(--sy-radius-xl)]' +
      (tall ? ' md:h-[min(820px,92vh)]' : '')
    );
  });
  readonly currentList = computed<NeedsYouEntry[]>(() => {
    const c = this.store.current();
    return c ? [c] : [];
  });

  private touch: { x: number; y: number } | null = null;

  constructor() {
    // Answering, snoozing or handing a question to the commander moves on.
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

  /** Notes typed for override decisions, by entry key. */
  private readonly notes = signal<Record<string, string>>({});
  /** Decisions, resolutions and dismissals made here, for the receipts. */
  readonly decided = signal<Record<string, 'approve' | 'deny'>>({});
  readonly resolved = signal<Record<string, 'done' | 'declined'>>({});
  readonly dismissedHere = signal<Record<string, boolean>>({});

  since(iso: string | undefined): string {
    return formatSince(iso, this.store.now());
  }

  requestMachine(r: UserRequest): string {
    return r.machine_name || this.fleet.workersById()[r.worker_id]?.name || '';
  }

  requestImpactLine(r: UserRequest): string {
    const machine = this.requestMachine(r);
    const target = r.commander
      ? 'Blocking the commander'
      : r.chat_title
        ? `Blocking ${r.chat_title}`
        : machine
          ? `Blocking an agent on ${machine}`
          : 'Blocking an agent';
    return [target, resourceLabel(r.resource)].filter(Boolean).join(' · ');
  }

  veto(e: NeedsYouEntry): { reason: string; note: string } {
    return e.veto ? vetoDetails(e.veto) : { reason: '', note: '' };
  }

  escalated(v: Veto): boolean {
    return vetoEscalated(v, this.store.now());
  }

  duration(seconds: number): string {
    return formatDuration(seconds);
  }

  clock(iso: string | undefined): string {
    return iso ? new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : '';
  }

  scope(e: NeedsYouEntry): string {
    const o = e.override;
    if (!o) return '';
    const machine = o.worker_id ? this.fleet.workersById()[o.worker_id]?.name : undefined;
    const hold = this.control.holds().find((h) => h.id === o.hold_id);
    const session = e.sessionId ? this.work.byId()[e.sessionId] : undefined;
    return overrideScope(o, machine, hold, session ? `${session.sid} (${session.title})` : undefined);
  }

  noteFor(key: string): string {
    return this.notes()[key] ?? '';
  }

  setNote(key: string, value: string): void {
    this.notes.update((n) => ({ ...n, [key]: value }));
  }

  async decidePolicy(e: NeedsYouEntry, decision: 'approve' | 'deny'): Promise<void> {
    if (!e.policy) return;
    const res = await this.control.decideProposal(e.policy, decision);
    if (!res) return;
    if (res !== true) this.fleet.applyDirective(res);
    this.decided.update((d) => ({ ...d, [e.key]: decision }));
    this.forward(() => this.store.advance(e.key));
  }

  async decide(e: NeedsYouEntry, decision: 'approve' | 'deny'): Promise<void> {
    if (!e.override) return;
    if (await this.store.decideOverride(e.override, decision, this.noteFor(e.key))) {
      this.decided.update((d) => ({ ...d, [e.key]: decision }));
      this.forward(() => this.store.advance(e.key));
    }
  }

  async dismiss(e: NeedsYouEntry): Promise<void> {
    if (!e.veto) return;
    if (await this.store.dismissVeto(e.veto)) {
      this.dismissedHere.update((d) => ({ ...d, [e.key]: true }));
      this.forward(() => this.store.advance(e.key, 400));
    }
  }

  reviewItem(e: NeedsYouEntry) {
    return (e.sessionId && this.work.byId()[e.sessionId]) || e.review || null;
  }

  async resolve(e: NeedsYouEntry, status: 'done' | 'declined'): Promise<void> {
    if (e.request && (await this.control.resolveRequest(e.request, status))) {
      this.resolved.update((r) => ({ ...r, [e.key]: status }));
      this.forward(() => this.store.advance(e.key));
    }
  }

  async remedy(e: NeedsYouEntry, action: 'kill' | 'ask' | 'keep'): Promise<void> {
    if (e.shells && (await this.control.remedyShells(e.shells, action))) this.forward(() => this.store.advance(e.key));
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

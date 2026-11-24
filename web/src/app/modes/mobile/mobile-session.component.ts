import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { map } from 'rxjs';
import { VETO_REASONS, VetoReason, causeLine } from '../../core/models/control.models';
import { githubLine } from '../../core/models/review.models';
import { AgentChatStore } from '../../core/state/agent-chat.store';
import { ControlStore } from '../../core/state/control.store';
import { formatSince } from '../../core/state/questions-logic';
import { WorkStore } from '../../core/state/work.store';
import { sessionActions } from '../../features/control/session-controls.component';
import { shortWhen } from '../../features/control/pause-everything.component';
import { IconComponent } from '../../shared/icon.component';
import { ModalDialogComponent } from '../../shared/modal-dialog.component';
import { ControlLabelComponent } from '../control-label.component';
import { NeedsYouItemComponent } from '../../features/needs-you/needs-you-item.component';
import { NeedsYouStore } from '../../core/state/needs-you.store';

/**
 * A session on a phone: what it is doing and why, its question with the options to tap,
 * the controls (suspend, hold, veto) with big targets, and the receipts of what you did.
 */
@Component({
  selector: 'ks-mobile-session',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RouterLink, IconComponent, ControlLabelComponent, ModalDialogComponent, NeedsYouItemComponent],
  host: { class: 'flex flex-col h-full min-h-0 overflow-y-auto' },
  template: `
    <div class="px-4 pt-4 pb-1">
      <a routerLink="/inbox" class="ks-btn ks-btn-sm h-[44px] -ml-2 no-underline" data-testid="mobile-back">
        <span class="inline-flex rotate-180"><app-icon name="chevron-right" [size]="12" /></span>
        Inbox
      </a>
    </div>
    @if (item(); as it) {
      <div class="px-4 flex items-center gap-2">
        <ks-control-label [label]="it.control" />
        <h1 class="m-0 text-[15px] font-semibold truncate">{{ it.sessionKind === 'terminal' ? 'terminal' : it.agent }} · {{ it.title }}</h1>
      </div>
      <div class="px-4 pt-1 text-[12.5px] text-[var(--ks-text-secondary)]">{{ it.sid }} · {{ it.machine }} · started {{ since(it.createdAt) }} ago{{ it.origin === 'orchestrator' ? ' by the orchestrator' : '' }}</div>

      @if (entry(); as e) {
        <div class="mx-4 mt-4" data-testid="mobile-session-needs-you"><ks-needs-you-item [entry]="e" /></div>
      }

      <dl class="mx-4 mt-4 grid grid-cols-[52px_minmax(0,1fr)] gap-x-2.5 gap-y-1.5 text-[13px] leading-snug">
        <dt class="font-mono-ide text-[11px] text-[var(--ks-text-muted)]">GOAL</dt>
        <dd class="m-0">{{ it.goal }}</dd>
        <dt class="font-mono-ide text-[11px] text-[var(--ks-text-muted)]">NOW</dt>
        <dd class="m-0" aria-live="polite">{{ it.now }}</dd>
        @if (cause()) {
          <dt class="font-mono-ide text-[11px] text-[var(--ks-text-muted)]">WHY</dt>
          <dd class="m-0">{{ cause() }}</dd>
        }
      </dl>

      <div class="grid grid-cols-2 gap-2 px-4 pt-4" role="group" aria-label="Controls">
        @if (actions().resume) {
          <button type="button" class="ks-btn ks-btn-soft h-[44px] text-[var(--ks-violet)]" data-testid="mobile-resume-session" [disabled]="busy()" (click)="resume()">Resume</button>
        } @else {
          <button type="button" class="ks-btn ks-btn-soft h-[44px]" data-testid="mobile-suspend" [disabled]="busy() || !actions().suspend" (click)="suspend()">Suspend</button>
        }
        @if (it.hold; as h) {
          <button type="button" class="ks-btn ks-btn-soft h-[44px] text-[#e6b763]" data-testid="mobile-release" [disabled]="control.isBusy(h.id)" (click)="control.releaseHold(h)">Release hold</button>
        } @else {
          <button type="button" class="ks-btn ks-btn-soft h-[44px]" data-testid="mobile-hold" [disabled]="busy() || !it.live" (click)="control.holdSession(it.kind, it.id)">Hold</button>
        }
        <button type="button" class="ks-btn h-[44px] bg-[var(--ks-red-soft)] text-[#f3a3a3]" data-testid="mobile-veto" [disabled]="busy() || !(actions().veto || it.column === 'review')" (click)="vetoing.set(true)">Veto…</button>
        @if (it.column === 'review') {
          <button type="button" class="ks-btn ks-btn-soft h-[44px]" data-testid="mobile-approve" [disabled]="approving()" (click)="approve()">Approve</button>
        } @else {
          <a class="ks-btn ks-btn-soft h-[44px] no-underline" [routerLink]="['/command', it.sid]">Open full view</a>
        }
      </div>
      @if (reviewNote()) {
        <p class="mx-4 mt-2 mb-0 text-[12.5px] text-[var(--ks-text-secondary)]" role="status" data-testid="mobile-review-note">{{ reviewNote() }}</p>
      }

      @if (it.kind === 'chat' && it.chat) {
        <form class="mx-4 mt-4 flex gap-2" (submit)="$event.preventDefault(); reply()">
          <label class="ks-sr-only" for="ks-mobile-reply">Reply to {{ it.agent }}</label>
          <input id="ks-mobile-reply" class="ks-field flex-1 h-[44px]" placeholder="Reply…" data-testid="mobile-reply" [value]="draft()" (input)="draft.set($any($event.target).value)" />
          <button type="submit" class="ks-btn ks-btn-primary h-[44px]" [disabled]="!draft().trim()">Send</button>
        </form>
      }

      <h2 class="px-4 pt-5 mb-1 text-[12px] font-semibold uppercase tracking-wider text-[var(--ks-text-secondary)]">Receipts</h2>
      <ul class="m-0 px-4 pb-6 list-none space-y-1.5" data-testid="mobile-receipts">
        @for (e of receipts(); track e.id) {
          <li class="flex gap-2.5 text-[13px]">
            <span class="text-[#8fcb9b]" aria-hidden="true">✓</span>
            <time class="font-mono-ide text-[11.5px] text-[var(--ks-text-muted)] shrink-0" [attr.datetime]="e.created_at">{{ when(e.created_at) }}</time>
            <span class="break-words">{{ e.summary }}</span>
          </li>
        } @empty {
          <li class="text-[13px] text-[var(--ks-text-muted)]">Nothing you did here yet.</li>
        }
      </ul>

      @if (vetoing()) {
        <app-modal-dialog labelledBy="ks-mveto-title" panelClass="!fixed !inset-x-0 !bottom-0 w-full rounded-t-[22px] bg-[var(--ks-surface-2)] p-5 pb-7" (close)="vetoing.set(false)">
          <div class="mx-auto mb-3 h-1 w-10 rounded-full bg-white/20" aria-hidden="true"></div>
          <h2 id="ks-mveto-title" class="m-0 text-[16px] font-semibold">Veto: why?</h2>
          <p class="mt-1 mb-3 text-[13px] text-[var(--ks-text-secondary)]">It stops, and the orchestrator remembers not to retry it the same way.</p>
          <div class="grid grid-cols-2 gap-2" role="radiogroup" aria-label="Veto reason">
            @for (r of reasons; track r.id) {
              <button type="button" role="radio" class="ks-btn ks-btn-soft h-[44px]" [attr.aria-checked]="reason() === r.id" [class.!bg-[var(--ks-red-soft)]]="reason() === r.id" (click)="reason.set(r.id)">{{ r.label }}</button>
            }
          </div>
          <div class="grid gap-2 mt-4">
            <button type="button" class="ks-btn h-[44px] bg-[var(--ks-red)] !text-[#1e0b0b] font-semibold" data-testid="mobile-veto-confirm" [disabled]="!reason()" (click)="veto()">Veto</button>
            <button type="button" class="ks-btn ks-btn-soft h-[44px]" autofocus (click)="vetoing.set(false)">Cancel</button>
          </div>
        </app-modal-dialog>
      }
    } @else {
      <p class="px-4 pt-4 text-[13px] text-[var(--ks-text-secondary)]">No session {{ sid() }} here (it may have been removed).</p>
    }
  `,
})
export class MobileSessionComponent {
  readonly work = inject(WorkStore);
  readonly control = inject(ControlStore);
  private readonly agentChat = inject(AgentChatStore);
  readonly reasons = VETO_REASONS;

  readonly sid = toSignal(inject(ActivatedRoute).paramMap.pipe(map((p) => p.get('sid') ?? '')), { initialValue: '' });
  readonly item = computed(() => this.work.bySid(this.sid()) ?? null);
  private readonly needsYou = inject(NeedsYouStore);
  readonly entry = computed(() => {
    const it = this.item();
    return it ? this.needsYou.entryFor(it.id) : undefined;
  });
  readonly actions = computed(() => {
    const it = this.item();
    return it ? sessionActions(it.kind, it.status) : { suspend: false, resume: false, cancel: false, veto: false };
  });
  readonly busy = computed(() => {
    const it = this.item();
    return !!it && this.control.isBusy(it.id);
  });
  readonly cause = computed(() => {
    const it = this.item();
    const c = it?.task?.cause ?? it?.chat?.cause;
    return c && c.actor === 'user' ? causeLine(it!.status, c) : '';
  });
  readonly receipts = computed(() => {
    const it = this.item();
    if (!it) return [];
    return this.control
      .events()
      .filter((e) => (it.kind === 'task' ? e.task_id === it.id : e.chat_id === it.id))
      .slice(0, 8);
  });

  readonly vetoing = signal(false);
  readonly reason = signal<VetoReason | ''>('');
  readonly draft = signal('');

  since(iso: string): string {
    return formatSince(iso);
  }

  when(iso: string): string {
    return shortWhen(iso);
  }

  suspend(): void {
    const it = this.item();
    if (!it) return;
    void (it.kind === 'task' ? this.control.suspendTask(it.id) : this.control.suspendChat(it.id));
  }

  resume(): void {
    const it = this.item();
    if (!it) return;
    void (it.kind === 'task' ? this.control.resumeTask(it.id) : this.control.resumeChat(it.id));
  }

  async veto(): Promise<void> {
    const it = this.item();
    const r = this.reason();
    if (!it || !r) return;
    const ok = it.kind === 'task' ? await this.control.vetoTask(it.id, r) : await this.control.vetoChat(it.id, r);
    if (ok) {
      this.vetoing.set(false);
      this.reason.set('');
    }
  }

  readonly approving = signal(false);
  readonly reviewNote = signal('');

  /** Approves the work on the hub (which also approves its PR with gh). */
  async approve(): Promise<void> {
    const it = this.item();
    if (!it) return;
    this.approving.set(true);
    try {
      const res = await this.work.review(it.id, 'approved');
      this.reviewNote.set(['Approved.', githubLine(res.review)].filter(Boolean).join(' '));
    } catch (err) {
      this.reviewNote.set(err instanceof Error ? err.message : 'Could not approve');
    } finally {
      this.approving.set(false);
    }
  }

  reply(): void {
    const it = this.item();
    const t = this.draft().trim();
    if (!it?.chat || !t) return;
    if (this.agentChat.send(it.chat, t)) this.draft.set('');
  }

}

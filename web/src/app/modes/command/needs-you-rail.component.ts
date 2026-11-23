import { ChangeDetectionStrategy, Component, DestroyRef, ElementRef, computed, effect, inject, input, signal, untracked } from '@angular/core';
import { Router } from '@angular/router';
import { FleetEvent, UserRequest, VETO_REASONS, resourceLabel } from '../../core/models/control.models';
import { QuestionView } from '../../core/models/question.models';
import { ControlStore } from '../../core/state/control.store';
import { FleetStore } from '../../core/state/fleet.store';
import { NeedsYouStore } from '../../core/state/needs-you.store';
import { QuestionsStore } from '../../core/state/questions.store';
import { formatSince } from '../../core/state/questions-logic';
import { WorkItem, parseGitSummary } from '../../core/state/work-logic';
import { WorkStore } from '../../core/state/work.store';
import { YourActionsComponent } from '../../features/control/your-actions.component';
import { IconComponent } from '../../shared/icon.component';
import { TooltipDirective } from '../../shared/tooltip.directive';
import { ModeUiStore } from '../mode-ui.store';

/** A veto event's reason label and note (from its meta). */
export function vetoDetails(v: FleetEvent): { reason: string; note: string } {
  let meta: { reason?: string; note?: string } = {};
  try {
    meta = v.meta_json ? (JSON.parse(v.meta_json) as typeof meta) : {};
  } catch {
    meta = {};
  }
  const id = meta.reason || v.action || '';
  return { reason: VETO_REASONS.find((r) => r.id === id)?.label ?? id.replace(/_/g, ' '), note: (meta.note || '').trim() };
}

/** One question as a rail card: who is blocked, the options, Ask about this. */
@Component({
  selector: 'ks-rail-question',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent],
  host: { class: 'block' },
  template: `
    @let q = question();
    <article class="ks-rail-card" [attr.data-rail-item]="'q:' + q.id" [attr.data-question]="q.sid" tabindex="-1" [attr.aria-labelledby]="'ks-rq-' + q.id">
      <div class="flex items-start gap-2">
        <span class="ks-ptag ks-ptag-p2">{{ q.kind === 'permission' ? 'ASK' : 'P2' }}</span>
        <h3 [id]="'ks-rq-' + q.id" class="m-0 flex-1 min-w-0 text-[12.5px] font-medium leading-snug text-[var(--ks-text)] break-words">{{ q.prompt }}</h3>
      </div>
      <div class="mt-1 text-[11.5px] text-[var(--ks-text-muted)] truncate">
        {{ who() }} · {{ since() }}
        @if (q.status === 'delegated') {
          · <span class="text-[var(--ks-violet)]">orchestrator deciding</span>
        }
      </div>
      <div class="mt-2 flex flex-wrap gap-1">
        @for (o of q.options ?? []; track o.id; let i = $index) {
          <button
            type="button"
            class="ks-btn ks-btn-sm ks-btn-soft max-w-full"
            [attr.data-testid]="'rail-option-' + i"
            [disabled]="busy()"
            [attr.aria-label]="(i + 1) + ': ' + o.label + (o.recommended ? ' (recommended)' : '')"
            (click)="store.answer(q, { option_id: o.id })"
          >
            <span class="font-mono-ide text-[10px] text-[var(--ks-text-muted)]">{{ i + 1 }}</span>
            <span class="truncate">{{ o.label }}</span>
            @if (o.recommended) {
              <span class="text-[10px] text-[var(--ks-text-muted)]">rec.</span>
            }
          </button>
        }
        <button type="button" class="ks-btn ks-btn-sm text-[#cfe2ff] bg-[var(--ks-blue-soft)]" data-testid="rail-ask" (click)="askAbout()">
          <app-icon name="question" [size]="11" />
          Ask about this
        </button>
      </div>
      @if (q.allow_text) {
        <form class="mt-2 flex gap-1.5" (submit)="$event.preventDefault(); sendText()">
          <label class="ks-sr-only" [attr.for]="'ks-rq-text-' + q.id">Your own answer</label>
          <input
            [id]="'ks-rq-text-' + q.id"
            class="ks-field ks-field-sm flex-1 min-w-0"
            data-testid="rail-text"
            placeholder="Or type an answer…"
            [value]="text()"
            (input)="text.set($any($event.target).value)"
          />
          <button type="submit" class="ks-btn ks-btn-sm ks-btn-soft" [disabled]="!text().trim() || busy()">Send</button>
        </form>
      }
    </article>
  `,
})
export class RailQuestionComponent {
  readonly question = input.required<QuestionView>();
  readonly store = inject(QuestionsStore);
  private readonly needsYou = inject(NeedsYouStore);
  readonly text = signal('');
  readonly busy = computed(() => this.store.isBusy(this.question().id));
  readonly since = computed(() => `blocked ${formatSince(this.question().blocked_since, this.needsYou.now())}`);
  readonly who = computed(() => {
    const q = this.question();
    const agent = q.orchestrator ? 'orchestrator' : q.agent || 'agent';
    return [agent, q.chat_title || q.chat_sid, q.machine_name].filter(Boolean).join(' · ');
  });

  askAbout(): void {
    this.store.open(this.question().id);
    this.store.threadOpen.set(true);
  }

  sendText(): void {
    const t = this.text().trim();
    if (!t) return;
    void this.store.answer(this.question(), { text: t }).then((ok) => ok && this.text.set(''));
  }
}

/**
 * Command's right rail: the one place on the page for everything that needs the user —
 * questions, agents' requests, vetoes and how they were delivered, override requests
 * (no hub API yet), reviews ready, and the holds the user placed — plus "Your actions".
 */
@Component({
  selector: 'ks-needs-you-rail',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective, RailQuestionComponent, YourActionsComponent],
  host: { class: 'flex flex-col min-h-0' },
  template: `
    <div class="flex items-center justify-between gap-2 px-4 pt-3 pb-2 shrink-0">
      <h2
        id="ks-needs-you-heading"
        class="m-0 text-[11.5px] font-semibold uppercase tracking-wider"
        [class.text-[var(--ks-amber)]]="store.count() > 0"
        [class.text-[var(--ks-text-secondary)]]="store.count() === 0"
        data-testid="rail-heading"
      >
        Needs you@if (!ui.firstRun()) { · {{ store.count() }} }
      </h2>
      @if (!ui.firstRun()) {
        <button type="button" class="ks-btn ks-btn-sm text-[var(--ks-blue)]" data-testid="rail-all-questions" (click)="questions.open()">All questions</button>
      }
    </div>

    <div class="flex-1 min-h-0 overflow-y-auto px-3 pb-4 space-y-2" role="region" aria-labelledby="ks-needs-you-heading" data-testid="needs-you-rail">
      @if (ui.firstRun()) {
        <p class="px-1 text-[13px] leading-relaxed text-[var(--ks-text-secondary)]">
          When an agent is blocked on a decision, its question lands here with its options and an “Ask about this” button, so you can find out what
          you're agreeing to before you answer.
        </p>
      } @else {
        @if (store.empty()) {
          <p class="px-1 text-[12.5px] leading-relaxed text-[var(--ks-text-muted)]" data-testid="rail-empty">
            Nothing needs you. Questions, requests from agents, vetoes and finished work to review land here.
          </p>
        }

        @for (v of store.vetoes(); track v.id) {
          <article class="ks-rail-card bg-[var(--ks-red-soft)]" [attr.data-rail-item]="'v:' + v.id" data-testid="rail-veto">
            <div class="flex items-start gap-2">
              <span class="ks-ptag ks-ptag-p1">VETO</span>
              <h3 class="m-0 flex-1 min-w-0 text-[12.5px] font-medium leading-snug text-[var(--ks-text)]">{{ vetoTitle(v) }}</h3>
            </div>
            <div class="mt-1 text-[11.5px] text-[var(--ks-text-muted)]">
              {{ vetoMeta(v) }}
            </div>
            <div class="mt-2 flex gap-1">
              @if (itemFor(v); as it) {
                <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" (click)="ui.openInCommand(it)">Open</button>
              }
              <button type="button" class="ks-btn ks-btn-sm" (click)="store.dismissVeto(v)">Dismiss</button>
            </div>
          </article>
        }

        @for (q of store.questions(); track q.id) {
          <ks-rail-question [question]="q" />
        }

        @for (r of store.requests(); track r.id) {
          <article class="ks-rail-card" [attr.data-rail-item]="'r:' + r.id" [attr.data-request]="r.sid">
            <div class="flex items-start gap-2">
              <span class="ks-ptag ks-ptag-p2">P2</span>
              <h3 class="m-0 flex-1 min-w-0 text-[12.5px] font-medium leading-snug text-[var(--ks-text)] break-words">{{ r.description }}</h3>
            </div>
            <div class="mt-1 text-[11.5px] text-[var(--ks-text-muted)]">request · {{ requestMeta(r) }}</div>
            <div class="mt-2 flex gap-1">
              <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" [attr.data-testid]="'request-done-' + r.sid" [disabled]="control.isBusy(r.id)" (click)="control.resolveRequest(r, 'done')">
                <app-icon name="check" [size]="11" />
                Mark done
              </button>
              <button type="button" class="ks-btn ks-btn-sm" [attr.data-testid]="'request-decline-' + r.sid" [disabled]="control.isBusy(r.id)" (click)="control.resolveRequest(r, 'declined')">Decline</button>
            </div>
          </article>
        }

        @for (it of store.reviews(); track it.id) {
          <article class="ks-rail-card" [attr.data-rail-item]="'rv:' + it.id" data-testid="rail-review">
            <div class="flex items-start gap-2">
              <span class="ks-ptag ks-ptag-p3">REVIEW</span>
              <h3 class="m-0 flex-1 min-w-0 text-[12.5px] font-medium leading-snug text-[var(--ks-text)] break-words">{{ it.title }}</h3>
            </div>
            <div class="mt-1 text-[11.5px] text-[var(--ks-text-muted)] truncate">{{ reviewMeta(it) }}</div>
            <div class="mt-2 flex gap-1">
              <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" (click)="review(it)">Review</button>
              <button type="button" class="ks-btn ks-btn-sm" (click)="work.approve(it.id)">Approve</button>
            </div>
          </article>
        }

        @if (store.holds().length) {
          <h3 class="m-0 pt-3 px-1 text-[11px] font-semibold uppercase tracking-wider text-[var(--ks-text-muted)]">Your holds · {{ store.holds().length }}</h3>
          @for (h of store.holds(); track h.id) {
            <div class="flex items-start gap-2 rounded-[var(--ks-radius-md)] px-2.5 py-2 text-[12px] bg-[var(--ks-raised)]" [attr.data-hold]="h.sid">
              <app-icon name="lock" [size]="12" />
              <span class="flex-1 min-w-0 break-words text-[var(--ks-text-secondary)]">{{ h.summary }}</span>
              <button type="button" class="ks-btn ks-btn-sm shrink-0" [attr.data-testid]="'hold-release-' + h.sid" [disabled]="control.isBusy(h.id)" (click)="control.releaseHold(h)" ksTooltip="Allow automatic actions here again">Release</button>
            </div>
          }
        }

        <div class="pt-3 px-1">
          <ks-your-actions [limit]="6" />
        </div>
      }
    </div>
  `,
  styles: `
    :host ::ng-deep .ks-rail-card {
      border-radius: var(--ks-radius-lg);
      background: var(--ks-surface-2);
      padding: 10px 11px;
      outline: none;
      transition: box-shadow var(--ks-dur-base) var(--ks-ease-out);
    }
    :host ::ng-deep .ks-rail-card:focus-visible,
    :host ::ng-deep .ks-rail-card.ks-flash {
      box-shadow: inset 0 0 0 1px var(--ks-amber);
    }
    :host ::ng-deep .ks-ptag {
      flex-shrink: 0;
      margin-top: 1px;
      padding: 1px 6px;
      border-radius: 4px;
      font-family: 'JetBrains Mono', ui-monospace, monospace;
      font-size: 10px;
      font-weight: 500;
    }
    :host ::ng-deep .ks-ptag-p1 {
      background: var(--ks-red);
      color: #1e0b0b;
    }
    :host ::ng-deep .ks-ptag-p2 {
      background: rgba(224, 168, 76, 0.18);
      color: #e9c27f;
    }
    :host ::ng-deep .ks-ptag-p3 {
      background: var(--ks-raised-strong);
      color: var(--ks-text-secondary);
    }
  `,
})
export class NeedsYouRailComponent {
  readonly store = inject(NeedsYouStore);
  readonly questions = inject(QuestionsStore);
  readonly control = inject(ControlStore);
  readonly work = inject(WorkStore);
  readonly ui = inject(ModeUiStore);
  private readonly fleet = inject(FleetStore);
  private readonly router = inject(Router);
  private readonly host = inject(ElementRef<HTMLElement>);

  constructor() {
    // Keep "blocked 12m" and the veto window current while the rail is on screen.
    const timer = setInterval(() => this.store.now.set(Date.now()), 30_000);
    inject(DestroyRef).onDestroy(() => clearInterval(timer));
    // "Answer in Needs you" from a chat: bring that card into view and focus it.
    effect(() => {
      const req = this.store.focusRequest();
      if (!req) return;
      untracked(() =>
        setTimeout(() => {
          const el = (this.host.nativeElement as HTMLElement).querySelector<HTMLElement>(`[data-rail-item="q:${CSS.escape(req.id)}"]`);
          if (!el) return;
          el.scrollIntoView({ block: 'nearest', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' });
          el.focus();
          el.classList.add('ks-flash');
          setTimeout(() => el.classList.remove('ks-flash'), 1600);
        }),
      );
    });
  }

  since(iso: string): string {
    return formatSince(iso, this.store.now());
  }

  vetoTitle(v: FleetEvent): string {
    const it = this.itemFor(v);
    if (it) return `Vetoed ${it.sid}: ${it.title}`;
    const s = v.summary.replace(/^The user vetoed /, 'Vetoed ');
    const cut = s.search(/ — reason:|\. Don/);
    return cut > 0 ? s.slice(0, cut) : s;
  }

  vetoMeta(v: FleetEvent): string {
    const m = vetoDetails(v);
    const why = m.note ? `${m.reason} (“${m.note}”)` : m.reason;
    return [why, `${this.since(v.created_at)} ago`, 'enforced', 'the orchestrator was told'].filter(Boolean).join(' · ');
  }

  itemFor(v: FleetEvent): WorkItem | undefined {
    const id = v.task_id || v.chat_id;
    return id ? this.work.byId()[id] : undefined;
  }

  requestMeta(r: UserRequest): string {
    const machine = this.fleet.workersById()[r.worker_id]?.name ?? '';
    return [machine, resourceLabel(r.resource), `${this.since(r.created_at)} ago`].filter(Boolean).join(' · ');
  }

  reviewMeta(it: WorkItem): string {
    const g = parseGitSummary(it.task?.git_summary_json);
    const commits = g?.commits?.length ? `${g.commits.length} commit${g.commits.length === 1 ? '' : 's'}` : '';
    return [it.agent, it.machine, commits].filter(Boolean).join(' · ');
  }

  review(it: WorkItem): void {
    void this.router.navigate(['/work', it.sid]);
  }

}

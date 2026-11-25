import { ChangeDetectionStrategy, Component, computed, effect, inject, input, output, signal, untracked } from '@angular/core';
import { Router } from '@angular/router';
import { TaskEvent } from '../../core/models/konsert.models';
import { Acceptance, AcceptanceItem, DeliveryPlan, approveActionLabel, deliveryOutcomeLine, prStatusLine } from '../../core/models/review.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { AgentChatStore } from '../../core/state/agent-chat.store';
import { FleetStore } from '../../core/state/fleet.store';
import { WorkItem } from '../../core/state/work-logic';
import { WorkStore } from '../../core/state/work.store';
import { KeepToggleComponent } from '../../features/control/keep-toggle.component';
import { SessionControlsComponent } from '../../features/control/session-controls.component';
import { IconComponent } from '../../shared/icon.component';

import { ControlLabelComponent } from '../control-label.component';
import { WallStore } from '../wall/wall.store';
import { receiptsFor } from './review-logic';

/**
 * Work's side panel for the selected card: summary, receipts against acceptance, and the
 * decision (approve, request changes, veto) for review cards; the controls for live ones.
 */
@Component({
  selector: 'ks-review-panel',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, ControlLabelComponent, SessionControlsComponent, KeepToggleComponent],
  host: { class: 'flex flex-col min-h-0' },
  template: `
    @if (item(); as it) {
      <div class="flex-1 min-h-0 overflow-y-auto px-4 py-3.5 flex flex-col gap-3" data-testid="review-panel" [attr.data-item]="it.sid">
        <div class="flex items-center gap-2">
          <span class="font-mono-ide text-[11px] text-[var(--ks-text-muted)]">{{ it.sid }}</span>
          <ks-control-label [label]="it.control" />
          <span class="flex-1"></span>
          @if (!embedded()) {
            <button type="button" class="ks-icon-btn ks-icon-btn-sm" aria-label="Close panel" (click)="closed()">
              <app-icon name="close" [size]="12" />
            </button>
          }
        </div>
        <h2 class="m-0 text-[15px] font-semibold leading-snug text-[var(--ks-text)]">{{ it.title }}</h2>
        <div class="text-[12px] text-[var(--ks-text-secondary)]">{{ sub() }}</div>

        <div class="rounded-[var(--ks-radius-md)] bg-[var(--ks-surface-2)] px-3 py-2.5 text-[12.5px] leading-relaxed whitespace-pre-wrap break-words" data-testid="review-summary">{{ summary() }}</div>

        <h3 class="m-0 mt-1 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--ks-text-secondary)]">Receipts vs acceptance</h3>
        <div class="grid gap-3">
          <section aria-labelledby="ks-acc-h">
            <h4 id="ks-acc-h" class="m-0 mb-1 text-[11px] text-[var(--ks-text-muted)]" data-testid="acceptance-source">{{ acceptanceHeading() }}</h4>
            <ul class="m-0 p-0 list-none space-y-1" data-testid="acceptance">
              @for (a of acceptance(); track $index) {
                <li class="flex gap-2 text-[12.5px]">
                  <span [class.text-[#8fcb9b]]="a.done" [class.text-[var(--ks-text-muted)]]="!a.done" aria-hidden="true">{{ a.done ? '✓' : '○' }}</span>
                  <span class="break-words"><span class="ks-sr-only">{{ a.done ? 'Done: ' : 'Open: ' }}</span>{{ a.text }}</span>
                </li>
              }
            </ul>
          </section>
          <section aria-labelledby="ks-rec-h">
            <h4 id="ks-rec-h" class="m-0 mb-1 text-[11px] text-[var(--ks-text-muted)]">What it shows for it</h4>
            @if (receipts().length === 0) {
              <p class="m-0 text-[12px] text-[var(--ks-text-muted)]">No commits, milestones or summary reported yet.</p>
            }
            <ul class="m-0 p-0 list-none space-y-1" data-testid="review-receipts">
              @for (r of receipts(); track $index) {
                <li class="flex gap-2 text-[12.5px]">
                  <span class="shrink-0" [class.text-[#8fcb9b]]="r.kind !== 'summary'" [class.text-[var(--ks-text-muted)]]="r.kind === 'summary'" aria-hidden="true">{{ r.kind === 'summary' ? '¶' : '✓' }}</span>
                  <span class="break-words" [class.font-mono-ide]="r.kind === 'commit' || r.kind === 'diff'" [class.text-[12px]]="r.kind === 'commit' || r.kind === 'diff'">{{ r.text }}</span>
                </li>
              }
            </ul>
          </section>
        </div>

        @if (prInfo(); as pr) {
          <div class="rounded-[var(--ks-radius-md)] bg-[var(--ks-surface-2)] px-3 py-2 text-[12px] flex flex-wrap items-center gap-2" data-testid="review-pr">
            @if (pr.url) {
              <a [href]="pr.url" target="_blank" rel="noopener noreferrer" class="underline text-[var(--ks-text)] font-medium">{{ pr.label }}</a>
            } @else {
              <span class="font-medium text-[var(--ks-text)]">{{ pr.label }}</span>
            }
          </div>
        }
        @if (compareUrl(); as cmp) {
          <div class="rounded-[var(--ks-radius-md)] bg-[var(--ks-surface-2)] px-3 py-2 text-[12px]" data-testid="review-compare-url">
            <a [href]="cmp" target="_blank" rel="noopener noreferrer" class="underline text-[var(--ks-text)]">Open PR/MR by hand →</a>
          </div>
        }

        @if (decision(); as d) {
          <p class="m-0 text-[12px] text-[var(--ks-text-secondary)]" data-testid="review-decision">{{ d }}</p>
        }
        @if (it.column === 'review') {
          @if (approvePreview(); as preview) {
            <p class="m-0 text-[12px] text-[var(--ks-text-secondary)]" data-testid="review-delivery-action">{{ preview }}</p>
          }
          <div class="flex flex-wrap gap-1.5 pt-1" role="group" aria-label="Decision">
            <button type="button" class="ks-btn ks-btn-sm bg-[var(--ks-green-soft)] text-[#a8dbb3]" data-testid="review-approve" [disabled]="sending()" (click)="approve(it)">
              <app-icon name="check" [size]="11" />
              Approve
            </button>
            <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" data-testid="review-request-changes" [attr.aria-expanded]="changing()" (click)="changing.set(!changing())">Request changes</button>
            <ks-session-controls [kind]="it.kind" [sessionId]="it.id" [status]="it.status" [vetoAlways]="true" />
          </div>
        } @else {
          <div class="flex flex-wrap items-center gap-1.5 pt-1">
            <ks-session-controls [kind]="it.kind" [sessionId]="it.id" [status]="it.status" [showHold]="it.live" />
            @if (it.live) {
              <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" (click)="changing.set(!changing())" [attr.aria-expanded]="changing()">Steer…</button>
            }
          </div>
        }

        @if (changing()) {
          <form class="flex flex-col gap-1.5" (submit)="$event.preventDefault(); sendChanges(it)">
            <label class="text-[11.5px] text-[var(--ks-text-secondary)]" for="ks-review-note">{{ noteLabel() }}</label>
            <textarea id="ks-review-note" rows="3" class="ks-field w-full resize-none text-[12.5px]" data-testid="review-note" [value]="note()" (input)="note.set($any($event.target).value)"></textarea>
            <div class="flex justify-end gap-1.5">
              <button type="button" class="ks-btn ks-btn-sm" (click)="changing.set(false)">Cancel</button>
              <button type="submit" class="ks-btn ks-btn-sm ks-btn-primary" data-testid="review-send" [disabled]="!note().trim() || sending()">Send</button>
            </div>
          </form>
        }
        @if (message()) {
          <p class="m-0 text-[12px] text-[var(--ks-text-secondary)]" role="status" data-testid="review-message">{{ message() }}</p>
        }
        @if (error()) {
          <p class="m-0 text-[12px] text-[#f3a5a5]" role="alert">{{ error() }}</p>
        }

        <div class="flex flex-wrap gap-1.5 pt-2 border-t border-[var(--ks-hairline)]">
          <button type="button" class="ks-btn ks-btn-sm" (click)="openInCommand(it)"><app-icon name="orchestrator" [size]="12" />Open in Command</button>
          <button type="button" class="ks-btn ks-btn-sm" [attr.aria-pressed]="work.isExpedited(it.id)" data-testid="toggle-expedite" (click)="toggleExpedite(it)">⚑ {{ work.isExpedited(it.id) ? 'Remove from Expedite' : 'Expedite' }}</button>
          <button type="button" class="ks-btn ks-btn-sm" [attr.aria-pressed]="wall.isPinned(it.id)" (click)="wall.togglePin(it.id)"><app-icon name="split" [size]="12" />{{ wall.isPinned(it.id) ? 'Unpin from Wall' : 'Pin to Wall' }}</button>
          <ks-keep-toggle [kind]="it.kind" [id]="it.id" [keep]="work.isKept(it.id)" />
        </div>
        @if (compactedNote(); as note) {
          <p class="m-0 text-[11.5px] text-[var(--ks-text-muted)]" data-testid="review-compacted">{{ note }}</p>
        }
      </div>
    } @else {
      <div class="p-4 text-[12.5px] leading-relaxed text-[var(--ks-text-muted)]" data-testid="review-empty">
        Select a card to see its summary, its receipts against what was asked, and the decision: approve, request changes or veto.
      </div>
    }
  `,
})
export class ReviewPanelComponent {
  readonly item = input<WorkItem | null>(null);
  /** Inside the Needs-you dialog: no close button; decisions are reported. */
  readonly embedded = input(false);
  /** Approved or changes requested. */
  readonly decided = output<void>();
  private readonly itemId = computed(() => this.item()?.id ?? '');
  readonly work = inject(WorkStore);
  readonly wall = inject(WallStore);
  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);
  private readonly agentChat = inject(AgentChatStore);
  private readonly router = inject(Router);

  readonly events = signal<TaskEvent[]>([]);
  readonly changing = signal(false);
  readonly note = signal('');
  readonly sending = signal(false);
  readonly message = signal('');
  readonly error = signal('');

  /** Retention note of a compacted task (its verbose events were dropped). */
  readonly compactedNote = computed(() => {
    const it = this.item();
    const at = it?.kind === 'task' ? this.fleet.tasksById()[it.id]?.compacted_at : null;
    if (!at) return '';
    const day = new Date(at).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
    return `Detailed events of this session were compacted on ${day}; its summary, milestones, errors and your interventions are kept.`;
  });

  readonly sub = computed(() => {
    const it = this.item();
    if (!it) return '';
    return [it.agent, it.machine, it.projectName, it.status.replace(/_/g, ' ')].filter(Boolean).join(' · ');
  });
  readonly summary = computed(() => {
    const it = this.item();
    if (!it) return '';
    return it.task?.summary?.trim() || it.now || it.goal;
  });
  /** What the work was asked to achieve: the ka ticket's items (from the hub), else the prompt's. */
  readonly acceptanceInfo = signal<Acceptance | null>(null);
  /** What Approve will do (from GET /api/tasks/{id}/delivery). */
  readonly deliveryPlan = signal<DeliveryPlan | null>(null);
  readonly acceptance = computed<AcceptanceItem[]>(() => {
    const it = this.item();
    if (!it) return [];
    return this.acceptanceInfo()?.items ?? (it.goal ? [{ text: it.goal, done: false }] : []);
  });
  readonly acceptanceHeading = computed(() => {
    const a = this.acceptanceInfo();
    if (a?.source === 'ticket') return `Asked for (from ${[a.ticket_id, a.ticket_title].filter(Boolean).join(' ')})`;
    if (a?.source === 'prompt') return 'Asked for (from the prompt)';
    return 'Asked for';
  });
  readonly receipts = computed(() => {
    const it = this.item();
    return it ? receiptsFor(it, this.events()) : [];
  });
  /** What Approve will do for the selected review item. */
  readonly approvePreview = computed(() => {
    const it = this.item();
    if (!it || it.kind !== 'task') return '';
    const plan = this.deliveryPlan();
    if (plan) return approveActionLabel(plan);
    const t = it.task;
    const repo = t?.repo_id ? this.fleet.repos().find((r) => r.id === t.repo_id) : undefined;
    const proj = t?.project_id ? this.fleet.projects().find((p) => p.id === t.project_id) : undefined;
    const settings = this.fleet.settings();
    const hasRemote = !!repo?.remote_url;
    const mode = repo?.delivery_mode || proj?.delivery_mode || settings?.default_delivery_mode || (hasRemote ? 'pr_merge' : 'direct');
    const mergeStyle = repo?.merge_style || proj?.merge_style || settings?.default_merge_style || 'squash';
    return approveActionLabel(null, { mode, mergeStyle, hasRemote, branch: t?.branch, review: this.work.reviewOf(it.id) });
  });
  /** Linked PR/MR state and checks when one exists. */
  readonly prInfo = computed(() => {
    const it = this.item();
    if (!it) return null;
    const r = this.work.reviewOf(it.id);
    const p = this.deliveryPlan();
    const prNumber = r?.pr_number || p?.pr_number || 0;
    if (!prNumber) return null;
    const label = prStatusLine({
      pr_number: prNumber,
      pr_state: r?.pr_state || p?.pr_state,
      pr_checks: r?.pr_checks || p?.pr_checks,
      pr_auto_merge: r?.pr_auto_merge ?? p?.pr_auto_merge,
    });
    return { label, url: r?.pr_url || p?.pr_url || '' };
  });
  readonly compareUrl = computed(() => {
    const it = this.item();
    if (!it) return '';
    const r = this.work.reviewOf(it.id);
    if (r?.pr_url) return '';
    return r?.compare_url || this.deliveryPlan()?.compare_url || '';
  });
  /** The decision already made (on the hub, by anyone) and its delivery outcome. */
  readonly decision = computed(() => {
    const it = this.item();
    const r = it ? this.work.reviewOf(it.id) : undefined;
    if (!r?.decision) return '';
    const who = r.decided_by ? ` by ${r.decided_by}` : '';
    const what = r.decision === 'approved' ? `Approved${who}.` : `Changes requested${who}: ${r.note ?? ''}`;
    return [what, deliveryOutcomeLine(r)].filter(Boolean).join(' ');
  });
  readonly noteLabel = computed(() => {
    const it = this.item();
    if (!it) return '';
    if (it.kind === 'chat') return 'What should change? (sent to the agent as your next message)';
    if (it.column === 'review') return 'What should change? (sent to the agent in a follow-up session)';
    if (it.live) return 'Type into the session (sent as input, with Enter)';
    return 'What should change?';
  });

  constructor() {
    effect(() => {
      this.itemId();
      const it = untracked(() => this.item());
      untracked(() => {
        this.changing.set(false);
        this.note.set('');
        this.message.set('');
        this.error.set('');
        this.events.set([]);
        this.acceptanceInfo.set(null);
        this.deliveryPlan.set(null);
        if (it?.kind === 'task') {
          const id = it.id;
          this.api
            .getTaskEvents(id)
            .then((r) => {
              if (this.item()?.id === id) this.events.set(r.events ?? []);
            })
            .catch(() => undefined);
          this.api
            .getTaskAcceptance(id)
            .then((a) => {
              if (this.item()?.id === id) this.acceptanceInfo.set(a);
            })
            .catch(() => undefined);
          this.api
            .getTaskDelivery(id)
            .then((d) => {
              if (this.item()?.id === id) this.deliveryPlan.set(d);
            })
            .catch(() => undefined);
        }
      });
    });
  }

  closed(): void {
    void this.router.navigate(['/work']);
  }

  async approve(it: WorkItem): Promise<void> {
    this.sending.set(true);
    this.error.set('');
    try {
      const res = await this.work.review(it.id, 'approved');
      const outcome = deliveryOutcomeLine(res.review);
      this.message.set(outcome ? `Approved. ${outcome}` : 'Approved. It moves to Done.');
      this.decided.emit();
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not approve');
    } finally {
      this.sending.set(false);
    }
  }

  async toggleExpedite(it: WorkItem): Promise<void> {
    this.error.set('');
    try {
      await this.work.toggleExpedite(it.id);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not change Expedite');
    }
  }

  openInCommand(it: WorkItem): void {
    void this.router.navigate(['/command', it.sid]);
  }

  async sendChanges(it: WorkItem): Promise<void> {
    const note = this.note().trim();
    if (!note) return;
    this.sending.set(true);
    this.error.set('');
    try {
      if (it.kind === 'chat' && it.chat) {
        if (!this.agentChat.send(it.chat, note)) throw new Error('Not connected to the hub; try again in a moment.');
        this.message.set('Sent to the agent.');
      } else if (it.column === 'review' && it.task) {
        // The hub records the change request and hands the note to the agent.
        const res = await this.work.review(it.id, 'changes_requested', note);
        let sent = 'Changes requested.';
        if (res.delivered === 'steered') sent = 'Sent to the session.';
        else if (res.delivered === 'follow_up') {
          await this.fleet.refresh();
          const sid = res.follow_up_task_sid || this.fleet.tasksById()[res.follow_up_task_id ?? '']?.sid;
          sent = `Started ${sid ?? 'a follow-up session'} with your changes.`;
        }
        this.message.set([sent, deliveryOutcomeLine(res.review)].filter(Boolean).join(' '));
      } else if (it.live && it.task) {
        await this.api.steerTask(it.id, note);
        this.message.set('Sent to the session.');
      }
      this.changing.set(false);
      this.note.set('');
      if (it.column === 'review') this.decided.emit();
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not send');
    } finally {
      this.sending.set(false);
    }
  }
}

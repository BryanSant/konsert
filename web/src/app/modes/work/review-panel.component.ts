import { ChangeDetectionStrategy, Component, computed, effect, inject, input, output, signal, untracked } from '@angular/core';
import { Router } from '@angular/router';
import { TaskEvent } from '../../core/models/konsert.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { AgentChatStore } from '../../core/state/agent-chat.store';
import { FleetStore } from '../../core/state/fleet.store';
import { WorkItem } from '../../core/state/work-logic';
import { WorkStore } from '../../core/state/work.store';
import { SessionControlsComponent } from '../../features/control/session-controls.component';
import { IconComponent } from '../../shared/icon.component';

import { ControlLabelComponent } from '../control-label.component';
import { WallStore } from '../wall/wall.store';
import { acceptanceFrom, changeRequestPrompt, receiptsFor } from './review-logic';

/**
 * Work's side panel for the selected card: summary, receipts against acceptance, and the
 * decision (approve, request changes, veto) for review cards; the controls for live ones.
 */
@Component({
  selector: 'ks-review-panel',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, ControlLabelComponent, SessionControlsComponent],
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
            <h4 id="ks-acc-h" class="m-0 mb-1 text-[11px] text-[var(--ks-text-muted)]">Asked for (from the prompt)</h4>
            <ul class="m-0 p-0 list-none space-y-1">
              @for (a of acceptance(); track $index) {
                <li class="flex gap-2 text-[12.5px]"><span class="text-[var(--ks-text-muted)]" aria-hidden="true">○</span><span class="break-words">{{ a }}</span></li>
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

        @if (it.column === 'review') {
          <div class="flex flex-wrap gap-1.5 pt-1" role="group" aria-label="Decision">
            <button type="button" class="ks-btn ks-btn-sm bg-[var(--ks-green-soft)] text-[#a8dbb3]" data-testid="review-approve" (click)="approve(it)">
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
          <button type="button" class="ks-btn ks-btn-sm" [attr.aria-pressed]="work.isExpedited(it.id)" data-testid="toggle-expedite" (click)="work.toggleExpedite(it.id)">⚑ {{ work.isExpedited(it.id) ? 'Remove from Expedite' : 'Expedite' }}</button>
          <button type="button" class="ks-btn ks-btn-sm" [attr.aria-pressed]="wall.isPinned(it.id)" (click)="wall.togglePin(it.id)"><app-icon name="split" [size]="12" />{{ wall.isPinned(it.id) ? 'Unpin from Wall' : 'Pin to Wall' }}</button>
        </div>
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
  readonly acceptance = computed(() => {
    const it = this.item();
    return it ? acceptanceFrom(it.task?.prompt, it.goal) : [];
  });
  readonly receipts = computed(() => {
    const it = this.item();
    return it ? receiptsFor(it, this.events()) : [];
  });
  readonly noteLabel = computed(() => {
    const it = this.item();
    if (!it) return '';
    if (it.kind === 'chat') return 'What should change? (sent to the agent as your next message)';
    if (it.live) return 'Type into the session (sent as input, with Enter)';
    return 'What should change? (starts a follow-up session in the same checkout)';
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
        if (it?.kind === 'task') {
          const id = it.id;
          this.api
            .getTaskEvents(id)
            .then((r) => {
              if (this.item()?.id === id) this.events.set(r.events ?? []);
            })
            .catch(() => undefined);
        }
      });
    });
  }

  closed(): void {
    void this.router.navigate(['/work']);
  }

  approve(it: WorkItem): void {
    this.work.approve(it.id);
    this.message.set('Approved. It moves to Done today.');
    this.decided.emit();
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
      } else if (it.live && it.task) {
        await this.api.steerTask(it.id, note);
        this.message.set('Sent to the session.');
      } else if (it.task) {
        const t = it.task;
        const res = await this.api.dispatchTask({
          worker_selector: { worker_id: t.worker_id },
          agent: t.agent,
          model: t.model || undefined,
          project_id: t.project_id || undefined,
          repo_id: t.repo_id || undefined,
          worktree_id: t.worktree_id || undefined,
          repo_path: t.effective_dir || t.workdir,
          branch: t.branch || undefined,
          prompt: changeRequestPrompt(it, note),
          mode: 'interactive',
          origin: 'direct',
          permission_mode: t.permission_mode,
        });
        this.work.reject(it.id);
        await this.fleet.refresh();
        const follow = this.fleet.tasksById()[res.task_id];
        this.message.set(`Started ${follow?.sid ?? 'a follow-up session'} with your changes.`);
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

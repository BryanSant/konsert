import { ChangeDetectionStrategy, Component, computed, effect, inject, input, signal, untracked } from '@angular/core';
import { Attempt, RunDigest, Ticket, TicketComment, TicketRepoBinding, TicketStatus } from '../../core/models/syre.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { FleetStore } from '../../core/state/fleet.store';
import { TicketStore } from '../../core/state/ticket.store';
import { IconComponent } from '../../shared/icon.component';
import { PersonComponent } from '../../shared/person.component';
import { ALL_MACHINES, MachineTarget } from '../../shared/machines/machine-target';
import { MachineTargetPickerComponent } from '../../shared/machines/machine-target-picker.component';
import { compactDigest, isContainerKind } from './board-logic';
import { Router } from '@angular/router';

const STATUSES: TicketStatus[] = ['backlog', 'ready', 'running', 'needs_you', 'review', 'done', 'failed', 'canceled'];

function errText(err: unknown): string {
  return err instanceof Error ? err.message : 'Something went wrong';
}

/**
 * Work's side panel for a ticket: the same shape as the review panel, with the ticket's
 * body, acceptance, bindings, blockers, hints and the actions that move it.
 */
@Component({
  selector: 'sy-ticket-panel',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, MachineTargetPickerComponent, PersonComponent],
  host: { class: 'flex flex-col min-h-0' },
  template: `
    <div class="flex-1 min-h-0 overflow-y-auto px-4 py-3.5 flex flex-col gap-3" data-testid="ticket-panel" [attr.data-ticket]="ticket().sid">
      <div class="flex items-center gap-2">
        <span class="font-mono-ide text-[11px] text-[var(--sy-text-muted)]">{{ ticket().sid }}</span>
        <span class="text-[10.5px] font-semibold text-[var(--sy-text-secondary)]">{{ ticket().priority }}</span>
        <span class="text-[11px] text-[var(--sy-text-muted)]">{{ ticket().status.replace('_', ' ') }}</span>
        <span class="flex-1"></span>
        <button type="button" class="sy-icon-btn sy-icon-btn-sm" aria-label="Close panel" (click)="close()">
          <app-icon name="close" [size]="12" />
        </button>
      </div>
      <h2 class="m-0 text-[15px] font-semibold leading-snug text-[var(--sy-text)]">{{ ticket().title }}</h2>

      <section>
        <h3 class="m-0 mb-1 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--sy-text-secondary)]">Body</h3>
        <div class="rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-2)] px-3 py-2.5 text-[12.5px] leading-relaxed whitespace-pre-wrap break-words" data-testid="ticket-body">{{ ticket().body || 'No description yet.' }}</div>
      </section>

      <section>
        <h3 class="m-0 mb-1 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--sy-text-secondary)]">
          Acceptance
          @if (ticket().acceptance.length) {
            <span class="font-normal normal-case tracking-normal text-[var(--sy-text-muted)]">{{ doneCount() }}/{{ ticket().acceptance.length }}</span>
          }
        </h3>
        @if (ticket().acceptance.length === 0) {
          <p class="m-0 text-[12px] text-[var(--sy-text-muted)]">No acceptance items.</p>
        } @else {
          <ul class="m-0 p-0 list-none space-y-1" data-testid="ticket-acceptance">
            @for (item of ticket().acceptance; track $index) {
              <li>
                <label class="flex gap-2 text-[12.5px] items-start">
                  <input type="checkbox" class="mt-0.5" [checked]="item.done" [disabled]="busy()" [attr.data-acceptance]=" $index " (change)="toggleAcceptance($index, $any($event.target).checked)" />
                  <span class="break-words" [class.line-through]="item.done" [class.text-[var(--sy-text-muted)]]="item.done">{{ item.text }}</span>
                </label>
              </li>
            }
          </ul>
        }
      </section>

      @if (!container()) {
        <section>
          <h3 class="m-0 mb-1 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--sy-text-secondary)]">Repos</h3>
          <ul class="m-0 p-0 list-none space-y-1" data-testid="ticket-repos">
            @for (b of repos(); track b.repo_id) {
              <li class="flex items-center gap-2 text-[12.5px]">
                <span class="flex-1 truncate">{{ repoName(b.repo_id) }}</span>
                <button type="button" class="sy-btn sy-btn-sm" [attr.data-remove-repo]="b.repo_id" [disabled]="busy()" (click)="removeRepo(b.repo_id)">Remove</button>
              </li>
            }
          </ul>
          <div class="mt-1.5 flex flex-wrap gap-1.5">
            <select class="sy-field sy-field-sm flex-1 min-w-[8rem]" data-testid="add-repo" [value]="repoPick()" (change)="repoPick.set($any($event.target).value)">
              <option value="">Add a repo…</option>
              @for (r of unboundRepos(); track r.id) {
                <option [value]="r.id">{{ r.name }}</option>
              }
            </select>
            <button type="button" class="sy-btn sy-btn-sm" data-testid="add-repo-btn" [disabled]="!repoPick() || busy()" (click)="addRepo()">Add</button>
          </div>
        </section>

        <section>
          <h3 class="m-0 mb-1 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--sy-text-secondary)]">Blocked by</h3>
          <ul class="m-0 p-0 list-none flex flex-wrap gap-1.5" data-testid="ticket-blockers">
            @for (b of blockerEdges(); track b.blocked_by_id) {
              <li class="inline-flex items-center gap-1 rounded-full bg-[var(--sy-surface-2)] px-2 py-0.5 text-[11px]">
                <span>blocked by {{ blockerSid(b.blocked_by_id) }}</span>
                <button type="button" class="sy-icon-btn sy-icon-btn-sm" [attr.aria-label]="'Remove blocker ' + blockerSid(b.blocked_by_id)" [attr.data-remove-blocker]="b.blocked_by_id" [disabled]="busy()" (click)="removeBlocker(b.blocked_by_id)">
                  <app-icon name="close" [size]="10" />
                </button>
              </li>
            }
          </ul>
          <div class="mt-1.5 flex flex-wrap gap-1.5">
            <select class="sy-field sy-field-sm flex-1 min-w-[8rem]" data-testid="add-blocker" [value]="blockerPick()" (change)="blockerPick.set($any($event.target).value)">
              <option value="">Add a blocker…</option>
              @for (t of blockerChoices(); track t.id) {
                <option [value]="t.id">{{ t.sid }} {{ t.title }}</option>
              }
            </select>
            <button type="button" class="sy-btn sy-btn-sm" data-testid="add-blocker-btn" [disabled]="!blockerPick() || busy()" (click)="addBlocker()">Add</button>
          </div>
          @if (blockerError()) {
            <p class="m-0 mt-1 text-[12px] text-[#f3a5a5]" role="alert" data-testid="blocker-error">{{ blockerError() }}</p>
          }
        </section>
      }

      <section data-testid="ticket-run-on">
        <h3 class="m-0 mb-1 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--sy-text-secondary)]">Run on</h3>
        <sy-machine-target-picker mode="scope" hint="Where this ticket's work may run" [value]="runOn()" (valueChange)="setRunOn($event)" />
        @if (runOnError()) {
          <p class="m-0 mt-1 text-[12px] text-[#f3a5a5]" role="alert">{{ runOnError() }}</p>
        }
      </section>

      <section>
        <h3 class="m-0 mb-1 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--sy-text-secondary)]">Hints</h3>
        <div class="grid grid-cols-3 gap-1.5" data-testid="ticket-hints">
          <label class="block text-[11px] text-[var(--sy-text-muted)]">Agent
            <input class="sy-field sy-field-sm w-full mt-0.5" data-testid="hint-agent" [value]="hintAgent()" (input)="hintAgent.set($any($event.target).value)" />
          </label>
          <label class="block text-[11px] text-[var(--sy-text-muted)]">Model
            <input class="sy-field sy-field-sm w-full mt-0.5" data-testid="hint-model" [value]="hintModel()" (input)="hintModel.set($any($event.target).value)" />
          </label>
          <label class="block text-[11px] text-[var(--sy-text-muted)]">Machine
            <input class="sy-field sy-field-sm w-full mt-0.5" data-testid="hint-machine" [value]="hintMachine()" (input)="hintMachine.set($any($event.target).value)" />
          </label>
        </div>
        <button type="button" class="sy-btn sy-btn-sm mt-1.5" data-testid="save-hints" [disabled]="busy()" (click)="saveHints()">Save hints</button>
      </section>

      <section>
        <h3 class="m-0 mb-1 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--sy-text-secondary)]">Verify</h3>
        <div class="flex gap-1.5">
          <input class="sy-field sy-field-sm flex-1" data-testid="ticket-verify" [value]="verify()" (input)="verify.set($any($event.target).value)" placeholder="Command (empty uses each repo's)" />
          <button type="button" class="sy-btn sy-btn-sm" data-testid="save-verify" [disabled]="busy()" (click)="saveVerify()">Save</button>
        </div>
      </section>

      <section>
        <h3 class="m-0 mb-1 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--sy-text-secondary)]">Comments</h3>
        <ul class="m-0 p-0 list-none space-y-1.5" data-testid="ticket-comments">
          @for (c of comments(); track c.id) {
            <li class="text-[12.5px]">
              <span class="text-[11px] text-[var(--sy-text-muted)]"><sy-person [login]="c.author" /></span>
              <span class="block whitespace-pre-wrap break-words">{{ c.body }}</span>
            </li>
          }
        </ul>
        <form class="mt-1.5 flex gap-1.5" (submit)="$event.preventDefault(); postComment()">
          <input class="sy-field sy-field-sm flex-1" data-testid="comment-input" maxlength="500" placeholder="Note" [value]="comment()" (input)="comment.set($any($event.target).value)" />
          <button type="submit" class="sy-btn sy-btn-sm" data-testid="comment-send" [disabled]="!comment().trim() || busy()">Post</button>
        </form>
      </section>

      <section>
        <h3 class="m-0 mb-1 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--sy-text-secondary)]">Latest digest</h3>
        <div class="rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-2)] px-3 py-2 text-[12px] whitespace-pre-wrap break-words text-[var(--sy-text-secondary)]" data-testid="ticket-digest">{{ digestText() || 'No digest yet.' }}</div>
        <div class="mt-1.5 flex flex-wrap items-center gap-1.5">
          @if (repos().length > 1) {
            <select class="sy-field sy-field-sm" data-testid="diff-repo" aria-label="Diff repo" [value]="diffRepo()" (change)="pickDiffRepo($any($event.target).value)">
              @for (b of repos(); track b.repo_id) {
                <option [value]="repoName(b.repo_id)">{{ repoName(b.repo_id) }}</option>
              }
            </select>
          }
          <button type="button" class="sy-btn sy-btn-sm" data-testid="view-diff" [disabled]="diffBusy()" (click)="viewDiff(0)">View diff</button>
          @if (diffTruncated()) {
            <button type="button" class="sy-btn sy-btn-sm" data-testid="diff-more" [disabled]="diffBusy()" (click)="viewDiff(diffNext())">More</button>
          }
        </div>
        @if (diffOpen()) {
          <pre class="m-0 mt-1.5 max-h-60 overflow-auto rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-2)] px-3 py-2 text-[11px] leading-relaxed whitespace-pre-wrap break-words" data-testid="ticket-diff">{{ diffText() }}</pre>
          @if (diffNote()) {
            <p class="m-0 mt-1 text-[11px] text-[var(--sy-text-muted)]" data-testid="diff-note">{{ diffNote() }}</p>
          }
        }
      </section>

      <section data-testid="ticket-attempts">
        <h3 class="m-0 mb-1 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--sy-text-secondary)]">Attempts</h3>
        @if (attempts().length === 0) {
          <p class="m-0 text-[12px] text-[var(--sy-text-muted)]">No attempts yet.</p>
        } @else {
          <div class="overflow-x-auto">
            <table class="w-full text-left text-[12px]">
              <thead>
                <tr class="text-[10.5px] uppercase tracking-wider text-[var(--sy-text-muted)]">
                  <th class="py-1 pr-2 font-semibold">#</th>
                  <th class="py-1 pr-2 font-semibold">Machine</th>
                  <th class="py-1 pr-2 font-semibold">Agent</th>
                  <th class="py-1 pr-2 font-semibold">Duration</th>
                  <th class="py-1 pr-2 font-semibold">Status</th>
                  <th class="py-1 font-semibold">Verify</th>
                </tr>
              </thead>
              <tbody>
                @for (a of attempts(); track a.id) {
                  <tr>
                    <td class="py-0.5 pr-2 font-mono-ide">{{ a.attempt }}</td>
                    <td class="py-0.5 pr-2">{{ a.machine || a.worker_id || '—' }}</td>
                    <td class="py-0.5 pr-2">{{ attemptAgent(a) }}</td>
                    <td class="py-0.5 pr-2">{{ a.duration_s != null ? a.duration_s + 's' : '—' }}</td>
                    <td class="py-0.5 pr-2">{{ attemptStatus(a) }}</td>
                    <td class="py-0.5">{{ attemptVerify(a) }}</td>
                  </tr>
                }
              </tbody>
            </table>
          </div>
        }
      </section>

      <section>
        <h3 class="m-0 mb-1 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--sy-text-secondary)]">Packet</h3>
        <pre class="m-0 max-h-40 overflow-auto rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-2)] px-3 py-2 text-[11px] leading-relaxed whitespace-pre-wrap break-words" data-testid="ticket-packet">{{ packetText() || packetNote() }}</pre>
      </section>

      @if (error()) {
        <p class="m-0 text-[12px] text-[#f3a5a5]" role="alert" data-testid="ticket-error">{{ error() }}</p>
      }

      <div class="flex flex-wrap items-center gap-1.5 pt-2 border-t border-[var(--sy-hairline)]">
        <select class="sy-field sy-field-sm" data-testid="ticket-status" aria-label="Move to status" [value]="statusPick()" (change)="statusPick.set($any($event.target).value)">
          @for (s of statuses; track s) {
            <option [value]="s">{{ s.replace('_', ' ') }}</option>
          }
        </select>
        <button type="button" class="sy-btn sy-btn-sm sy-btn-primary" data-testid="ticket-move" [disabled]="busy() || statusPick() === ticket().status" (click)="move()">Move</button>
        <button type="button" class="sy-btn sy-btn-sm" data-testid="ticket-requeue" [disabled]="busy()" (click)="requeue()">Requeue</button>
        <button type="button" class="sy-btn sy-btn-sm" data-testid="ticket-cancel" [disabled]="busy()" (click)="cancelTicket()">Cancel</button>
      </div>
    </div>
  `,
})
export class TicketPanelComponent {
  readonly ticket = input.required<Ticket>();
  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);
  private readonly tickets = inject(TicketStore);
  private readonly router = inject(Router);

  readonly statuses = STATUSES;
  readonly busy = signal(false);
  readonly error = signal('');
  readonly blockerError = signal('');
  readonly comments = signal<TicketComment[]>([]);
  readonly attempts = signal<Attempt[]>([]);
  readonly repos = signal<TicketRepoBinding[]>([]);
  readonly digest = signal<RunDigest | null>(null);
  readonly diffText = signal('');
  readonly diffNote = signal('');
  readonly diffRepo = signal('');
  readonly diffOpen = signal(false);
  readonly diffTruncated = signal(false);
  readonly diffNext = signal(0);
  readonly diffBusy = signal(false);
  readonly packetText = signal('');
  readonly packetNote = signal('No packet yet.');
  readonly statusPick = signal<TicketStatus>('ready');
  readonly verify = signal('');
  readonly hintAgent = signal('');
  readonly hintModel = signal('');
  readonly hintMachine = signal('');
  readonly comment = signal('');
  readonly repoPick = signal('');
  readonly blockerPick = signal('');
  /** Drafts reload only when a different ticket is opened, so a live upsert does not wipe an edit. */
  private loadedFor = '';

  readonly container = computed(() => isContainerKind(this.ticket().kind));
  readonly doneCount = computed(() => this.ticket().acceptance.filter((a) => a.done).length);
  readonly digestText = computed(() => compactDigest(this.digest()));
  readonly blockerEdges = computed(() => this.tickets.edges().filter((e) => e.ticket_id === this.ticket().id));
  readonly blockerChoices = computed(() => {
    const id = this.ticket().id;
    const taken = new Set(this.blockerEdges().map((e) => e.blocked_by_id));
    return this.tickets.tickets().filter((t) => t.id !== id && t.project_id === this.ticket().project_id && !isContainerKind(t.kind) && !taken.has(t.id));
  });
  readonly unboundRepos = computed(() => {
    const bound = new Set(this.repos().map((b) => b.repo_id));
    return this.fleet.repos().filter((r) => r.project_id === this.ticket().project_id && !bound.has(r.id));
  });

  /** Where this ticket's work may run (SYR-0M4QQ). */
  readonly runOn = signal<MachineTarget>(ALL_MACHINES);
  readonly runOnError = signal('');

  private async loadRunOn(id: string): Promise<void> {
    try {
      const v = await this.api.getTicketScope(id);
      if (this.ticket().id === id) this.runOn.set(v.scope);
    } catch {
      // Keep All machines; the hub is the source of truth on the next open.
    }
  }

  async setRunOn(t: MachineTarget): Promise<void> {
    const id = this.ticket().id;
    this.runOnError.set('');
    try {
      const v = await this.api.setTicketScope(id, t);
      if (this.ticket().id === id) this.runOn.set(v.scope);
    } catch (err) {
      this.runOnError.set(errText(err));
    }
  }

  constructor() {
    effect(() => {
      const id = this.ticket().id;
      untracked(() => {
        if (this.loadedFor === id) return;
        this.loadedFor = id;
        const t = this.ticket();
        this.statusPick.set(t.status);
        this.verify.set(t.verify_cmd ?? '');
        this.hintAgent.set(t.hints?.agent ?? '');
        this.hintModel.set(t.hints?.model ?? '');
        this.hintMachine.set(t.hints?.machine ?? '');
        this.runOn.set(ALL_MACHINES);
        this.runOnError.set('');
        void this.loadRunOn(id);
        this.error.set('');
        this.blockerError.set('');
        this.diffText.set('');
        this.diffNote.set('');
        this.diffRepo.set('');
        this.diffOpen.set(false);
        this.diffTruncated.set(false);
        this.diffNext.set(0);
        this.attempts.set([]);
        void this.reload(id);
      });
    });
  }

  attemptAgent(a: Attempt): string {
    const parts = [a.agent, a.model].filter((p) => !!p);
    return parts.join('/') || '—';
  }

  attemptStatus(a: Attempt): string {
    if (!a.status) return '—';
    return a.failure_category ? `${a.status}/${a.failure_category}` : a.status;
  }

  attemptVerify(a: Attempt): string {
    if (a.verify_exit == null && !a.verify_cmd) return '—';
    if (a.verify_exit === 0) return 'pass';
    const n = a.verify_failed ?? 0;
    return n > 0 ? `fail ${n}` : 'fail';
  }

  repoName(id: string): string {
    return this.fleet.repos().find((r) => r.id === id)?.name || id;
  }

  blockerSid(id: string): string {
    return this.tickets.tickets().find((t) => t.id === id)?.sid || id;
  }

  close(): void {
    void this.router.navigate(['/work']);
  }

  async toggleAcceptance(index: number, done: boolean): Promise<void> {
    const acceptance = this.ticket().acceptance.map((item, i) => (i === index ? { ...item, done } : item));
    await this.save({ acceptance });
  }

  async saveHints(): Promise<void> {
    const hints = {
      ...(this.ticket().hints ?? {}),
      agent: this.hintAgent().trim(),
      model: this.hintModel().trim(),
      machine: this.hintMachine().trim(),
    };
    await this.save({ hints });
  }

  async saveVerify(): Promise<void> {
    await this.save({ verify_cmd: this.verify().trim() });
  }

  async move(): Promise<void> {
    await this.save({ status: this.statusPick() });
  }

  async requeue(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      const ticket = await this.tickets.requeue(this.ticket().id);
      this.statusPick.set(ticket.status);
    } catch (err) {
      this.error.set(errText(err));
    } finally {
      this.busy.set(false);
    }
  }

  async cancelTicket(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      const ticket = await this.tickets.cancel(this.ticket().id);
      this.statusPick.set(ticket.status);
    } catch (err) {
      this.error.set(errText(err));
    } finally {
      this.busy.set(false);
    }
  }

  async addRepo(): Promise<void> {
    const repoId = this.repoPick();
    if (!repoId) return;
    this.busy.set(true);
    this.error.set('');
    try {
      const binding = await this.api.putTicketRepo(this.ticket().id, repoId);
      this.repos.update((list) => [...list.filter((b) => b.repo_id !== repoId), binding]);
      this.repoPick.set('');
    } catch (err) {
      this.error.set(errText(err));
    } finally {
      this.busy.set(false);
    }
  }

  async removeRepo(repoId: string): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.removeTicketRepo(this.ticket().id, repoId);
      this.repos.update((list) => list.filter((b) => b.repo_id !== repoId));
    } catch (err) {
      this.error.set(errText(err));
    } finally {
      this.busy.set(false);
    }
  }

  async addBlocker(): Promise<void> {
    const blockerId = this.blockerPick();
    if (!blockerId) return;
    this.busy.set(true);
    this.blockerError.set('');
    try {
      await this.tickets.addEdge(this.ticket().id, blockerId);
      this.blockerPick.set('');
    } catch (err) {
      this.blockerError.set(errText(err));
    } finally {
      this.busy.set(false);
    }
  }

  async removeBlocker(blockerId: string): Promise<void> {
    this.busy.set(true);
    this.blockerError.set('');
    try {
      await this.tickets.removeEdge(this.ticket().id, blockerId);
    } catch (err) {
      this.blockerError.set(errText(err));
    } finally {
      this.busy.set(false);
    }
  }

  pickDiffRepo(name: string): void {
    this.diffRepo.set(name);
    this.diffText.set('');
    this.diffNote.set('');
    this.diffOpen.set(false);
    this.diffTruncated.set(false);
    this.diffNext.set(0);
  }

  async viewDiff(offset: number): Promise<void> {
    this.diffBusy.set(true);
    this.error.set('');
    try {
      const bindings = this.repos();
      const repo = bindings.length > 1 ? this.diffRepo() || this.repoName(bindings[0].repo_id) : undefined;
      const res = await this.api.inspectTicket(this.ticket().id, { kind: 'diff', repo, max_lines: 200, offset });
      const text = res.text ?? '';
      this.diffText.set(offset > 0 ? this.diffText() + text : text);
      this.diffTruncated.set(!!res.truncated);
      this.diffNext.set(res.next_offset ?? 0);
      this.diffNote.set(res.note ?? '');
      this.diffOpen.set(true);
    } catch (err) {
      this.error.set(errText(err));
    } finally {
      this.diffBusy.set(false);
    }
  }

  async postComment(): Promise<void> {
    const body = this.comment().trim();
    if (!body) return;
    this.busy.set(true);
    this.error.set('');
    try {
      const created = await this.tickets.comment(this.ticket().id, body);
      this.comments.update((list) => [...list, created]);
      this.comment.set('');
    } catch (err) {
      this.error.set(errText(err));
    } finally {
      this.busy.set(false);
    }
  }

  private async save(patch: Partial<Ticket>): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      const updated = await this.tickets.update(this.ticket().id, patch);
      this.statusPick.set(updated.status);
    } catch (err) {
      this.error.set(errText(err));
    } finally {
      this.busy.set(false);
    }
  }

  private async reload(id: string): Promise<void> {
    try {
      const [detail, comments, packet, attempts] = await Promise.all([
        this.api.getTicket(id),
        this.api.listTicketComments(id).catch(() => ({ comments: [] as TicketComment[] })),
        this.api.getTicketPacket(id).catch((err: unknown) => ({ text: '', note: errText(err) })),
        this.api.listTicketAttempts(id).catch(() => ({ attempts: [] as Attempt[] })),
      ]);
      if (this.ticket().id !== id) return;
      // A save that landed while this request was in flight is newer than the snapshot.
      const local = this.tickets.tickets().find((t) => t.id === detail.ticket.id);
      if (!local || local.updated_at <= detail.ticket.updated_at) this.tickets.upsert(detail.ticket);
      for (const edge of detail.blocked_by ?? []) this.tickets.upsertEdge(edge);
      this.repos.set(detail.repo_bindings ?? []);
      if (!this.diffRepo() && this.repos().length > 0) this.diffRepo.set(this.repoName(this.repos()[0].repo_id));
      this.digest.set(detail.digest ?? null);
      this.comments.set(comments.comments ?? []);
      this.attempts.set(attempts.attempts ?? []);
      this.packetText.set(packet.text ?? '');
      this.packetNote.set('note' in packet && packet.note ? packet.note : 'No packet yet.');
    } catch (err) {
      if (this.ticket().id === id) this.error.set(errText(err));
    }
  }
}

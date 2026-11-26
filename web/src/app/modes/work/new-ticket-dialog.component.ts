import { ChangeDetectionStrategy, Component, computed, effect, inject, output, signal, untracked } from '@angular/core';
import { Ticket, TicketKind, TicketPriority } from '../../core/models/syre.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { FleetStore } from '../../core/state/fleet.store';
import { TicketStore } from '../../core/state/ticket.store';
import { ModalDialogComponent } from '../../shared/modal-dialog.component';
import { isContainerKind } from './board-logic';

const KINDS: TicketKind[] = ['feature', 'bug', 'chore', 'spike', 'epic', 'initiative'];
const PRIORITIES: TicketPriority[] = ['P0', 'P1', 'P2', 'P3'];

function errText(err: unknown): string {
  return err instanceof Error ? err.message : 'Could not create the ticket';
}

/**
 * Creates a ticket (or an initiative / epic container) on the hub, then its repo
 * bindings and blocked_by edges.
 */
@Component({
  selector: 'sy-new-ticket-dialog',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [ModalDialogComponent],
  template: `
    <app-modal-dialog labelledBy="sy-new-ticket-title" panelClass="w-full max-w-lg max-h-[88vh] overflow-y-auto rounded-[var(--sy-radius-xl)] bg-[var(--sy-surface-2)] p-4" align="top" (close)="closed.emit()">
      <h2 id="sy-new-ticket-title" class="m-0 text-[15px] font-semibold">New ticket</h2>
      <form class="mt-3 flex flex-col gap-2.5 text-[12.5px]" data-testid="new-ticket-dialog" (submit)="submit($event)">
        <label class="block">Project
          <select class="sy-field sy-field-sm w-full mt-0.5" data-testid="nt-project" [value]="projectId()" (change)="projectId.set($any($event.target).value)">
            @for (p of fleet.projects(); track p.id) {
              <option [value]="p.id">{{ p.name }}</option>
            }
          </select>
        </label>
        <div class="grid grid-cols-2 gap-2">
          <label class="block">Kind
            <select class="sy-field sy-field-sm w-full mt-0.5" data-testid="nt-kind" [value]="kind()" (change)="setKind($any($event.target).value)">
              @for (k of kinds; track k) {
                <option [value]="k">{{ k }}</option>
              }
            </select>
          </label>
          <label class="block">Priority
            <select class="sy-field sy-field-sm w-full mt-0.5" data-testid="nt-priority" [value]="priority()" (change)="priority.set($any($event.target).value)">
              @for (p of priorities; track p) {
                <option [value]="p">{{ p }}</option>
              }
            </select>
          </label>
        </div>
        @if (kind() !== 'initiative') {
          <label class="block">Parent
            <select class="sy-field sy-field-sm w-full mt-0.5" data-testid="nt-parent" [value]="parentId()" (change)="parentId.set($any($event.target).value)">
              <option value="">None</option>
              @for (t of parents(); track t.id) {
                <option [value]="t.id">{{ t.sid }} {{ t.title }}</option>
              }
            </select>
          </label>
        }
        <label class="block">Title
          <input class="sy-field sy-field-sm w-full mt-0.5" data-testid="nt-title" required [value]="title()" (input)="title.set($any($event.target).value)" />
        </label>
        <label class="block">Body
          <textarea class="sy-field w-full mt-0.5 resize-y text-[12.5px]" rows="3" data-testid="nt-body" [value]="body()" (input)="body.set($any($event.target).value)"></textarea>
        </label>
        <label class="block">Acceptance
          <textarea class="sy-field w-full mt-0.5 resize-y text-[12.5px]" rows="3" data-testid="nt-acceptance" placeholder="One line per item" [value]="acceptance()" (input)="acceptance.set($any($event.target).value)"></textarea>
        </label>
        @if (!container()) {
          <fieldset class="border-0 p-0 m-0">
            <legend class="text-[12px] text-[var(--sy-text-secondary)] mb-1">Repos</legend>
            <div class="max-h-28 overflow-auto flex flex-col gap-1" data-testid="nt-repos">
              @for (r of repos(); track r.id) {
                <label class="flex items-center gap-2">
                  <input type="checkbox" [checked]="repoIds().includes(r.id)" (change)="toggle(repoIds, r.id, $any($event.target).checked)" />
                  {{ r.name }}
                </label>
              }
              @if (repos().length === 0) {
                <span class="text-[var(--sy-text-muted)]">This project has no repos yet.</span>
              }
            </div>
          </fieldset>
          <fieldset class="border-0 p-0 m-0">
            <legend class="text-[12px] text-[var(--sy-text-secondary)] mb-1">Blocked by</legend>
            <div class="max-h-28 overflow-auto flex flex-col gap-1" data-testid="nt-blockers">
              @for (t of blockerChoices(); track t.id) {
                <label class="flex items-center gap-2">
                  <input type="checkbox" [checked]="blockerIds().includes(t.id)" (change)="toggle(blockerIds, t.id, $any($event.target).checked)" />
                  {{ t.sid }} {{ t.title }}
                </label>
              }
              @if (blockerChoices().length === 0) {
                <span class="text-[var(--sy-text-muted)]">No other tickets in this project.</span>
              }
            </div>
          </fieldset>
        }
        <div class="grid grid-cols-3 gap-2" data-testid="nt-hints">
          <label class="block text-[11px] text-[var(--sy-text-muted)]">Agent
            <input class="sy-field sy-field-sm w-full mt-0.5" data-testid="nt-hint-agent" [value]="hintAgent()" (input)="hintAgent.set($any($event.target).value)" />
          </label>
          <label class="block text-[11px] text-[var(--sy-text-muted)]">Model
            <input class="sy-field sy-field-sm w-full mt-0.5" data-testid="nt-hint-model" [value]="hintModel()" (input)="hintModel.set($any($event.target).value)" />
          </label>
          <label class="block text-[11px] text-[var(--sy-text-muted)]">Machine
            <input class="sy-field sy-field-sm w-full mt-0.5" data-testid="nt-hint-machine" [value]="hintMachine()" (input)="hintMachine.set($any($event.target).value)" />
          </label>
        </div>
        @if (error()) {
          <p class="m-0 text-[12px] text-[#f3a5a5]" role="alert" data-testid="new-ticket-error">{{ error() }}</p>
        }
        <div class="flex justify-end gap-1.5 pt-1">
          <button type="button" class="sy-btn sy-btn-sm" (click)="closed.emit()">Cancel</button>
          <button type="submit" class="sy-btn sy-btn-sm sy-btn-primary" data-testid="nt-submit" [disabled]="busy() || !title().trim()">Create</button>
        </div>
      </form>
    </app-modal-dialog>
  `,
})
export class NewTicketDialogComponent {
  readonly closed = output<void>();
  readonly fleet = inject(FleetStore);
  private readonly api = inject(ApiClientService);
  private readonly tickets = inject(TicketStore);

  readonly kinds = KINDS;
  readonly priorities = PRIORITIES;
  readonly projectId = signal('');
  readonly kind = signal<TicketKind>('feature');
  readonly priority = signal<TicketPriority>('P2');
  readonly parentId = signal('');
  readonly title = signal('');
  readonly body = signal('');
  readonly acceptance = signal('');
  readonly repoIds = signal<string[]>([]);
  readonly blockerIds = signal<string[]>([]);
  readonly hintAgent = signal('');
  readonly hintModel = signal('');
  readonly hintMachine = signal('');
  readonly busy = signal(false);
  readonly error = signal('');

  readonly container = computed(() => isContainerKind(this.kind()));
  readonly parents = computed(() => {
    const project = this.projectId();
    const kind = this.kind();
    return this.tickets.tickets().filter((t) => {
      if (t.project_id !== project) return false;
      if (kind === 'epic') return t.kind === 'initiative';
      return t.kind === 'initiative' || t.kind === 'epic';
    });
  });
  readonly repos = computed(() => this.fleet.repos().filter((r) => r.project_id === this.projectId()));
  readonly blockerChoices = computed(() => this.tickets.tickets().filter((t) => t.project_id === this.projectId() && !isContainerKind(t.kind)));

  constructor() {
    effect(() => {
      const projects = this.fleet.projects();
      untracked(() => {
        if (this.projectId() || projects.length === 0) return;
        const preferred = projects.find((p) => p.is_default) ?? projects[0];
        this.projectId.set(preferred.id);
      });
    });
  }

  setKind(kind: TicketKind): void {
    this.kind.set(kind);
    if (kind === 'initiative') this.parentId.set('');
    if (!this.parents().some((t) => t.id === this.parentId())) this.parentId.set('');
  }

  toggle(list: ReturnType<typeof signal<string[]>>, id: string, on: boolean): void {
    list.update((cur) => (on ? [...cur, id] : cur.filter((x) => x !== id)));
  }

  async submit(ev: Event): Promise<void> {
    ev.preventDefault();
    const title = this.title().trim();
    if (!title || this.busy()) return;
    this.busy.set(true);
    this.error.set('');
    const acceptance = this.acceptance()
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean)
      .map((text) => ({ text, done: false }));
    const hints: Ticket['hints'] = {};
    if (this.hintAgent().trim()) hints.agent = this.hintAgent().trim();
    if (this.hintModel().trim()) hints.model = this.hintModel().trim();
    if (this.hintMachine().trim()) hints.machine = this.hintMachine().trim();
    // A ready ticket with no repo is a decomposition pulse. When the user picked
    // repos, create it in the backlog, bind those repos, then move it to ready.
    const repoIds = this.container() ? [] : this.repoIds();
    const promote = repoIds.length > 0;
    try {
      const ticket = await this.api.createTicket({
        project_id: this.projectId(),
        parent_id: this.kind() === 'initiative' ? '' : this.parentId(),
        kind: this.kind(),
        title,
        body: this.body().trim(),
        acceptance,
        priority: this.priority(),
        status: this.container() || promote ? 'backlog' : 'ready',
        hints,
      });
      this.tickets.upsert(ticket);
      try {
        for (const repoId of repoIds) {
          await this.api.putTicketRepo(ticket.id, repoId);
        }
        for (const blockerId of this.blockerIds()) {
          const edge = await this.api.addTicketEdge(ticket.id, blockerId);
          this.tickets.upsertEdge(edge);
        }
        if (promote) {
          const ready = await this.api.updateTicket(ticket.id, { status: 'ready' });
          this.tickets.upsert(ready);
        }
      } catch (err) {
        this.error.set(`${ticket.sid} was created, but a link failed: ${errText(err)}`);
        this.busy.set(false);
        return;
      }
      this.closed.emit();
    } catch (err) {
      this.error.set(errText(err));
      this.busy.set(false);
    }
  }
}

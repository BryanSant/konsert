import { ChangeDetectionStrategy, Component, OnInit, computed, effect, inject, input, output, signal, untracked, viewChild } from '@angular/core';
import { AgentAccount } from '../core/models/agent-chat.models';
import { PermissionMode, Repo, SessionKind, Space, Worker, Worktree } from '../core/models/syre.models';
import { ApiClientService } from '../core/services/api-client.service';
import { AgentAccountsStore } from '../core/state/agent-accounts.store';
import { FleetStore } from '../core/state/fleet.store';
import { ChatComposerComponent, ComposerSubmit } from '../features/chat/composer/chat-composer.component';
import { ComposerSelection, emptySelection, findModel, normalizeSelection, pickDefaultAccount } from '../features/chat/composer/composer-logic';
import { ContextBarComponent, ContextChange } from '../features/chat/composer/context-bar.component';
import { ReloginService } from '../features/chat/relogin.service';
import { IconComponent } from '../shared/icon.component';
import { ModalDialogComponent } from '../shared/modal-dialog.component';
import { TooltipDirective } from '../shared/tooltip.directive';

/** What the session modal was opened for. */
export interface SessionDispatchRequest {
  kind: SessionKind;
  worker: Worker | null;
  space?: Space | null;
  repo?: Repo | null;
  worktree?: Worktree | null;
}

/**
 * New session dialog: an agent PTY session or a shell terminal. Agent sessions reuse the
 * chat composer (account chip, model·effort, approval mode, machine · folder context bar),
 * so they offer the same real models, efforts and approval modes as a chat.
 */
@Component({
  selector: 'app-session-dispatch-modal',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective, ModalDialogComponent, ChatComposerComponent, ContextBarComponent],
  template: `
    <app-modal-dialog
      labelledBy="session-dispatch-title"
      panelClass="w-full max-w-2xl rounded-[var(--sy-radius-xl)] bg-[var(--sy-surface-2)] p-6 space-y-5"
      (close)="close.emit()"
    >
      <div class="flex items-center gap-3">
        <h2 id="session-dispatch-title" class="text-[15px] font-semibold text-[var(--sy-text)]">New session</h2>
        <div class="inline-flex rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-0)] p-0.5 text-[12px]" role="group" aria-label="Session type">
          <button
            type="button"
            data-testid="dispatch-kind-chat"
            (click)="sessionKind.set('chat')"
            [attr.aria-pressed]="sessionKind() === 'chat'"
            class="rounded-[var(--sy-radius-sm)] px-3 py-1.5 font-medium transition cursor-pointer"
            [class.bg-[var(--sy-surface-3)]]="sessionKind() === 'chat'"
            [class.text-[var(--sy-text)]]="sessionKind() === 'chat'"
            [class.text-[var(--sy-text-muted)]]="sessionKind() !== 'chat'"
          >
            Agent session
          </button>
          <button
            type="button"
            data-testid="dispatch-kind-terminal"
            (click)="sessionKind.set('terminal')"
            [attr.aria-pressed]="sessionKind() === 'terminal'"
            class="rounded-[var(--sy-radius-sm)] px-3 py-1.5 font-medium transition cursor-pointer"
            [class.bg-[var(--sy-surface-3)]]="sessionKind() === 'terminal'"
            [class.text-[var(--sy-text)]]="sessionKind() === 'terminal'"
            [class.text-[var(--sy-text-muted)]]="sessionKind() !== 'terminal'"
          >
            Terminal
          </button>
        </div>
        <span class="flex-1"></span>
        <button type="button" class="sy-icon-btn" aria-label="Close dialog" syTooltip="Close" (click)="close.emit()">
          <app-icon name="close" [size]="15" />
        </button>
      </div>

      @if (sessionKind() === 'chat') {
        @if (accounts().length > 0) {
          <sy-chat-composer
            class="block"
            data-testid="dispatch-composer"
            [accounts]="accounts()"
            [selection]="selection()"
            (selectionChange)="onSelection($event)"
            [allowEmpty]="true"
            [allowAttachments]="false"
            [blockedReason]="submitting() ? 'Starting…' : ''"
            sendLabel="Start session"
            [hideSend]="true"
            placeholder="What should the agent do? Leave empty for an interactive session"
            (submitted)="start($event)"
            (refreshAccounts)="agentAccounts.refresh(selection().workerId)"
            (showRelogin)="showRelogin($event)"
          />
        } @else {
          <!-- Machines that report no agent accounts (older syred): pick the CLI by name. -->
          <div class="space-y-3">
            <p class="text-[12px] text-[var(--sy-text-secondary)]" data-testid="dispatch-no-accounts">
              {{ machineName() }} hasn't reported its agent accounts, so models and approval modes aren't available.
              The agent starts with its own defaults.
            </p>
            <div>
              <label for="session-dispatch-agent" class="block text-[11.5px] text-[var(--sy-text-muted)] mb-1.5">Agent</label>
              <select
                id="session-dispatch-agent"
                class="w-full rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-0)] px-3 py-2 text-[12.5px] text-[var(--sy-text)] focus:outline-none focus-visible:shadow-[var(--sy-focus-ring)]"
                [value]="fallbackAgent()"
                (change)="fallbackAgent.set($any($event.target).value)"
              >
                @for (a of fallbackAgents(); track a) {
                  <option [value]="a" [selected]="a === fallbackAgent()">{{ a }}</option>
                }
              </select>
            </div>
            <sy-context-bar [workerId]="selection().workerId" [cwd]="selection().cwd" [branch]="selection().branch" (changed)="onContext($event)" />
          </div>
        }
      } @else {
        <div class="space-y-1.5">
          <div class="text-[11.5px] text-[var(--sy-text-muted)] px-1">Where to open the shell</div>
          <sy-context-bar [workerId]="selection().workerId" [cwd]="selection().cwd" [branch]="selection().branch" (changed)="onContext($event)" />
        </div>
      }

      <div class="grid grid-cols-1 sm:grid-cols-2 gap-4 text-[12px]">
        <div>
          <label for="session-dispatch-branch" class="block text-[11.5px] text-[var(--sy-text-muted)] mb-1.5">New worktree branch (optional)</label>
          <input
            id="session-dispatch-branch"
            type="text"
            data-testid="dispatch-branch"
            class="w-full rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-0)] px-3 py-2 font-mono-ide text-[var(--sy-text)] placeholder-[#80828a] focus:outline-none focus-visible:shadow-[var(--sy-focus-ring)]"
            placeholder="feat/my-branch"
            [value]="newBranch()"
            (input)="newBranch.set($any($event.target).value)"
          />
        </div>
        <label class="flex items-start gap-2.5 cursor-pointer sm:pt-6">
          <input type="checkbox" class="mt-0.5 accent-[#6d8fbf]" data-testid="dispatch-sandboxed" [checked]="sandboxed()" (change)="sandboxed.set($any($event.target).checked)" />
          <span>
            <span class="block text-[var(--sy-text)]">Run sandboxed</span>
            <span class="block text-[11.5px] text-[var(--sy-text-muted)]">Restrict the session to its folder on the machine.</span>
          </span>
        </label>
      </div>

      @if (error()) {
        <div role="alert" class="rounded-[var(--sy-radius-md)] bg-[var(--sy-red-soft)] px-3 py-2 text-[12px] text-[#f6c0c0]">{{ error() }}</div>
      }

      <div class="flex justify-end gap-2">
        <button type="button" class="sy-btn" (click)="close.emit()">Cancel</button>
        <button type="button" class="sy-btn sy-btn-primary" data-testid="dispatch-submit" [disabled]="submitting() || !canSubmit()" (click)="submitFromFooter()">
          {{ submitting() ? 'Starting…' : sessionKind() === 'chat' ? 'Start session' : 'Open terminal' }}
        </button>
      </div>
    </app-modal-dialog>
  `,
})
export class SessionDispatchModalComponent implements OnInit {
  private readonly api = inject(ApiClientService);
  private readonly relogin = inject(ReloginService);
  readonly fleet = inject(FleetStore);
  readonly agentAccounts = inject(AgentAccountsStore);

  readonly request = input.required<SessionDispatchRequest>();
  readonly close = output<void>();
  /** The new session's task id, once it was dispatched. */
  readonly started = output<string>();

  private readonly composer = viewChild(ChatComposerComponent);

  readonly sessionKind = signal<SessionKind>('chat');
  readonly selection = signal<ComposerSelection>(emptySelection());
  readonly newBranch = signal('');
  readonly sandboxed = signal(false);
  readonly fallbackAgent = signal('claude');
  readonly submitting = signal(false);
  readonly error = signal('');

  /** Agent / model the shell's session defaults suggest (settings, last used). */
  private preferredAgent = '';
  private preferredModel = '';
  /** The request's context; dropped once the user picks another machine or folder. */
  private origin = { workerId: '', cwd: '', spaceId: '', repoId: '', worktreeId: '' };

  /** Every agent account on the chosen machine (PTY sessions don't need chat support). */
  readonly accounts = computed(() => this.agentAccounts.accountsFor(this.selection().workerId));
  readonly account = computed<AgentAccount | null>(() => this.accounts().find((a) => a.id === this.selection().accountId) ?? null);
  readonly machineName = computed(() => this.fleet.workersById()[this.selection().workerId]?.name || 'This machine');

  readonly fallbackAgents = computed(() => {
    const w = this.fleet.workersById()[this.selection().workerId];
    const detected = (w?.agents ?? []).filter((a) => a !== 'shell');
    return Array.from(new Set([...detected, 'claude', 'codex', 'grok', 'copilot', 'cursor', 'agy']));
  });

  constructor() {
    // Accounts arrive (or the machine changes): pick a valid account for that machine.
    effect(() => {
      const list = this.accounts();
      if (list.length === 0) return;
      untracked(() => {
        const sel = this.selection();
        if (list.some((a) => a.id === sel.accountId)) return;
        this.selection.set(this.seed(sel, list));
      });
    });
  }

  ngOnInit(): void {
    const { kind, worker, space, repo, worktree } = this.request();
    this.sessionKind.set(kind);
    this.sandboxed.set((this.fleet.settings().default_permission_mode || 'full_access') === 'sandboxed');

    const resolved = this.fleet.resolveSessionDefaults(space?.worker_id || worker?.id || '');
    this.preferredAgent = resolved.agent;
    this.preferredModel = resolved.model;
    if (resolved.agent) this.fallbackAgent.set(resolved.agent);

    const cwd = (repo ? worktree?.path || repo.path : space?.path) || '~';
    const branch = (repo ? worktree?.branch : space?.branch) || '';
    this.origin = {
      workerId: resolved.machine_id,
      cwd,
      spaceId: space?.id || '',
      repoId: repo?.id || '',
      worktreeId: repo ? worktree?.id || '' : '',
    };
    this.selection.set(this.seed({ ...emptySelection(), workerId: resolved.machine_id, cwd, branch }, this.accounts()));
  }

  private seed(sel: ComposerSelection, accounts: AgentAccount[]): ComposerSelection {
    if (accounts.length === 0) return sel;
    const account = pickDefaultAccount(accounts, { agent: this.preferredAgent });
    const withModel = findModel(account, this.preferredModel) ? { ...sel, model: this.preferredModel } : sel;
    return normalizeSelection(withModel, account);
  }

  onSelection(next: ComposerSelection): void {
    const prev = this.selection();
    if (next.workerId !== prev.workerId) {
      // Another machine: its own accounts (keep the agent if it has it there).
      const account = this.account();
      if (account) this.preferredAgent = account.agent;
      this.selection.set(this.seed({ ...next, accountId: '' }, this.agentAccounts.accountsFor(next.workerId)));
      return;
    }
    this.selection.set(next);
  }

  onContext(c: ContextChange): void {
    const s = this.selection();
    this.onSelection({
      ...s,
      workerId: c.workerId ?? s.workerId,
      cwd: c.cwd ?? s.cwd,
      branch: c.branch ?? (c.cwd !== undefined ? '' : s.branch),
    });
  }

  showRelogin(a: AgentAccount): void {
    this.relogin.show({ workerId: this.selection().workerId, accountId: a.id, label: a.label, relogin: a.auth?.relogin });
  }

  canSubmit(): boolean {
    if (!this.selection().cwd.trim()) return false;
    if (this.sessionKind() === 'chat') {
      if (this.accounts().length === 0) return !!this.fallbackAgent();
      const a = this.account();
      return !!a && a.auth?.state !== 'logged_out';
    }
    return true;
  }

  /** The footer button sends the composer's prompt (or opens the terminal). */
  submitFromFooter(): void {
    const composer = this.composer();
    if (this.sessionKind() === 'chat' && composer) {
      composer.submit();
      return;
    }
    void this.start({ text: '', attachments: [] });
  }

  async start(ev: ComposerSubmit): Promise<void> {
    if (this.submitting() || !this.canSubmit()) return;
    this.submitting.set(true);
    this.error.set('');
    const sel = this.selection();
    const isChat = this.sessionKind() === 'chat';
    const account = isChat ? this.account() : null;
    const sameContext = sel.workerId === this.origin.workerId && sel.cwd === this.origin.cwd;
    const branch = this.newBranch().trim();
    const perm: PermissionMode = this.sandboxed() ? 'sandboxed' : 'full_access';
    try {
      const res = await this.api.createSession({
        worker_selector: sel.workerId ? { worker_id: sel.workerId } : undefined,
        space_id: (sameContext && this.origin.spaceId) || undefined,
        repo_id: (sameContext && this.origin.repoId) || undefined,
        worktree_id: (sameContext && this.origin.worktreeId) || undefined,
        session_kind: this.sessionKind(),
        agent: isChat ? account?.agent || this.fallbackAgent() : 'shell',
        account_id: account?.id || undefined,
        model: (account && sel.model) || undefined,
        effort: (account && sel.effort) || undefined,
        approval_mode: (account && sel.approvalMode) || undefined,
        repo_path: sel.cwd.trim() || '~',
        branch: branch || undefined,
        use_worktree: !!branch,
        prompt: isChat ? ev.text.trim() : '',
        mode: 'interactive',
        origin: 'direct',
        permission_mode: perm,
      });
      await Promise.all([this.fleet.refresh(), this.fleet.loadSettings()]);
      const task = this.fleet.tasksById()[res.task_id];
      if (task) {
        this.started.emit(task.id);
      }
      this.composer()?.clear();
      this.close.emit();
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Failed to start session');
    } finally {
      this.submitting.set(false);
    }
  }
}

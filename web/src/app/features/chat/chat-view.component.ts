import { ChangeDetectionStrategy, Component, OnInit, computed, effect, inject, input, output, signal, untracked, viewChild } from '@angular/core';
import { AgentAccount, ChatStatus } from '../../core/models/agent-chat.models';
import { accountHealth, alternativeAccounts } from '../../core/models/agent-usage';
import { ChatSession } from '../../core/models/konsert.models';
import { KonsertSocketService } from '../../core/services/konsert-socket.service';
import { AgentAccountsStore } from '../../core/state/agent-accounts.store';
import { AgentChatStore } from '../../core/state/agent-chat.store';
import { ATTACHMENT_MARKER } from '../../core/state/chat-transcript';
import { FleetStore } from '../../core/state/fleet.store';
import { OrchestratorStore, isAgentChat } from '../../core/state/orchestrator.store';
import { IconComponent } from '../../shared/icon.component';
import { PopoverComponent, PopoverTriggerDirective } from '../../shared/popover.component';
import { TooltipDirective } from '../../shared/tooltip.directive';
import { causeLine } from '../../core/models/control.models';
import { SessionControlsComponent } from '../control/session-controls.component';
import { KeepToggleComponent } from '../control/keep-toggle.component';
import { ChatComposerComponent, ComposerSubmit } from './composer/chat-composer.component';
import { ComposerSelection, emptySelection, normalizeSelection, pickDefaultAccount } from './composer/composer-logic';
import { legacyItems } from './legacy-adapter';
import { ReloginService } from './relogin.service';
import { StatusPillComponent } from './status-pill.component';
import { ElicitationAnswer } from './transcript/elicitation-card.component';
import { ChatTranscriptComponent } from './transcript/chat-transcript.component';

const LAST_SELECTION_KEY = 'ks.composer.last';

function loadLastSelection(): Partial<ComposerSelection> {
  try {
    return JSON.parse(localStorage.getItem(LAST_SELECTION_KEY) || '{}') as Partial<ComposerSelection>;
  } catch {
    return {};
  }
}

function saveLastSelection(sel: ComposerSelection): void {
  try {
    const { workerId, accountId, model, effort, approvalMode } = sel;
    localStorage.setItem(LAST_SELECTION_KEY, JSON.stringify({ workerId, accountId, model, effort, approvalMode }));
  } catch {
    // storage unavailable
  }
}

export function chatTitleFrom(text: string): string {
  const line = text.split('\n').find((l) => l.trim()) ?? 'New chat';
  const t = line.trim();
  return t.length > 60 ? `${t.slice(0, 57)}…` : t;
}

/** The main chat tab: chat switcher + status, transcript, composer. */
@Component({
  selector: 'app-chat-view',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [
    IconComponent,
    TooltipDirective,
    PopoverComponent,
    PopoverTriggerDirective,
    StatusPillComponent,
    ChatTranscriptComponent,
    ChatComposerComponent,
    SessionControlsComponent,
    KeepToggleComponent,
  ],
  template: `
    <div class="flex flex-col h-full w-full bg-[var(--ks-surface-0)]">
      <!-- Header -->
      <header class="flex items-center gap-2 px-3 sm:px-4 h-12 shrink-0">
        @if (renaming()) {
          <form class="flex items-center gap-1.5" (submit)="$event.preventDefault(); commitRename()">
            <label class="ks-sr-only" for="ks-chat-rename">Chat title</label>
            <input
              id="ks-chat-rename"
              type="text"
              autofocus
              class="h-8 w-[240px] rounded-[var(--ks-radius-md)] bg-[var(--ks-surface-2)] px-3 text-[12.5px] text-[var(--ks-text)] outline-none focus-visible:shadow-[var(--ks-focus-ring)]"
              [value]="renameDraft()"
              (input)="renameDraft.set($any($event.target).value)"
              (keydown.escape)="renaming.set(false)"
            />
            <button type="submit" class="ks-btn ks-btn-sm ks-btn-primary">Save</button>
            <button type="button" class="ks-btn ks-btn-sm" (click)="renaming.set(false)">Cancel</button>
          </form>
        } @else {
          <ks-popover label="Chats" placement="bottom-start" panelClass="w-[340px] max-h-[420px] overflow-y-auto" [(open)]="switcherOpen">
            <button type="button" ksPopoverTrigger class="ks-btn gap-2 -ml-1.5 max-w-[38vw] sm:max-w-[min(46vw,320px)]" data-testid="chat-switcher" [attr.aria-label]="'Chat: ' + title()">
              @if (active(); as s) {
                <app-icon [name]="s.agent || 'orchestrator'" [size]="14" />
              }
              <span class="truncate text-[13px] font-semibold text-[var(--ks-text)]">{{ title() }}</span>
              <svg aria-hidden="true" width="10" height="10" viewBox="0 0 10 10" fill="none" stroke="currentColor" stroke-width="1.5" class="shrink-0 opacity-70">
                <path d="M2.5 4 5 6.5 7.5 4" stroke-linecap="round" stroke-linejoin="round" />
              </svg>
            </button>
            <button type="button" role="menuitem" class="ks-menu-item items-center" (click)="newChat()">
              <app-icon name="plus" [size]="14" />
              <span class="ks-menu-label">New chat</span>
            </button>
            @if (orch.sessions().length) {
              <div class="ks-menu-sep"></div>
            }
            @for (s of orch.sessions(); track s.id) {
              <button
                type="button"
                role="menuitemradio"
                class="ks-menu-item items-center"
                [attr.aria-checked]="s.id === orch.activeSessionId()"
                [attr.data-chat]="s.id"
                (click)="select(s)"
              >
                <app-icon [name]="s.agent || 'orchestrator'" [size]="14" />
                <span class="flex-1 min-w-0">
                  <span class="ks-menu-label block truncate">{{ s.title }}</span>
                  <span class="ks-menu-desc block truncate">{{ sessionSubtitle(s) }}</span>
                </span>
                @if (statusOf(s); as st) {
                  @if (st !== 'idle') {
                    <ks-status-pill [status]="st" [compact]="true" />
                  }
                }
              </button>
            }
          </ks-popover>

          <div aria-live="polite" class="shrink-0 max-w-[45%] overflow-hidden hidden sm:flex items-center">
            @if (active()) {
              <ks-status-pill [status]="status()" [subtitle]="subtitle()" data-testid="header-status" />
            }
          </div>
          @if (active() && (status() === 'working' || status() === 'needs_you' || status() === 'starting')) {
            <button type="button" class="ks-btn ks-btn-sm ks-btn-danger" data-testid="header-stop" (click)="stopTurn()">
              <app-icon name="stop" [size]="11" />
              Stop
            </button>
          }
          @if (active(); as s) {
            @if (isAgent()) {
              <ks-session-controls kind="chat" [sessionId]="s.id" [status]="status()" [compact]="true" />
            }
            @if (causeText()) {
              <span class="hidden lg:inline truncate text-[11px] text-[var(--ks-text-muted)]" data-testid="header-cause">{{ causeText() }}</span>
            }
          }
        }

        <span class="flex-1"></span>

        @if (active(); as s) {
          @if (!renaming()) {
            <ks-keep-toggle kind="chat" [id]="s.id" [keep]="!!s.keep" [compact]="true" />
          }
        }
        @if (active() && !renaming()) {
          <button type="button" class="ks-icon-btn hidden sm:inline-flex" aria-label="Rename chat" ksTooltip="Rename chat" (click)="startRename()">
            <app-icon name="edit" [size]="14" />
          </button>
          <button type="button" class="ks-icon-btn ks-btn-danger hidden sm:inline-flex" aria-label="Delete chat" ksTooltip="Delete chat" (click)="deleteActive()">
            <app-icon name="trash" [size]="14" />
          </button>
        }
        <button type="button" class="ks-btn" (click)="newChat()" data-testid="new-chat" aria-label="New chat">
          <app-icon name="plus" [size]="13" />
          <span class="hidden sm:inline">New chat</span>
        </button>
        <button type="button" class="ks-btn" (click)="openNewTerminal.emit()" aria-label="New terminal">
          <app-icon name="shell" [size]="13" />
          <span class="hidden sm:inline">Terminal</span>
        </button>
      </header>

      @if (active()) {
        <div class="sm:hidden flex items-center gap-2 px-3 pb-1" aria-hidden="true">
          <ks-status-pill [status]="status()" [subtitle]="subtitle()" [compact]="true" />
        </div>
      }

      <!-- Body -->
      @if (active(); as s) {
        <ks-chat-transcript
          class="flex-1"
          [items]="items()"
          [status]="status()"
          [subtitle]="subtitle()"
          [agent]="s.agent || ''"
          [accountLabel]="activeAccount()?.label || s.account_id || ''"
          [workerId]="s.worker_id || ''"
          [accountId]="s.account_id || ''"
          [relogin]="activeAccount()?.auth?.relogin"
          [alternatives]="alternatives()"
          [compactedAt]="s.compacted_at"
          [compactedBefore]="s.compacted_before"
          (answer)="respond($event)"
          (retry)="retry()"
          (switchAccount)="switchTo($event)"
          (keepWaiting)="keepWaiting($event)"
          (stop)="stopTurn()"
        />
      } @else {
        <div class="flex-1 min-h-0 overflow-y-auto flex flex-col items-center justify-center px-6 text-center">
          <img src="icon.svg" alt="" class="h-9 w-9 mb-4 opacity-90" />
          <h2 class="text-[19px] font-semibold tracking-tight text-[var(--ks-text)] mb-1.5">What should we work on?</h2>
          <p class="max-w-md text-[13px] text-[var(--ks-text-secondary)] mb-6">
            Chat with the agents you're already signed in to, on any of your machines. No API key needed.
          </p>
          <div class="flex flex-wrap justify-center gap-1.5 mb-4">
            @for (p of suggestions; track p.label) {
              <button type="button" class="ks-btn ks-btn-soft" (click)="composer()?.setDraft(p.prompt)">{{ p.label }}</button>
            }
          </div>
          <button
            type="button"
            class="ks-btn ks-btn-sm"
            [attr.aria-pressed]="fleetTools()"
            ksTooltip="Lets the agent see your fleet and dispatch work to other machines"
            (click)="fleetTools.set(!fleetTools())"
          >
            <span class="h-3.5 w-6 rounded-full p-0.5 transition-colors" [class.bg-[var(--ks-blue)]]="fleetTools()" [class.bg-[#3a3c44]]="!fleetTools()">
              <span class="block h-2.5 w-2.5 rounded-full bg-white transition-transform" [class.translate-x-2.5]="fleetTools()"></span>
            </span>
            Fleet tools
          </button>
        </div>
      }

      @if (bannerError()) {
        <div class="max-w-3xl w-full mx-auto px-4">
          <div role="alert" class="flex items-center gap-2 rounded-[var(--ks-radius-md)] bg-[var(--ks-red-soft)] px-3 py-2 text-[12px] text-[#f6c0c0]">
            <app-icon name="alert" [size]="13" />
            <span class="flex-1">{{ bannerError() }}</span>
            <button type="button" class="ks-icon-btn ks-icon-btn-sm" aria-label="Dismiss" (click)="clearErrors()">
              <app-icon name="close" [size]="11" />
            </button>
          </div>
        </div>
      }

      <!-- Composer -->
      <div class="px-3 sm:px-4 pb-3 pt-2 shrink-0">
        <div class="max-w-3xl mx-auto">
          <ks-chat-composer
            [accounts]="composerAccounts()"
            [(selection)]="selection"
            [mode]="isApiChat() ? 'api' : 'agent'"
            [busy]="busy()"
            [contextLocked]="!!active()"
            [sessionId]="orch.activeSessionId()"
            [approvalNote]="active() ? 'Changes apply live when the agent supports it, otherwise from the next turn.' : ''"
            [blockedReason]="creating() ? 'Starting the chat…' : status() === 'suspended' ? 'Suspended: resume the chat to continue' : ''"
            (submitted)="submit($event)"
            (stop)="stopTurn()"
            (controlChange)="onControlChange($event)"
            (refreshAccounts)="accounts.refresh(selection().workerId)"
            (showRelogin)="showRelogin($event)"
          />
        </div>
      </div>
    </div>
  `,
})
export class ChatViewComponent implements OnInit {
  readonly openNewTerminal = output<void>();

  readonly orch = inject(OrchestratorStore);
  readonly agentChat = inject(AgentChatStore);
  readonly accounts = inject(AgentAccountsStore);
  private readonly fleet = inject(FleetStore);
  private readonly socket = inject(KonsertSocketService);
  private readonly relogin = inject(ReloginService);
  readonly composer = viewChild(ChatComposerComponent);

  readonly switcherOpen = signal(false);
  readonly renaming = signal(false);
  readonly renameDraft = signal('');
  readonly creating = signal(false);
  readonly fleetTools = signal(true);
  readonly selection = signal<ComposerSelection>(emptySelection());
  private draftSeeded = false;

  readonly suggestions = [
    { label: 'Review my changes', prompt: 'Review the uncommitted changes in this checkout and point out bugs.' },
    { label: 'Fix failing tests', prompt: 'Run the tests, find the failures and fix them.' },
    { label: 'Explain this repo', prompt: 'Give me a quick tour of this repository: layout, entry points and how to run it.' },
  ];

  readonly active = computed(() => this.orch.sessions().find((s) => s.id === this.orch.activeSessionId()) ?? null);
  readonly isApiChat = computed(() => !!this.active() && !isAgentChat(this.active()));
  readonly isAgent = computed(() => !!this.active() && isAgentChat(this.active()));
  /** "Suspended by you via the Konsert UI" for user-caused states; empty otherwise. */
  readonly causeText = computed(() => {
    const s = this.active();
    const st = this.status();
    if (!s?.cause || s.cause.actor !== 'user') return '';
    if (st !== 'suspended' && st !== 'cancelled' && st !== 'failed') return '';
    return causeLine(st, s.cause);
  });
  readonly title = computed(() => this.active()?.title || 'New chat');

  readonly transcript = computed(() => {
    const s = this.active();
    return s && isAgentChat(s) ? this.agentChat.transcript(s.id) : null;
  });
  readonly items = computed(() => {
    const t = this.transcript();
    if (t) return t.items;
    return legacyItems(this.orch.messages(), this.orch.streamingBuffer(), this.orch.activeToolCards());
  });
  readonly status = computed<ChatStatus>(() => {
    const t = this.transcript();
    if (t) return t.status;
    return this.orch.sending() ? 'working' : 'idle';
  });
  readonly subtitle = computed(() => this.transcript()?.subtitle ?? '');
  readonly busy = computed(() => {
    const st = this.status();
    return !!this.active() && (st === 'working' || st === 'starting');
  });

  readonly composerAccounts = computed(() => this.accounts.accountsFor(this.selection().workerId));
  readonly activeAccount = computed<AgentAccount | null>(() => {
    const s = this.active();
    if (!s?.worker_id) return null;
    return this.accounts.find(s.worker_id, s.account_id || '') ?? null;
  });
  readonly alternatives = computed(() => {
    const s = this.active();
    const acct = this.activeAccount();
    return s?.worker_id && acct ? alternativeAccounts(acct, this.accounts.accountsFor(s.worker_id)) : [];
  });
  readonly bannerError = computed(() => this.agentChat.error() || this.orch.error());

  constructor() {
    // Existing agent chat: mirror its settings into the composer and load its events.
    effect(() => {
      const id = this.orch.activeSessionId();
      const s = untracked(() => this.orch.sessions().find((x) => x.id === id));
      if (!s || !isAgentChat(s)) return;
      untracked(() => {
        this.selection.set({
          workerId: s.worker_id || '',
          accountId: s.account_id || '',
          model: s.model || '',
          effort: s.effort || '',
          approvalMode: s.approval_mode || '',
          cwd: s.cwd || '~',
          branch: '',
        });
        void this.agentChat.open(s.id);
      });
    });
    // New chat: seed machine/account/model from last use, settings and capabilities.
    effect(() => {
      const s = this.orch.activeSessionId();
      const loaded = this.accounts.loaded();
      const byWorker = this.accounts.byWorker();
      this.fleet.workers();
      if (s) {
        this.draftSeeded = false;
        return;
      }
      untracked(() => this.seedDraft(loaded, Object.keys(byWorker).length > 0));
    });
  }

  ngOnInit(): void {
    void this.orch.init();
  }

  private seedDraft(loaded: boolean, haveAccounts: boolean): void {
    const cur = this.selection();
    const curAccounts = this.accounts.accountsFor(cur.workerId);
    // Keep a valid user choice; re-seed when nothing is chosen or it vanished.
    if (this.draftSeeded && cur.accountId && curAccounts.some((a) => a.id === cur.accountId)) return;
    const last = loadLastSelection();
    const settings = this.fleet.settings();
    let workerId = cur.workerId || last.workerId || this.fleet.resolveSessionDefaults().machine_id || '';
    if (haveAccounts && this.accounts.accountsFor(workerId).length === 0) {
      workerId = this.accounts.all()[0]?.workerId ?? workerId;
    }
    const list = this.accounts.accountsFor(workerId);
    const account = pickDefaultAccount(list, {
      accountId: last.workerId === workerId ? last.accountId : settings.default_account_id,
      agent: settings.default_agent,
    });
    const base: ComposerSelection = {
      ...emptySelection(),
      workerId,
      cwd: cur.cwd || '~',
      branch: cur.branch,
      model: last.accountId === account?.id ? last.model || '' : settings.default_model || '',
      effort: last.accountId === account?.id ? last.effort || '' : settings.default_effort || '',
      approvalMode: last.accountId === account?.id ? last.approvalMode || '' : '',
    };
    this.selection.set(normalizeSelection(base, account));
    if (loaded) this.draftSeeded = true;
  }

  statusOf(s: ChatSession): ChatStatus {
    return isAgentChat(s) ? this.agentChat.statusOf(s.id) : 'idle';
  }

  sessionSubtitle(s: ChatSession): string {
    if (!isAgentChat(s)) return 'API-key orchestrator';
    const acct = s.worker_id ? this.accounts.find(s.worker_id, s.account_id || '') : undefined;
    const host = s.worker_id ? this.fleet.workersById()[s.worker_id]?.name : '';
    return [acct?.label || s.account_id || s.agent, host].filter(Boolean).join(' · ');
  }

  select(s: ChatSession): void {
    void this.orch.selectSession(s.id);
  }

  newChat(): void {
    this.draftSeeded = false;
    this.orch.startDraft();
    queueMicrotask(() => this.composer()?.focus());
  }

  async submit(msg: ComposerSubmit): Promise<void> {
    const paths = msg.attachments.map((a) => a.remotePath);
    const active = this.active();
    if (active && !isAgentChat(active)) {
      const payload = paths.length ? `${msg.text}${ATTACHMENT_MARKER}${paths.map((p) => `- ${p}`).join('\n')}` : msg.text;
      if (this.socket.sendChatMessage(active.id, payload)) this.composer()?.clear();
      return;
    }
    if (active) {
      if (this.agentChat.send(active, msg.text, paths)) this.composer()?.clear();
      return;
    }
    // Draft: create the chat with the composer's choices, then send the first turn.
    const sel = this.selection();
    const account = this.accounts.find(sel.workerId, sel.accountId);
    if (!account) return;
    this.creating.set(true);
    try {
      saveLastSelection(sel);
      const chat = await this.agentChat.create({
        title: chatTitleFrom(msg.text || msg.attachments[0]?.filename || 'New chat'),
        worker_id: sel.workerId,
        agent: account.agent,
        account_id: account.id,
        model: sel.model || undefined,
        effort: sel.effort || undefined,
        approval_mode: sel.approvalMode || undefined,
        cwd: sel.cwd || '~',
        fleet_tools: this.fleetTools(),
      });
      if (this.agentChat.send(chat, msg.text, paths)) this.composer()?.clear();
    } catch (err) {
      this.agentChat.error.set(err instanceof Error ? `Could not start the chat: ${err.message}` : 'Could not start the chat');
    } finally {
      this.creating.set(false);
    }
  }

  onControlChange(change: Partial<ComposerSelection>): void {
    const s = this.active();
    if (!s) {
      saveLastSelection(this.selection());
      return;
    }
    if (!isAgentChat(s)) return;
    saveLastSelection(this.selection());
    if (change.accountId && change.accountId !== s.account_id) {
      void this.agentChat.switchAccount(s.id, change.accountId);
    }
    this.agentChat.control(s.id, { model: change.model, effort: change.effort, approval_mode: change.approvalMode });
  }

  respond(a: ElicitationAnswer): void {
    const s = this.active();
    if (s) this.agentChat.respond(s.id, a.elicitationId, { optionId: a.optionId, text: a.text, label: a.label });
  }

  retry(): void {
    const s = this.active();
    if (s) this.agentChat.retry(s);
  }

  switchTo(acct: AgentAccount): void {
    const s = this.active();
    if (!s) return;
    const next = normalizeSelection(this.selection(), acct);
    this.selection.set(next);
    this.onControlChange({ accountId: acct.id, model: next.model, effort: next.effort, approvalMode: next.approvalMode });
  }

  keepWaiting(errorKey: string): void {
    const s = this.active();
    if (s) this.agentChat.dismissError(s.id, errorKey);
  }

  stopTurn(): void {
    const s = this.active();
    if (s && isAgentChat(s)) this.agentChat.cancel(s.id);
  }

  showRelogin(a: AgentAccount): void {
    this.relogin.show({ workerId: this.selection().workerId, accountId: a.id, label: a.label, relogin: a.auth?.relogin });
  }

  startRename(): void {
    this.renameDraft.set(this.active()?.title || '');
    this.renaming.set(true);
  }

  async commitRename(): Promise<void> {
    const s = this.active();
    const title = this.renameDraft().trim();
    this.renaming.set(false);
    if (!s || !title) return;
    try {
      await this.orch.renameSession(s.id, title);
    } catch {
      // keep the old title
    }
  }

  async deleteActive(): Promise<void> {
    const s = this.active();
    if (!s || !window.confirm(`Delete chat "${s.title}"?`)) return;
    try {
      await this.orch.deleteSession(s.id);
    } catch {
      // ignore
    }
  }

  clearErrors(): void {
    this.agentChat.error.set('');
    this.orch.error.set('');
  }

  /** Exposed for the footer/chip flows. */
  healthOf(a: AgentAccount) {
    return accountHealth(a);
  }
}

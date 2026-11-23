import { ChangeDetectionStrategy, Component, OnInit, computed, inject, input, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { PermissionMode, Repo, SessionKind, Space, Worker, Worktree } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { FleetStore, guessModelForAgent } from '../core/state/fleet.store';
import { TabsStore } from '../core/state/tabs.store';
import { IconComponent } from '../shared/icon.component';
import { RemoteDirPickerComponent } from '../shared/remote-dir-picker.component';

/** What the session modal was opened for. */
export interface SessionDispatchRequest {
  kind: SessionKind;
  worker: Worker | null;
  space?: Space | null;
  repo?: Repo | null;
  worktree?: Worktree | null;
}

function formatSourceHint(src: string): string {
  switch (src) {
    case 'localhost':
      return 'localhost';
    case 'default':
      return 'default';
    case 'last_used':
      return 'last used';
    case 'guess':
      return 'auto';
    default:
      return '';
  }
}

/** Unified Chat Session vs Terminal modal with the Machine / Agent / Model cascade. */
@Component({
  selector: 'app-session-dispatch-modal',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [FormsModule, IconComponent, RemoteDirPickerComponent],
  template: `
    <div class="ks-backdrop fixed inset-0 z-50 flex items-center justify-center px-4" (click)="close.emit()">
      <div class="ks-modal w-full max-w-2xl rounded-xl border border-[#2d3038] bg-[#1b1c20] p-5 space-y-4" (click)="$event.stopPropagation()">
        <div class="flex items-center justify-between">
          <div class="inline-flex rounded-lg bg-[#111214] p-0.5 border border-[#26282e] text-[12px]">
            <button
              type="button"
              (click)="sessionKind.set('chat')"
              class="rounded-md px-3 py-1 font-medium transition cursor-pointer"
              [class.bg-[#262830]]="sessionKind() === 'chat'"
              [class.text-[#ececed]]="sessionKind() === 'chat'"
              [class.text-[#787a82]]="sessionKind() !== 'chat'"
            >
              Chat Session
            </button>
            <button
              type="button"
              (click)="sessionKind.set('terminal')"
              class="rounded-md px-3 py-1 font-medium transition cursor-pointer"
              [class.bg-[#262830]]="sessionKind() === 'terminal'"
              [class.text-[#ececed]]="sessionKind() === 'terminal'"
              [class.text-[#787a82]]="sessionKind() !== 'terminal'"
            >
              Terminal
            </button>
          </div>

          <button type="button" (click)="close.emit()" class="text-[#787a82] hover:text-[#ececed] transition cursor-pointer">
            <app-icon name="close" [size]="15" />
          </button>
        </div>

        <form (ngSubmit)="submit()" class="space-y-3 text-[12px]">
          @if (sessionKind() === 'chat') {
            <div class="grid grid-cols-1 sm:grid-cols-3 gap-3">
              <div>
                <div class="flex items-center justify-between mb-1">
                  <label class="text-[11px] text-[#8e9098]">Machine</label>
                  @if (machineSourceHint()) {
                    <span class="text-[10px] text-[#686a72]">{{ machineSourceHint() }}</span>
                  }
                </div>
                <select
                  name="worker"
                  [ngModel]="workerId()"
                  (ngModelChange)="onMachineChange($event)"
                  class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed] focus:border-[#4a668a] focus:outline-none"
                >
                  @if (fleet.workers().length === 0) {
                    <option value="">localhost (no daemon connected)</option>
                  }
                  @for (w of fleet.workers(); track w.id) {
                    <option [value]="w.id">{{ w.name }} ({{ w.status }})</option>
                  }
                </select>
              </div>

              <div>
                <div class="flex items-center justify-between mb-1">
                  <label class="text-[11px] text-[#8e9098]">Agent</label>
                  @if (agentSourceHint()) {
                    <span class="text-[10px] text-[#686a72]">{{ agentSourceHint() }}</span>
                  }
                </div>
                <select
                  name="agent"
                  [ngModel]="agent()"
                  (ngModelChange)="onAgentChange($event)"
                  class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed] focus:border-[#4a668a] focus:outline-none"
                >
                  @for (a of chatAgentOptions(); track a) {
                    <option [value]="a">{{ a }}</option>
                  }
                </select>
              </div>

              <div>
                <div class="flex items-center justify-between mb-1">
                  <label class="text-[11px] text-[#8e9098]">Model</label>
                  @if (modelSourceHint()) {
                    <span class="text-[10px] text-[#686a72]">{{ modelSourceHint() }}</span>
                  }
                </div>
                <input
                  type="text"
                  name="sessionModel"
                  list="ks-model-suggestions"
                  [ngModel]="model()"
                  (ngModelChange)="onModelChange($event)"
                  placeholder="Auto"
                  class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 font-mono-ide text-[#ececed] focus:border-[#4a668a] focus:outline-none"
                />
                <datalist id="ks-model-suggestions">
                  @for (m of modelSuggestions(); track m) {
                    <option [value]="m"></option>
                  }
                </datalist>
              </div>
            </div>
          } @else {
            <div>
              <label class="block text-[11px] text-[#8e9098] mb-1">Machine</label>
              <select
                name="worker"
                [ngModel]="workerId()"
                (ngModelChange)="onMachineChange($event)"
                class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed] focus:border-[#4a668a] focus:outline-none"
              >
                @if (fleet.workers().length === 0) {
                  <option value="">localhost (no daemon connected)</option>
                }
                @for (w of fleet.workers(); track w.id) {
                  <option [value]="w.id">{{ w.name }} ({{ w.status }})</option>
                }
              </select>
            </div>
          }

          <div>
            <label class="block text-[11px] text-[#8e9098] mb-1">Workspace Directory</label>
            <app-remote-dir-picker
              [workerId]="workerId()"
              [value]="repoPath()"
              (valueChange)="repoPath.set($event)"
              inputName="repo"
              placeholder="~ or /path/to/repo"
            />
          </div>

          @if (sessionKind() === 'chat') {
            <div>
              <label class="block text-[11px] text-[#8e9098] mb-1">Initial Prompt (optional)</label>
              <textarea
                rows="3"
                name="prompt"
                [ngModel]="prompt()"
                (ngModelChange)="prompt.set($event)"
                placeholder="Ask the agent anything, or leave blank to start an interactive session..."
                class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed] focus:border-[#4a668a] focus:outline-none"
              ></textarea>
            </div>
          }

          <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
            <div>
              <label class="block text-[11px] text-[#8e9098] mb-1">Git Branch (optional worktree)</label>
              <input
                type="text"
                name="branch"
                [ngModel]="branch()"
                (ngModelChange)="branch.set($event)"
                placeholder="feat/my-branch"
                class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 font-mono-ide text-[#ececed] focus:border-[#4a668a] focus:outline-none"
              />
            </div>
            <div>
              <label class="block text-[11px] text-[#8e9098] mb-1">Permission Mode</label>
              <select
                name="perm"
                [ngModel]="perm()"
                (ngModelChange)="perm.set($event)"
                class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed]"
              >
                <option value="full_access">Full Access (Default)</option>
                <option value="sandboxed">Sandboxed (Opt-In)</option>
              </select>
            </div>
          </div>

          @if (error()) {
            <div class="rounded-md border border-[#e05d5d]/30 bg-[#e05d5d]/10 px-3 py-2 text-[#f09595]">
              {{ error() }}
            </div>
          }

          <div class="flex justify-end gap-2 pt-1">
            <button
              type="button"
              (click)="close.emit()"
              class="rounded-md border border-[#2c2e36] px-3 py-1.5 text-[#a1a3ab] hover:text-[#ececed] transition cursor-pointer"
            >
              Cancel
            </button>
            <button
              type="submit"
              [disabled]="submitting() || !canSubmit()"
              class="rounded-md bg-[#ececed] hover:bg-white active:scale-[0.98] disabled:opacity-40 px-4 py-1.5 font-medium text-[#111214] transition cursor-pointer"
            >
              {{ submitting() ? 'Starting...' : sessionKind() === 'chat' ? 'Start Chat' : 'Open Terminal' }}
            </button>
          </div>
        </form>
      </div>
    </div>
  `,
})
export class SessionDispatchModalComponent implements OnInit {
  private readonly api = inject(ApiClientService);
  private readonly tabs = inject(TabsStore);
  readonly fleet = inject(FleetStore);

  readonly request = input.required<SessionDispatchRequest>();
  readonly close = output<void>();

  readonly sessionKind = signal<SessionKind>('chat');
  readonly spaceId = signal<string | null>(null);
  readonly repoId = signal<string | null>(null);
  readonly worktreeId = signal<string | null>(null);
  readonly agent = signal('claude');
  readonly model = signal('');
  readonly workerId = signal('');
  readonly perm = signal<PermissionMode>('full_access');
  readonly repoPath = signal('~');
  readonly branch = signal('');
  readonly prompt = signal('');
  readonly machineSourceHint = signal('');
  readonly agentSourceHint = signal('');
  readonly modelSourceHint = signal('');
  readonly submitting = signal(false);
  readonly error = signal('');

  readonly chatAgentOptions = computed(() => {
    const w = this.fleet.workersById()[this.workerId()];
    const detected = (w?.agents ?? []).filter((a) => a !== 'shell');
    const base = ['agy', 'claude', 'codex', 'grok', 'mock'];
    return Array.from(new Set([...detected, ...base]));
  });

  readonly modelSuggestions = computed(() => {
    switch (this.agent().toLowerCase()) {
      case 'claude':
        return ['claude-sonnet-4-5', 'claude-opus-4-1', 'claude-haiku-3-5'];
      case 'agy':
        return ['gemini-2.5-pro', 'gemini-2.5-flash'];
      case 'codex':
        return ['o4-mini', 'o3', 'gpt-4.1'];
      case 'grok':
        return ['grok-code-fast-1', 'grok-4'];
      default:
        return ['claude-sonnet-4-5', 'gemini-2.5-pro', 'o4-mini', 'grok-code-fast-1'];
    }
  });

  ngOnInit(): void {
    const { kind, worker, space, repo, worktree } = this.request();
    this.sessionKind.set(kind);
    this.spaceId.set(space?.id || null);
    this.perm.set(this.fleet.settings().default_permission_mode || 'full_access');

    const explicitMachine = space?.worker_id || worker?.id || '';
    const resolved = this.fleet.resolveSessionDefaults(explicitMachine);
    this.workerId.set(resolved.machine_id);
    this.machineSourceHint.set(formatSourceHint(resolved.machine_source));
    this.agent.set(resolved.agent);
    this.agentSourceHint.set(formatSourceHint(resolved.agent_source));
    this.model.set(resolved.model);
    this.modelSourceHint.set(formatSourceHint(resolved.model_source));

    if (repo) {
      this.repoId.set(repo.id);
      this.worktreeId.set(worktree?.id || null);
      this.repoPath.set(worktree?.path || repo.path || '~');
      this.branch.set(worktree?.branch || '');
    } else {
      this.repoPath.set(space?.path || '~');
      this.branch.set(space?.branch || '');
    }
  }

  onMachineChange(workerId: string): void {
    this.workerId.set(workerId);
    this.machineSourceHint.set('');
    const resolved = this.fleet.resolveSessionDefaults(workerId);
    if (this.agentSourceHint() !== '') {
      this.agent.set(resolved.agent);
      this.agentSourceHint.set(formatSourceHint(resolved.agent_source));
    }
  }

  onAgentChange(agent: string): void {
    this.agent.set(agent);
    this.agentSourceHint.set('');
    if (this.modelSourceHint() === 'auto' || !this.model().trim()) {
      this.model.set(guessModelForAgent(agent, this.fleet.settings().llm_model));
      this.modelSourceHint.set('auto');
    }
  }

  onModelChange(model: string): void {
    this.model.set(model);
    this.modelSourceHint.set('');
  }

  canSubmit(): boolean {
    if (!this.repoPath().trim()) return false;
    if (this.sessionKind() === 'chat' && !this.agent().trim()) return false;
    return true;
  }

  async submit(): Promise<void> {
    this.submitting.set(true);
    this.error.set('');
    try {
      const isChat = this.sessionKind() === 'chat';
      const res = await this.api.createSession({
        worker_selector: this.workerId() ? { worker_id: this.workerId() } : undefined,
        space_id: this.spaceId() || undefined,
        repo_id: this.repoId() || undefined,
        worktree_id: this.worktreeId() || undefined,
        session_kind: this.sessionKind(),
        agent: isChat ? this.agent() : 'shell',
        model: isChat ? this.model().trim() || undefined : undefined,
        repo_path: this.repoPath().trim() || '~',
        branch: this.branch().trim() || undefined,
        use_worktree: !!this.branch().trim(),
        prompt: isChat ? this.prompt().trim() : '',
        mode: 'interactive',
        origin: 'direct',
        permission_mode: this.perm(),
      });
      await Promise.all([this.fleet.refresh(), this.fleet.loadSettings()]);
      const task = this.fleet.tasksById()[res.task_id];
      if (task) {
        const w = this.fleet.workersById()[task.worker_id];
        this.tabs.openTaskTab(task, w?.name);
      }
      this.prompt.set('');
      this.close.emit();
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Failed to start session');
    } finally {
      this.submitting.set(false);
    }
  }
}

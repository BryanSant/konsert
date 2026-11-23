import { Component, ElementRef, OnInit, ViewChild, computed, effect, inject, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ChatMessage, PermissionMode } from '../../core/models/konsert.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { AuthService } from '../../core/services/auth.service';
import { KonsertSocketService } from '../../core/services/konsert-socket.service';
import { NotificationService } from '../../core/services/notification.service';
import { FleetStore, guessModelForAgent } from '../../core/state/fleet.store';
import { OrchestratorStore } from '../../core/state/orchestrator.store';
import { TabsStore } from '../../core/state/tabs.store';
import { IconComponent } from '../../shared/icon.component';
import { MarkdownViewComponent } from '../../shared/markdown-view.component';

export interface UploadedAttachment {
  filename: string;
  remotePath: string;
  size: number;
  workerId: string;
}

const ATTACHMENT_MARKER = '\n\n[Attached files on host]\n';

export function parseUserMessageWithAttachments(content: string): { text: string; files: string[] } {
  const idx = content.indexOf(ATTACHMENT_MARKER);
  if (idx === -1) {
    return { text: content, files: [] };
  }
  const visible = content.slice(0, idx).trim();
  const trailer = content.slice(idx + ATTACHMENT_MARKER.length);
  const files = trailer
    .split('\n')
    .map((line) => line.replace(/^-\s*/, '').trim())
    .filter(Boolean)
    .map((fullPath) => {
      const base = fullPath.split(/[\\/]/).pop() || fullPath;
      // Strip timestamp-rand prefix if formatted as "<prefix>-<filename>"
      const parts = base.split('-');
      if (parts.length >= 3 && /^\d{9,}$/.test(parts[0])) {
        return parts.slice(2).join('-');
      }
      if (parts.length >= 4 && /^\d{9,}$/.test(parts[1])) {
        return parts.slice(3).join('-');
      }
      return base;
    });
  return { text: visible, files };
}

export function extractTaskIdsFromToolPayload(raw: unknown): string[] {
  const found = new Set<string>();
  const visit = (val: unknown, depth: number) => {
    if (depth > 4 || val == null) return;
    if (typeof val === 'string') {
      try {
        const parsed = JSON.parse(val);
        if (parsed && typeof parsed === 'object') {
          visit(parsed, depth + 1);
        }
      } catch {
        // plain string
      }
      return;
    }
    if (Array.isArray(val)) {
      for (const item of val) visit(item, depth + 1);
      return;
    }
    if (typeof val === 'object') {
      const obj = val as Record<string, unknown>;
      if (typeof obj['task_id'] === 'string' && obj['task_id'].length >= 6) {
        found.add(obj['task_id']);
      }
      if (typeof obj['id'] === 'string' && obj['id'].length >= 6 && ('agent' in obj || 'worker_id' in obj)) {
        found.add(obj['id']);
      }
      for (const k of ['tasks', 'results', 'dispatched', 'data']) {
        if (k in obj) visit(obj[k], depth + 1);
      }
    }
  };
  visit(raw, 0);
  return Array.from(found);
}

@Component({
  selector: 'app-orchestrator-chat',
  standalone: true,
  imports: [FormsModule, IconComponent, MarkdownViewComponent],
  template: `
    <div class="flex flex-col h-full w-full bg-[#111214]">
      <!-- Subtle Top Bar with Session Selector, Rename & Delete -->
      <div class="flex items-center justify-between gap-2 px-4 py-2 border-b border-[#22242a] bg-[#141518] shrink-0">
        <div class="flex items-center gap-1.5 min-w-0">
          @if (renaming()) {
            <form (ngSubmit)="commitRename()" class="flex items-center gap-1.5">
              <input
                type="text"
                name="renameTitle"
                [ngModel]="renameDraft()"
                (ngModelChange)="renameDraft.set($event)"
                (keydown.escape)="renaming.set(false)"
                autofocus
                class="rounded-md border border-[#4a668a] bg-[#1b1c20] px-2.5 py-1 text-[12px] text-[#ececed] focus:outline-none w-[200px]"
              />
              <button
                type="submit"
                class="rounded-md bg-[#ececed] hover:bg-white px-2 py-1 text-[11px] font-medium text-[#111214] cursor-pointer"
              >
                Save
              </button>
              <button
                type="button"
                (click)="renaming.set(false)"
                class="rounded-md border border-[#2a2c33] px-2 py-1 text-[11px] text-[#8e9098] hover:text-[#ececed] cursor-pointer"
              >
                Cancel
              </button>
            </form>
          } @else {
            <select
              class="rounded-md border border-[#2a2c33] bg-[#1b1c20] px-2.5 py-1 text-[12px] text-[#e4e4e7] focus:border-[#4a668a] focus:outline-none max-w-[220px]"
              [ngModel]="orch.activeSessionId()"
              (ngModelChange)="orch.selectSession($event)"
            >
              @for (s of orch.sessions(); track s.id) {
                <option [value]="s.id">{{ s.title }}</option>
              }
            </select>
            <button
              type="button"
              (click)="startRename()"
              [disabled]="!orch.activeSessionId()"
              title="Rename session"
              class="rounded-md border border-[#2a2c33] bg-[#1b1c20] hover:bg-[#24262c] disabled:opacity-40 p-1.5 text-[#8e9098] hover:text-[#ececed] transition cursor-pointer"
            >
              <app-icon name="edit" [size]="11" />
            </button>
            <button
              type="button"
              (click)="deleteActiveSession()"
              [disabled]="!orch.activeSessionId()"
              title="Delete session"
              class="rounded-md border border-[#2a2c33] bg-[#1b1c20] hover:bg-[#24262c] disabled:opacity-40 p-1.5 text-[#8e9098] hover:text-[#f09595] transition cursor-pointer"
            >
              <app-icon name="trash" [size]="11" />
            </button>
            <button
              type="button"
              (click)="orch.createSession()"
              class="inline-flex items-center gap-1 rounded-md border border-[#2a2c33] bg-[#1b1c20] hover:bg-[#24262c] px-2 py-1 text-[11px] text-[#a1a3ab] hover:text-[#ececed] transition cursor-pointer"
            >
              <app-icon name="plus" [size]="11" />
              <span>New</span>
            </button>
          }
        </div>

        <div class="flex items-center gap-2 shrink-0">
          <button
            type="button"
            (click)="openNewChat.emit()"
            class="inline-flex items-center gap-1.5 rounded-md border border-[#2a2c33] bg-[#1b1c20] hover:bg-[#24262c] px-2.5 py-1 text-[11px] text-[#d4d4d8] transition cursor-pointer"
          >
            <app-icon name="orchestrator" [size]="12" class="text-[#60a5fa]" />
            <span>New Chat</span>
          </button>
          <button
            type="button"
            (click)="openNewTerminal.emit()"
            class="inline-flex items-center gap-1.5 rounded-md border border-[#2a2c33] bg-[#1b1c20] hover:bg-[#24262c] px-2.5 py-1 text-[11px] text-[#d4d4d8] transition cursor-pointer"
          >
            <app-icon name="shell" [size]="12" />
            <span>New Terminal</span>
          </button>
        </div>
      </div>

      <!-- Messages / Calm Empty State -->
      <div class="flex-1 overflow-y-auto px-4 py-6">
        @if (orch.messages().length === 0 && !orch.sending()) {
          <div class="h-full flex flex-col items-center justify-center max-w-lg mx-auto text-center">
            <div class="h-12 w-12 rounded-xl border border-[#282a30] bg-[#191a1e] flex items-center justify-center mb-4 shadow-lg">
              <img src="icon.svg" alt="Konsert" class="h-7 w-7" />
            </div>
            <h2 class="text-[18px] font-semibold tracking-tight text-[#ececed] mb-1">KONSERT</h2>
            <p class="text-[13px] text-[#8e9098] mb-6">
              Start a chat or terminal on any machine, or ask the Orchestrator to route work across your fleet as one logical system.
            </p>
            <div class="flex flex-wrap items-center justify-center gap-2 mb-6">
              <button
                type="button"
                (click)="openNewChat.emit()"
                class="inline-flex items-center gap-1.5 rounded-lg border border-[#2c2e36] bg-[#1b1c20] hover:bg-[#24262c] px-3.5 py-2 text-[12px] font-medium text-[#e4e4e7] transition cursor-pointer"
              >
                <app-icon name="orchestrator" [size]="13" class="text-[#60a5fa]" />
                <span>New Chat Session</span>
              </button>
              <button
                type="button"
                (click)="openNewTerminal.emit()"
                class="inline-flex items-center gap-1.5 rounded-lg border border-[#2c2e36] bg-[#1b1c20] hover:bg-[#24262c] px-3.5 py-2 text-[12px] font-medium text-[#e4e4e7] transition cursor-pointer"
              >
                <app-icon name="shell" [size]="13" />
                <span>New Terminal</span>
              </button>
            </div>
            <div class="w-full max-w-sm space-y-1.5 text-[12px] text-[#6d7078]">
              <button
                type="button"
                (click)="usePrompt('Check fleet_status and summarize online hosts, available agents, and capacity.')"
                class="w-full flex items-center justify-between rounded-lg px-3 py-1.5 hover:bg-[#1a1b1f] hover:text-[#b4b6be] transition cursor-pointer"
              >
                <span>Inspect fleet capacity</span>
                <span class="font-mono-ide text-[10px] text-[#5e6068]">fleet_status</span>
              </button>
              <button
                type="button"
                (click)="usePrompt('List recent tasks and summarize status, branches, and any failures.')"
                class="w-full flex items-center justify-between rounded-lg px-3 py-1.5 hover:bg-[#1a1b1f] hover:text-[#b4b6be] transition cursor-pointer"
              >
                <span>Review recent fleet jobs</span>
                <span class="font-mono-ide text-[10px] text-[#5e6068]">list_tasks</span>
              </button>
            </div>
          </div>
        } @else {
          <div class="max-w-3xl mx-auto space-y-3.5">
            @for (msg of orch.messages(); track msg.id) {
              @if (msg.role === 'user') {
                @let parsedUser = parseUserMsg(msg.content);
                <div class="flex justify-end">
                  <div class="max-w-[82%] rounded-2xl rounded-br-sm bg-[#23252b] border border-[#31333c] px-4 py-2.5 text-[13px] text-[#f4f4f5] leading-relaxed space-y-2">
                    @if (parsedUser.text) {
                      <div class="whitespace-pre-wrap">{{ parsedUser.text }}</div>
                    }
                    @if (parsedUser.files.length > 0) {
                      <div class="flex flex-wrap gap-1.5 pt-0.5">
                        @for (fname of parsedUser.files; track fname) {
                          <span class="inline-flex items-center gap-1 rounded-md border border-[#3a3d47] bg-[#191b20] px-2 py-0.5 text-[11px] font-mono-ide text-[#c8cad0]">
                            <app-icon name="paperclip" [size]="10" class="text-[#8e9098]" />
                            <span class="max-w-[180px] truncate">{{ fname }}</span>
                          </span>
                        }
                      </div>
                    }
                  </div>
                </div>
              } @else if (msg.role === 'assistant' && msg.content) {
                <div class="flex justify-start">
                  <div class="max-w-[92%] rounded-2xl rounded-bl-sm bg-[#16171a] border border-[#24262c] px-4 py-3">
                    <app-markdown-view [content]="msg.content" />
                  </div>
                </div>
              } @else if (msg.role === 'tool') {
                @let taskIds = extractTaskIds(msg.content);
                <details class="rounded-lg border border-[#24262c] bg-[#151619] px-3 py-2 text-[11px] font-mono-ide">
                  <summary class="cursor-pointer text-[#8e9098] hover:text-[#e4e4e7] flex items-center justify-between gap-2">
                    <span class="inline-flex items-center gap-2">
                      <app-icon name="checks" [size]="12" class="text-[#69b879]" />
                      <span>{{ parseToolName(msg) }}</span>
                    </span>
                    @if (taskIds.length > 0) {
                      <span class="flex items-center gap-1.5 flex-wrap">
                        @for (tid of taskIds; track tid) {
                          <button
                            type="button"
                            (click)="$event.preventDefault(); $event.stopPropagation(); openTaskTerminal(tid)"
                            class="inline-flex items-center gap-1 rounded border border-[#343844] bg-[#1d1f26] hover:bg-[#282b35] px-1.5 py-0.5 text-[10px] text-[#9ec2f5] transition cursor-pointer"
                          >
                            <app-icon name="shell" [size]="10" />
                            <span>{{ tid.slice(0, 8) }}</span>
                          </button>
                        }
                      </span>
                    }
                  </summary>
                  <pre class="mt-2 overflow-x-auto text-[11px] text-[#b8bac2] bg-[#111214] p-2.5 rounded border border-[#22242a]">{{ formatJSON(msg.content) }}</pre>
                </details>
              }
            }

            <!-- Live Tool Call Cards -->
            @for (card of orch.activeToolCards(); track card.call_id) {
              @let liveTaskIds = extractTaskIds(card.result);
              <details class="rounded-lg border border-[#343844] bg-[#16171b] px-3 py-2 text-[11px] font-mono-ide" open>
                <summary class="cursor-pointer flex items-center justify-between gap-2 text-[#d4d4d8]">
                  <span class="inline-flex items-center gap-1.5">
                    <app-icon name="orchestrator" [size]="12" class="text-[#60a5fa]" />
                    {{ card.name }}
                  </span>
                  <div class="flex items-center gap-2">
                    @for (tid of liveTaskIds; track tid) {
                      <button
                        type="button"
                        (click)="$event.preventDefault(); $event.stopPropagation(); openTaskTerminal(tid)"
                        class="inline-flex items-center gap-1 rounded border border-[#343844] bg-[#1d1f26] hover:bg-[#282b35] px-1.5 py-0.5 text-[10px] text-[#9ec2f5] transition cursor-pointer"
                      >
                        <app-icon name="shell" [size]="10" />
                        <span>Open {{ tid.slice(0, 8) }}</span>
                      </button>
                    }
                    <span
                      class="uppercase text-[10px]"
                      [class.text-[#d9a043]]="card.status === 'running'"
                      [class.text-[#69b879]]="card.status === 'completed'"
                      [class.text-[#e05d5d]]="card.status === 'error'"
                    >
                      {{ card.status }}
                    </span>
                  </div>
                </summary>
                @if (card.result) {
                  <pre class="mt-2 overflow-x-auto text-[11px] text-[#b8bac2] bg-[#111214] p-2 rounded">{{ formatObj(card.result) }}</pre>
                }
              </details>
            }

            <!-- Live Streaming Buffer -->
            @if (orch.streamingBuffer()) {
              <div class="flex justify-start">
                <div class="max-w-[92%] rounded-2xl rounded-bl-sm bg-[#16171a] border border-[#323642] px-4 py-3">
                  <app-markdown-view [content]="orch.streamingBuffer()" />
                </div>
              </div>
            } @else if (orch.sending()) {
              <div class="text-[12px] text-[#787a82] animate-pulse px-1">Orchestrator thinking...</div>
            }

            @if (orch.error()) {
              <div class="rounded-lg border border-[#e05d5d]/30 bg-[#e05d5d]/10 px-3 py-2 text-[12px] text-[#f09595]">
                {{ orch.error() }}
              </div>
            }
          </div>
        }
      </div>

      <!-- Orca-Style Floating Rounded Composer Card with Drag-and-Drop Attachments & Live Controls -->
      <div class="px-4 pb-4 pt-1 shrink-0">
        <div class="max-w-3xl mx-auto">
          <form
            (ngSubmit)="send()"
            (dragover)="onDragOver($event)"
            (dragleave)="onDragLeave($event)"
            (drop)="onDrop($event)"
            class="rounded-2xl border bg-[#191a1e] focus-within:border-[#424654] transition shadow-lg overflow-hidden relative"
            [class.border-[#4a668a]]="dragOver()"
            [class.border-[#2b2d34]]="!dragOver()"
          >
            @if (dragOver()) {
              <div class="absolute inset-0 z-10 bg-[#191a1e]/92 backdrop-blur-sm flex items-center justify-center pointer-events-none">
                <div class="flex items-center gap-2 rounded-lg border border-dashed border-[#60a5fa]/60 px-4 py-2 text-[12px] text-[#9ec2f5]">
                  <app-icon name="paperclip" [size]="14" />
                  <span>Drop files to attach to host scratch directory</span>
                </div>
              </div>
            }

            <!-- Attachment Chips Row -->
            @if (attachments().length > 0 || uploadingCount() > 0 || uploadError()) {
              <div class="flex flex-wrap items-center gap-1.5 px-3.5 pt-2.5">
                @for (att of attachments(); track att.remotePath) {
                  <span
                    class="inline-flex items-center gap-1.5 rounded-md border border-[#30333d] bg-[#141519] px-2 py-0.5 text-[11px] text-[#d4d4d8]"
                    [title]="att.remotePath"
                  >
                    <app-icon name="paperclip" [size]="10" class="text-[#8e9098]" />
                    <span class="max-w-[160px] truncate">{{ att.filename }}</span>
                    <span class="text-[10px] font-mono-ide text-[#6d7078]">{{ formatBytes(att.size) }}</span>
                    <button
                      type="button"
                      (click)="removeAttachment(att.remotePath)"
                      class="text-[#6d7078] hover:text-[#f09595] cursor-pointer"
                    >
                      <app-icon name="close" [size]="10" />
                    </button>
                  </span>
                }
                @if (uploadingCount() > 0) {
                  <span class="inline-flex items-center gap-1.5 rounded-md border border-[#30333d] bg-[#141519] px-2 py-0.5 text-[11px] text-[#8e9098]">
                    <span class="h-2.5 w-2.5 rounded-full border-[1.5px] border-[#60a5fa] border-t-transparent ks-ring-spin"></span>
                    <span>Uploading {{ uploadingCount() }}...</span>
                  </span>
                }
                @if (uploadError()) {
                  <span class="text-[11px] text-[#f09595]">{{ uploadError() }}</span>
                }
              </div>
            }

            <textarea
              rows="2"
              name="prompt"
              [ngModel]="draft()"
              (ngModelChange)="draft.set($event)"
              (keydown.enter)="onEnter($event)"
              placeholder="Ask anything — inspect hosts, dispatch parallel agent jobs, attach files, or steer running tasks..."
              class="w-full resize-none bg-transparent px-4 pt-3 pb-1.5 text-[13px] text-[#ececed] placeholder-[#686a72] focus:outline-none"
            ></textarea>

            <input
              #fileInput
              type="file"
              multiple
              class="hidden"
              (change)="onFilesPicked($event)"
            />

            <div class="flex flex-wrap items-center justify-between gap-2 px-3 pb-2.5 pt-1">
              <!-- Left Controls: Attach + Permission Mode + Machine / Agent -->
              <div class="flex flex-wrap items-center gap-1.5">
                <button
                  type="button"
                  (click)="triggerFilePicker()"
                  title="Attach local files (copied to target host OS scratch dir)"
                  class="h-6 w-6 rounded-md hover:bg-[#26282e] flex items-center justify-center text-[#8e9098] hover:text-[#ececed] transition cursor-pointer"
                >
                  <app-icon name="paperclip" [size]="13" />
                </button>

                <!-- Live Permission Mode Selector -->
                <select
                  name="composerPerm"
                  [ngModel]="fleet.settings().default_permission_mode"
                  (ngModelChange)="onPermissionModeChange($event)"
                  title="Default permission mode for fleet dispatch"
                  class="rounded-md border border-transparent hover:border-[#2e3039] px-1.5 py-0.5 text-[11px] focus:outline-none cursor-pointer transition"
                  [class.bg-[#d9a043]/10]="fleet.settings().default_permission_mode === 'full_access'"
                  [class.text-[#d9a043]]="fleet.settings().default_permission_mode === 'full_access'"
                  [class.bg-[#22242b]]="fleet.settings().default_permission_mode !== 'full_access'"
                  [class.text-[#98dba5]]="fleet.settings().default_permission_mode !== 'full_access'"
                >
                  <option value="full_access" class="bg-[#18191d] text-[#ececed]">Full access</option>
                  <option value="sandboxed" class="bg-[#18191d] text-[#ececed]">Sandboxed</option>
                </select>

                <!-- Live Machine Selector (default localhost) -->
                <select
                  name="composerMachine"
                  [ngModel]="selectedMachineId()"
                  (ngModelChange)="onComposerMachineChange($event)"
                  title="Target machine (defaults to localhost)"
                  class="rounded-md border border-[#2a2c34] bg-[#141519] hover:border-[#393c47] px-1.5 py-0.5 text-[11px] text-[#c8cad0] max-w-[125px] focus:outline-none cursor-pointer"
                >
                  @if (fleet.workers().length === 0) {
                    <option value="">localhost</option>
                  }
                  @for (w of fleet.workers(); track w.id) {
                    <option [value]="w.id">{{ w.name }}</option>
                  }
                </select>

                <!-- Live Agent Selector (default -> last used -> guess) -->
                <select
                  name="composerAgent"
                  [ngModel]="selectedAgent()"
                  (ngModelChange)="onComposerAgentChange($event)"
                  title="Preferred coding agent"
                  class="rounded-md border border-[#2a2c34] bg-[#141519] hover:border-[#393c47] px-1.5 py-0.5 text-[11px] text-[#c8cad0] focus:outline-none cursor-pointer"
                >
                  @for (a of availableAgents(); track a) {
                    <option [value]="a">{{ a }}</option>
                  }
                </select>
              </div>

              <!-- Right Controls: Live Model Selector + Send -->
              <div class="flex items-center gap-2">
                <input
                  type="text"
                  name="composerModel"
                  list="ks-composer-models"
                  [ngModel]="selectedModel()"
                  (ngModelChange)="selectedModel.set($event)"
                  (blur)="commitModelChange()"
                  (keydown.enter)="$event.stopPropagation(); commitModelChange()"
                  placeholder="model"
                  title="Active Orchestrator / session model"
                  class="w-[148px] rounded-md border border-transparent hover:border-[#2d3039] focus:border-[#4a668a] bg-transparent focus:bg-[#141519] px-2 py-0.5 text-right text-[11px] font-mono-ide text-[#a1a3ab] focus:text-[#ececed] focus:outline-none transition"
                />
                <datalist id="ks-composer-models">
                  @for (m of modelChoices(); track m) {
                    <option [value]="m"></option>
                  }
                </datalist>

                <button
                  type="submit"
                  [disabled]="orch.sending() || uploadingCount() > 0 || (!draft().trim() && attachments().length === 0) || !orch.activeSessionId()"
                  title="Send message"
                  class="h-7 w-7 rounded-full bg-[#d4d4d8] hover:bg-white disabled:opacity-30 flex items-center justify-center text-[#111214] transition cursor-pointer shrink-0"
                >
                  <app-icon name="send-up" [size]="14" />
                </button>
              </div>
            </div>
          </form>

          <!-- Subtle Orca-style Context Breadcrumb Line -->
          <div class="flex items-center justify-between px-2 pt-1.5 text-[11px] text-[#6d7078]">
            <div class="flex items-center gap-3">
              <span class="inline-flex items-center gap-1">
                <app-icon name="server" [size]="11" />
                {{ fleet.onlineWorkersCount() }}/{{ fleet.workers().length }} hosts online
              </span>
              <span class="inline-flex items-center gap-1">
                <app-icon name="activity" [size]="11" />
                {{ fleet.activeTasksCount() }} active
              </span>
            </div>
            <span class="font-mono-ide text-[10px]">MCP · /mcp</span>
          </div>
        </div>
      </div>
    </div>
  `,
})
export class OrchestratorChatComponent implements OnInit {
  @ViewChild('fileInput') fileInput?: ElementRef<HTMLInputElement>;

  readonly openNewChat = output<void>();
  readonly openNewTerminal = output<void>();

  readonly orch = inject(OrchestratorStore);
  readonly auth = inject(AuthService);
  readonly fleet = inject(FleetStore);
  private readonly tabs = inject(TabsStore);
  private readonly api = inject(ApiClientService);
  private readonly socket = inject(KonsertSocketService);
  private readonly notifications = inject(NotificationService);

  readonly draft = signal('');
  readonly renaming = signal(false);
  readonly renameDraft = signal('');

  // Composer Machine / Agent / Model selectors
  readonly selectedMachineId = signal('');
  readonly selectedAgent = signal('claude');
  readonly selectedModel = signal('');
  private defaultsSeeded = false;

  // Local file attachments uploaded to target host's OS scratch dir
  readonly attachments = signal<UploadedAttachment[]>([]);
  readonly uploadingCount = signal(0);
  readonly uploadError = signal('');
  readonly dragOver = signal(false);

  readonly availableAgents = computed(() => {
    const w = this.fleet.workersById()[this.selectedMachineId()];
    const detected = (w?.agents ?? []).filter((a) => a !== 'shell');
    return Array.from(new Set([...detected, 'agy', 'claude', 'codex', 'grok', 'mock']));
  });

  readonly modelChoices = computed(() => {
    const base = [
      this.auth.model(),
      this.fleet.settings().llm_model,
      'gpt-4o',
      'o4-mini',
      'claude-sonnet-4-5',
      'claude-opus-4-1',
      'gemini-2.5-pro',
      'gemini-2.5-flash',
      'grok-code-fast-1',
    ].filter(Boolean);
    return Array.from(new Set(base));
  });

  constructor() {
    effect(() => {
      const workers = this.fleet.workers();
      const settings = this.fleet.settings();
      if (!this.defaultsSeeded && (workers.length > 0 || settings.llm_model)) {
        const res = this.fleet.resolveSessionDefaults();
        if (res.machine_id) this.selectedMachineId.set(res.machine_id);
        if (res.agent) this.selectedAgent.set(res.agent);
        this.selectedModel.set(settings.llm_model || res.model || this.auth.model() || 'gpt-4o');
        this.defaultsSeeded = true;
      }
    });
  }

  ngOnInit(): void {
    void this.orch.init();
  }

  startRename(): void {
    const cur = this.orch.sessions().find((s) => s.id === this.orch.activeSessionId());
    this.renameDraft.set(cur?.title || '');
    this.renaming.set(true);
  }

  async commitRename(): Promise<void> {
    const sid = this.orch.activeSessionId();
    const title = this.renameDraft().trim();
    this.renaming.set(false);
    if (!sid || !title) return;
    try {
      await this.orch.renameSession(sid, title);
    } catch {
      // ignore
    }
  }

  async deleteActiveSession(): Promise<void> {
    const sid = this.orch.activeSessionId();
    if (!sid) return;
    const cur = this.orch.sessions().find((s) => s.id === sid);
    if (!window.confirm(`Delete chat session "${cur?.title || sid}"?`)) return;
    try {
      await this.orch.deleteSession(sid);
    } catch {
      // ignore
    }
  }

  async onPermissionModeChange(mode: PermissionMode): Promise<void> {
    try {
      const updated = await this.api.updateSettings({ default_permission_mode: mode });
      this.fleet.settings.set(updated);
    } catch {
      // ignore
    }
  }

  onComposerMachineChange(machineId: string): void {
    this.selectedMachineId.set(machineId);
    const resolved = this.fleet.resolveSessionDefaults(machineId);
    if (resolved.agent) this.selectedAgent.set(resolved.agent);
  }

  onComposerAgentChange(agent: string): void {
    this.selectedAgent.set(agent);
    if (!this.selectedModel().trim()) {
      this.selectedModel.set(guessModelForAgent(agent, this.fleet.settings().llm_model));
    }
  }

  async commitModelChange(): Promise<void> {
    const model = this.selectedModel().trim();
    if (!model || model === this.fleet.settings().llm_model) return;
    try {
      const updated = await this.api.updateSettings({ llm_model: model });
      this.fleet.settings.set(updated);
      this.auth.model.set(updated.llm_model);
    } catch {
      // ignore
    }
  }

  triggerFilePicker(): void {
    this.fileInput?.nativeElement.click();
  }

  onFilesPicked(ev: Event): void {
    const input = ev.target as HTMLInputElement;
    if (input.files && input.files.length > 0) {
      void this.uploadFiles(Array.from(input.files));
      input.value = '';
    }
  }

  onDragOver(ev: DragEvent): void {
    ev.preventDefault();
    this.dragOver.set(true);
  }

  onDragLeave(ev: DragEvent): void {
    ev.preventDefault();
    this.dragOver.set(false);
  }

  onDrop(ev: DragEvent): void {
    ev.preventDefault();
    this.dragOver.set(false);
    const files = ev.dataTransfer?.files;
    if (files && files.length > 0) {
      void this.uploadFiles(Array.from(files));
    }
  }

  private async uploadFiles(files: File[]): Promise<void> {
    this.uploadError.set('');
    this.uploadingCount.update((n) => n + files.length);
    for (const file of files) {
      try {
        const res = await this.api.uploadAttachment(file, {
          workerId: this.selectedMachineId() || undefined,
          sessionId: this.orch.activeSessionId() || undefined,
        });
        this.attachments.update((list) => [
          ...list,
          {
            filename: res.filename || file.name,
            remotePath: res.remote_path,
            size: res.size || file.size,
            workerId: res.worker_id,
          },
        ]);
      } catch (err) {
        this.uploadError.set(err instanceof Error ? err.message : `Upload failed for ${file.name}`);
      } finally {
        this.uploadingCount.update((n) => Math.max(0, n - 1));
      }
    }
  }

  removeAttachment(remotePath: string): void {
    this.attachments.update((list) => list.filter((a) => a.remotePath !== remotePath));
  }

  formatBytes(size: number): string {
    if (size < 1024) return `${size}B`;
    if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)}KB`;
    return `${(size / (1024 * 1024)).toFixed(1)}MB`;
  }

  usePrompt(text: string): void {
    this.draft.set(text);
    this.send();
  }

  onEnter(event: Event): void {
    const ke = event as KeyboardEvent;
    if (!ke.shiftKey) {
      ke.preventDefault();
      this.send();
    }
  }

  send(): void {
    const text = this.draft().trim();
    const attached = this.attachments();
    const sid = this.orch.activeSessionId();
    if ((!text && attached.length === 0) || !sid || this.orch.sending() || this.uploadingCount() > 0) return;

    let payload = text;
    if (attached.length > 0) {
      const pathsBlock = attached.map((a) => `- ${a.remotePath}`).join('\n');
      payload = `${text}${ATTACHMENT_MARKER}${pathsBlock}`;
    }

    this.draft.set('');
    this.attachments.set([]);
    this.uploadError.set('');
    this.socket.sendChatMessage(sid, payload);
  }

  parseUserMsg(content: string): { text: string; files: string[] } {
    return parseUserMessageWithAttachments(content);
  }

  extractTaskIds(raw: unknown): string[] {
    return extractTaskIdsFromToolPayload(raw);
  }

  async openTaskTerminal(taskId: string): Promise<void> {
    let task = this.fleet.tasksById()[taskId];
    if (!task) {
      await this.fleet.refresh();
      task = this.fleet.tasksById()[taskId];
    }
    if (task) {
      const worker = this.fleet.workersById()[task.worker_id];
      this.tabs.openTaskTab(task, worker?.name, true);
      this.notifications.markRead(task.id);
    }
  }

  parseToolName(msg: ChatMessage): string {
    if (!msg.tool_json) return 'tool';
    try {
      const parsed = JSON.parse(msg.tool_json) as { name?: string };
      return parsed.name || 'tool';
    } catch {
      return 'tool';
    }
  }

  formatJSON(raw: string): string {
    try {
      return JSON.stringify(JSON.parse(raw), null, 2);
    } catch {
      return raw;
    }
  }

  formatObj(obj: unknown): string {
    try {
      return JSON.stringify(obj, null, 2);
    } catch {
      return String(obj);
    }
  }
}

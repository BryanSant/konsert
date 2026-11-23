import { Component, OnInit, inject, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ChatMessage } from '../../core/models/konsert.models';
import { AuthService } from '../../core/services/auth.service';
import { KonsertSocketService } from '../../core/services/konsert-socket.service';
import { FleetStore } from '../../core/state/fleet.store';
import { OrchestratorStore } from '../../core/state/orchestrator.store';
import { IconComponent } from '../../shared/icon.component';

@Component({
  selector: 'app-orchestrator-chat',
  standalone: true,
  imports: [FormsModule, IconComponent],
  template: `
    <div class="flex flex-col h-full w-full bg-[#111214]">
      <!-- Subtle Top Bar -->
      <div class="flex items-center justify-between gap-2 px-4 py-2 border-b border-[#22242a] bg-[#141518] shrink-0">
        <div class="flex items-center gap-2 min-w-0">
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
            (click)="orch.createSession()"
            class="inline-flex items-center gap-1 rounded-md border border-[#2a2c33] bg-[#1b1c20] hover:bg-[#24262c] px-2 py-1 text-[11px] text-[#a1a3ab] hover:text-[#ececed] transition cursor-pointer"
          >
            <app-icon name="plus" [size]="11" />
            <span>New</span>
          </button>
        </div>

        <div class="flex items-center gap-2">
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
                <div class="flex justify-end">
                  <div class="max-w-[82%] rounded-2xl rounded-br-sm bg-[#23252b] border border-[#31333c] px-4 py-2.5 text-[13px] text-[#f4f4f5] whitespace-pre-wrap leading-relaxed">
                    {{ msg.content }}
                  </div>
                </div>
              } @else if (msg.role === 'assistant' && msg.content) {
                <div class="flex justify-start">
                  <div class="max-w-[92%] rounded-2xl rounded-bl-sm bg-[#16171a] border border-[#24262c] px-4 py-3 text-[13px] text-[#e4e4e7] whitespace-pre-wrap leading-relaxed">
                    {{ msg.content }}
                  </div>
                </div>
              } @else if (msg.role === 'tool') {
                <details class="rounded-lg border border-[#24262c] bg-[#151619] px-3 py-2 text-[11px] font-mono-ide">
                  <summary class="cursor-pointer text-[#8e9098] hover:text-[#e4e4e7] flex items-center gap-2">
                    <app-icon name="checks" [size]="12" class="text-[#69b879]" />
                    <span>{{ parseToolName(msg) }}</span>
                  </summary>
                  <pre class="mt-2 overflow-x-auto text-[11px] text-[#b8bac2] bg-[#111214] p-2.5 rounded border border-[#22242a]">{{ formatJSON(msg.content) }}</pre>
                </details>
              }
            }

            <!-- Live Tool Call Cards -->
            @for (card of orch.activeToolCards(); track card.call_id) {
              <details class="rounded-lg border border-[#343844] bg-[#16171b] px-3 py-2 text-[11px] font-mono-ide" open>
                <summary class="cursor-pointer flex items-center justify-between text-[#d4d4d8]">
                  <span class="inline-flex items-center gap-1.5">
                    <app-icon name="orchestrator" [size]="12" class="text-[#60a5fa]" />
                    {{ card.name }}
                  </span>
                  <span
                    class="uppercase text-[10px]"
                    [class.text-[#d9a043]]="card.status === 'running'"
                    [class.text-[#69b879]]="card.status === 'completed'"
                    [class.text-[#e05d5d]]="card.status === 'error'"
                  >
                    {{ card.status }}
                  </span>
                </summary>
                @if (card.result) {
                  <pre class="mt-2 overflow-x-auto text-[11px] text-[#b8bac2] bg-[#111214] p-2 rounded">{{ formatObj(card.result) }}</pre>
                }
              </details>
            }

            <!-- Live Streaming Buffer -->
            @if (orch.streamingBuffer()) {
              <div class="flex justify-start">
                <div class="max-w-[92%] rounded-2xl rounded-bl-sm bg-[#16171a] border border-[#323642] px-4 py-3 text-[13px] text-[#ececed] whitespace-pre-wrap leading-relaxed">
                  {{ orch.streamingBuffer() }}<span class="inline-block w-1.5 h-3.5 bg-[#8e9098] ml-0.5 animate-pulse align-middle"></span>
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

      <!-- Orca-Style Floating Rounded Composer Card -->
      <div class="px-4 pb-4 pt-1 shrink-0">
        <div class="max-w-3xl mx-auto">
          <form
            (ngSubmit)="send()"
            class="rounded-2xl border border-[#2b2d34] bg-[#191a1e] focus-within:border-[#424654] transition shadow-lg overflow-hidden"
          >
            <textarea
              rows="2"
              name="prompt"
              [ngModel]="draft()"
              (ngModelChange)="draft.set($event)"
              (keydown.enter)="onEnter($event)"
              placeholder="Ask anything — inspect hosts, dispatch parallel agent jobs, or steer running tasks..."
              class="w-full resize-none bg-transparent px-4 pt-3 pb-1.5 text-[13px] text-[#ececed] placeholder-[#686a72] focus:outline-none"
            ></textarea>

            <div class="flex items-center justify-between px-3 pb-2.5 pt-1">
              <div class="flex items-center gap-2">
                <button
                  type="button"
                  (click)="openNewChat.emit()"
                  title="New chat session"
                  class="h-6 w-6 rounded-md hover:bg-[#26282e] flex items-center justify-center text-[#8e9098] hover:text-[#ececed] transition cursor-pointer"
                >
                  <app-icon name="plus" [size]="14" />
                </button>
                <span class="inline-flex items-center gap-1 rounded-md px-2 py-0.5 text-[11px] text-[#d9a043] bg-[#d9a043]/10">
                  <app-icon name="shield" [size]="12" />
                  <span>Full access</span>
                </span>
              </div>

              <div class="flex items-center gap-2.5">
                <span class="text-[11px] text-[#8e9098] font-mono-ide">
                  {{ auth.model() || auth.provider() }}
                </span>
                <button
                  type="submit"
                  [disabled]="orch.sending() || !draft().trim() || !orch.activeSessionId()"
                  title="Send message"
                  class="h-7 w-7 rounded-full bg-[#d4d4d8] hover:bg-white disabled:opacity-30 flex items-center justify-center text-[#111214] transition cursor-pointer"
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
  readonly openNewChat = output<void>();
  readonly openNewTerminal = output<void>();

  readonly orch = inject(OrchestratorStore);
  readonly auth = inject(AuthService);
  readonly fleet = inject(FleetStore);
  private readonly socket = inject(KonsertSocketService);

  readonly draft = signal('');

  ngOnInit(): void {
    void this.orch.init();
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
    const sid = this.orch.activeSessionId();
    if (!text || !sid || this.orch.sending()) return;
    this.draft.set('');
    this.socket.sendChatMessage(sid, text);
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

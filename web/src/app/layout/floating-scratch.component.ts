import { Component, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiClientService } from '../core/services/api-client.service';
import { FleetStore } from '../core/state/fleet.store';
import { TabsStore } from '../core/state/tabs.store';
import { XtermPaneComponent } from '../features/terminal/xterm-pane.component';
import { IconComponent } from '../shared/icon.component';

@Component({
  selector: 'app-floating-scratch',
  standalone: true,
  imports: [FormsModule, IconComponent, XtermPaneComponent],
  template: `
    <!-- Bottom-Right Floating Trigger Button (Orca Image 0 & 1 style) -->
    <button
      type="button"
      (click)="tabs.toggleScratch()"
      title="Floating Scratch Workspace (⌘J)"
      class="fixed bottom-10 right-4 z-30 h-9 w-9 rounded-lg border shadow-xl flex items-center justify-center transition-all duration-150 active:scale-95 cursor-pointer backdrop-blur-md"
      [class.border-[#4a668a]]="tabs.scratchOpen() || !!tabs.scratchTaskId()"
      [class.bg-[#20232b]/95]="tabs.scratchOpen()"
      [class.text-[#ececed]]="tabs.scratchOpen()"
      [class.border-[#2c2e36]]="!tabs.scratchOpen()"
      [class.bg-[#1a1b1f]/90]="!tabs.scratchOpen()"
      [class.text-[#9a9ca4]]="!tabs.scratchOpen()"
      [class.hover:text-[#ececed]]="!tabs.scratchOpen()"
      [class.hover:bg-[#23252b]]="!tabs.scratchOpen()"
    >
      <app-icon name="scratch" [size]="16" />
      @if (tabs.scratchTaskId() && !tabs.scratchOpen()) {
        <span class="absolute -top-1 -right-1 h-2.5 w-2.5 rounded-full bg-[#69b879] border-2 border-[#16171a]"></span>
      }
    </button>

    <!-- Floating Scratch Workspace Window (stays mounted when minimized so PTY persists, with smooth springy reveal!) -->
    <div
      class="fixed bottom-12 right-4 z-40 w-[620px] h-[400px] max-w-[94vw] max-h-[75vh] rounded-xl border border-[#32353f] bg-[#16171b]/95 backdrop-blur-xl shadow-2xl flex flex-col overflow-hidden origin-bottom-right transition-all duration-200 ease-[cubic-bezier(0.16,1,0.3,1)]"
      [class.opacity-100]="tabs.scratchOpen()"
      [class.scale-100]="tabs.scratchOpen()"
      [class.translate-y-0]="tabs.scratchOpen()"
      [class.pointer-events-auto]="tabs.scratchOpen()"
      [class.opacity-0]="!tabs.scratchOpen()"
      [class.scale-95]="!tabs.scratchOpen()"
      [class.translate-y-2]="!tabs.scratchOpen()"
      [class.pointer-events-none]="!tabs.scratchOpen()"
    >
      <!-- Scratch Header -->
      <div class="h-9 flex items-center justify-between px-3 border-b border-[#24262c] bg-[#18191d] shrink-0">
        <div class="flex items-center gap-2 text-[12px]">
          <app-icon name="scratch" [size]="13" class="text-[#8e9098]" />
          <span class="font-medium text-[#ececed]">Scratch Workspace</span>
          @if (activeScratchTask(); as st) {
            <span class="font-mono-ide text-[10px] text-[#787a82]">
              {{ fleet.workersById()[st.worker_id]?.name || st.worker_id }} · {{ st.agent }}
            </span>
          }
        </div>

        <div class="flex items-center gap-1.5">
          @if (tabs.scratchTaskId()) {
            <button
              type="button"
              (click)="resetScratch()"
              class="rounded px-2 py-0.5 text-[11px] text-[#8e9098] hover:text-[#f09595] hover:bg-[#24262c] transition cursor-pointer"
            >
              Close Session
            </button>
          }
          <button
            type="button"
            (click)="tabs.scratchOpen.set(false)"
            title="Minimize (keeps session running)"
            class="rounded p-1 text-[#8e9098] hover:text-[#ececed] hover:bg-[#24262c] transition cursor-pointer"
          >
            <app-icon name="close" [size]="13" />
          </button>
        </div>
      </div>

      <!-- Body: Either Launcher or Live Embedded Terminal -->
      @if (!tabs.scratchTaskId()) {
        <div class="flex-1 flex flex-col justify-center p-5 space-y-4">
          <div>
            <h3 class="text-[13px] font-semibold text-[#ececed] mb-1">Out-of-Band Scratchpad</h3>
            <p class="text-[12px] text-[#8e9098]">
              Spin up a quick interactive shell or ad-hoc agent on any connected host without disturbing your main workspace tabs.
            </p>
          </div>

          <form (ngSubmit)="launchScratch()" class="space-y-3 text-[12px]">
            <div class="grid grid-cols-2 gap-2.5">
              <div>
                <label for="floating-scratch-machine" class="block text-[11px] text-[#787a82] mb-1">Machine</label>
                <select id="floating-scratch-machine"
                  name="scratchWorker"
                  [ngModel]="selectedWorkerId()"
                  (ngModelChange)="selectedWorkerId.set($event)"
                  class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-1.5 text-[#ececed]"
                >
                  <option value="">Auto (first online)</option>
                  @for (w of onlineWorkers(); track w.id) {
                    <option [value]="w.id">{{ w.name }} ({{ w.os }})</option>
                  }
                </select>
              </div>

              <div>
                <label for="floating-scratch-session-type" class="block text-[11px] text-[#787a82] mb-1">Session Type</label>
                <select id="floating-scratch-session-type"
                  name="scratchAgent"
                  [ngModel]="selectedAgent()"
                  (ngModelChange)="selectedAgent.set($event)"
                  class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-1.5 text-[#ececed]"
                >
                  <option value="shell">shell (zsh / bash)</option>
                  <option value="claude">claude</option>
                  <option value="agy">agy</option>
                  <option value="grok">grok</option>
                  <option value="mock">mock</option>
                </select>
              </div>
            </div>

            <div>
              <label for="floating-scratch-directory" class="block text-[11px] text-[#787a82] mb-1">Directory</label>
              <input id="floating-scratch-directory"
                type="text"
                name="scratchDir"
                [ngModel]="workdir()"
                (ngModelChange)="workdir.set($event)"
                placeholder="~"
                class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-1.5 font-mono-ide text-[#ececed]"
              />
            </div>

            @if (error()) {
              <div class="text-[11px] text-[#f09595]">{{ error() }}</div>
            }

            <button
              type="submit"
              [disabled]="launching() || onlineWorkers().length === 0"
              class="w-full rounded-lg bg-[#ececed] hover:bg-white disabled:opacity-40 py-2 text-[12px] font-medium text-[#111214] transition cursor-pointer"
            >
              {{ launching() ? 'Starting session...' : onlineWorkers().length === 0 ? 'No online hosts available' : 'Launch Scratch Session' }}
            </button>
          </form>
        </div>
      } @else {
        <div class="flex-1 min-h-0">
          <app-xterm-pane
            [taskId]="tabs.scratchTaskId()!"
            [visible]="tabs.scratchOpen()"
            [embedded]="true"
          />
        </div>
      }
    </div>
  `,
})
export class FloatingScratchComponent {
  readonly tabs = inject(TabsStore);
  readonly fleet = inject(FleetStore);
  private readonly api = inject(ApiClientService);

  readonly selectedWorkerId = signal('');
  readonly selectedAgent = signal('shell');
  readonly workdir = signal('~');
  readonly launching = signal(false);
  readonly error = signal('');

  readonly onlineWorkers = computed(() =>
    this.fleet.workers().filter((w) => w.status === 'online')
  );

  readonly activeScratchTask = computed(() => {
    const id = this.tabs.scratchTaskId();
    return id ? this.fleet.tasksById()[id] : undefined;
  });

  async launchScratch(): Promise<void> {
    this.launching.set(true);
    this.error.set('');
    try {
      const res = await this.api.dispatchTask({
        worker_selector: this.selectedWorkerId() ? { worker_id: this.selectedWorkerId() } : undefined,
        agent: this.selectedAgent(),
        repo_path: this.workdir().trim() || '~',
        mode: 'interactive',
        origin: 'scratch',
        permission_mode: 'full_access',
      });
      await this.fleet.refresh();
      this.tabs.scratchTaskId.set(res.task_id);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Failed to start scratch session');
    } finally {
      this.launching.set(false);
    }
  }

  async resetScratch(): Promise<void> {
    const id = this.tabs.scratchTaskId();
    this.tabs.scratchTaskId.set(null);
    if (id) {
      try {
        await this.api.cancelTask(id);
      } catch {
        // ignore
      }
    }
  }
}

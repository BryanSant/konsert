import {
  Component,
  ElementRef,
  OnDestroy,
  afterNextRender,
  computed,
  effect,
  inject,
  input,
  signal,
  untracked,
  viewChild,
} from '@angular/core';
import { FitAddon } from '@xterm/addon-fit';
import { WebLinksAddon } from '@xterm/addon-web-links';
import { WebglAddon } from '@xterm/addon-webgl';
import { Terminal } from '@xterm/xterm';
import { FRAME_PTY_DATA, FRAME_PTY_SNAPSHOT } from '../../core/models/konsert.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { KonsertSocketService } from '../../core/services/konsert-socket.service';
import { FleetStore } from '../../core/state/fleet.store';
import { TabsStore } from '../../core/state/tabs.store';
import { IconComponent } from '../../shared/icon.component';

@Component({
  selector: 'app-xterm-pane',
  standalone: true,
  imports: [IconComponent],
  template: `
    <div class="h-full w-full flex flex-col" [class.p-2]="!embedded()" [class.bg-[#111214]]="true">
      <!-- Herdr-style Framed Terminal Container -->
      <div
        class="flex flex-col flex-1 min-h-0 w-full bg-[#111214] overflow-hidden transition-colors"
        [class.rounded-lg]="!embedded()"
        [class.border]="!embedded()"
        [class.border-[#4a668a]]="!embedded() && interactiveMode() && task()?.status !== 'waiting_input'"
        [class.border-[#d9a043]/70]="!embedded() && task()?.status === 'waiting_input'"
        [class.border-[#26282e]]="!embedded() && !interactiveMode() && task()?.status !== 'waiting_input'"
      >
        <!-- Whisper-slim Frame Header (32px, Herdr style) -->
        @if (!embedded()) {
          <div class="h-8 flex items-center justify-between gap-2 px-3 border-b border-[#22242a] bg-[#151619] shrink-0">
            <div class="flex items-center gap-2 min-w-0 text-[12px]">
              <app-icon [name]="task()?.agent || 'shell'" [size]="13" />
              <span class="font-medium text-[#e4e4e7] shrink-0">{{ displayAgent() }}</span>
              <span class="font-mono-ide text-[11px] text-[#8a8c94] truncate">
                {{ task()?.effective_dir || task()?.workdir }}
              </span>
              @if (task()?.branch) {
                <span class="hidden sm:inline-flex items-center gap-1 text-[11px] font-mono-ide text-[#8e9098] shrink-0">
                  <app-icon name="git-branch" [size]="11" />
                  {{ task()?.branch }}
                </span>
              }
              @if (task()?.model) {
                <span class="hidden md:inline-block rounded px-1.5 py-0.5 text-[10px] font-mono-ide bg-white/[0.05] text-[#9a9ca4] shrink-0">
                  {{ task()?.model }}
                </span>
              }
            </div>

            <div class="flex items-center gap-1.5 shrink-0">
              <!-- Live Status Pill -->
              @if (task()?.status === 'waiting_input') {
                <span class="inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-[10px] font-medium bg-[#d9a043]/15 text-[#e6b763]">
                  <span class="h-1.5 w-1.5 rounded-full bg-[#d9a043] animate-pulse"></span>
                  Needs you
                </span>
              } @else if (task()?.status === 'completed' || task()?.status === 'failed' || task()?.status === 'canceled') {
                <span
                  class="rounded px-1.5 py-0.5 text-[10px] font-mono-ide"
                  [class.text-[#69b879]]="task()?.status === 'completed'"
                  [class.text-[#e05d5d]]="task()?.status === 'failed'"
                  [class.text-[#8e9098]]="task()?.status === 'canceled'"
                >
                  {{ task()?.status }}
                </span>
              }

              <!-- Read-Only vs Interactive Segmented Toggle -->
              <button
                type="button"
                (click)="toggleInteractive()"
                class="inline-flex items-center gap-1.5 rounded-md px-2 py-0.5 text-[11px] font-medium border transition cursor-pointer"
                [class.border-[#4a668a]]="interactiveMode()"
                [class.bg-[#4a668a]/20]="interactiveMode()"
                [class.text-[#d8e6f7]]="interactiveMode()"
                [class.border-[#2a2c33]]="!interactiveMode()"
                [class.bg-[#1d1e22]]="!interactiveMode()"
                [class.text-[#8e9098]]="!interactiveMode()"
                [class.hover:text-[#ececed]]="!interactiveMode()"
              >
                <app-icon [name]="interactiveMode() ? 'unlock' : 'lock'" [size]="11" />
                <span>{{ interactiveMode() ? 'Interactive' : 'Read-Only' }}</span>
              </button>

              @if (isActiveTask()) {
                <button
                  type="button"
                  (click)="cancelTask()"
                  title="Stop remote process"
                  class="rounded-md border border-[#2a2c33] hover:border-[#e05d5d]/50 hover:bg-[#e05d5d]/10 px-2 py-0.5 text-[11px] text-[#8e9098] hover:text-[#f09595] transition cursor-pointer"
                >
                  Stop
                </button>
              }
            </div>
          </div>
        }

        <!-- Subtle Read-Only Toast Hint -->
        @if (readOnlyToast()) {
          <div class="px-3 py-1.5 bg-[#1e1d18] border-b border-[#d9a043]/30 text-[11px] text-[#e6b763] flex items-center justify-between shrink-0">
            <span>Read-Only observation mode — click <strong>Read-Only</strong> to take interactive control.</span>
            <button type="button" (click)="toggleInteractive()" class="underline font-medium ml-2 cursor-pointer">
              Take Control
            </button>
          </div>
        }

        <!-- Terminal Viewport -->
        <div #termHost class="flex-1 min-h-0 w-full overflow-hidden relative bg-[#111214]"></div>

        <!-- Mobile Touch Quick-Keys (only when Interactive on < 768px) -->
        @if (interactiveMode()) {
          <div class="md:hidden flex items-center justify-around gap-1.5 px-2 py-1.5 border-t border-[#22242a] bg-[#16171a] shrink-0">
            <button type="button" (click)="sendKey('\x03')" class="flex-1 rounded bg-[#222328] active:bg-[#d9a043]/25 border border-[#2e3038] py-1.5 text-[11px] font-mono-ide text-[#e6b763]">
              Ctrl+C
            </button>
            <button type="button" (click)="sendKey('\x1b')" class="flex-1 rounded bg-[#222328] active:bg-white/10 border border-[#2e3038] py-1.5 text-[11px] font-mono-ide text-[#ececed]">
              Esc
            </button>
            <button type="button" (click)="sendKey('\t')" class="flex-1 rounded bg-[#222328] active:bg-white/10 border border-[#2e3038] py-1.5 text-[11px] font-mono-ide text-[#ececed]">
              Tab
            </button>
            <button type="button" (click)="sendKey('\r')" class="flex-1 rounded bg-[#4a668a]/30 active:bg-[#4a668a]/50 border border-[#4a668a]/60 py-1.5 text-[11px] font-mono-ide text-[#ececed]">
              Enter
            </button>
          </div>
        }
      </div>
    </div>
  `,
})
export class XtermPaneComponent implements OnDestroy {
  readonly taskId = input.required<string>();
  readonly visible = input<boolean>(true);
  readonly embedded = input<boolean>(false);

  private readonly socket = inject(KonsertSocketService);
  private readonly api = inject(ApiClientService);
  private readonly fleetStore = inject(FleetStore);
  private readonly tabsStore = inject(TabsStore);

  private readonly termHost = viewChild<ElementRef<HTMLDivElement>>('termHost');

  readonly readOnlyToast = signal(false);
  private toastTimer: ReturnType<typeof setTimeout> | null = null;

  readonly task = computed(() => this.fleetStore.tasksById()[this.taskId()]);
  readonly worker = computed(() => {
    const wid = this.task()?.worker_id;
    return wid ? this.fleetStore.workersById()[wid] : undefined;
  });
  readonly displayAgent = computed(() => {
    switch (this.task()?.agent) {
      case 'claude':
        return 'Claude Code';
      case 'agy':
        return 'Antigravity';
      case 'grok':
        return 'Grok';
      case 'copilot':
        return 'GitHub Copilot';
      case 'cursor':
        return 'Cursor Agent';
      case 'shell':
        return 'Shell';
      case 'mock':
        return 'Mock Agent';
      default:
        return this.task()?.agent || 'Terminal';
    }
  });
  readonly interactiveMode = computed(() => {
    if (this.embedded()) return true;
    const tab = this.tabsStore.openTabs().find((t) => t.id === this.taskId());
    return tab?.interactiveMode ?? false;
  });
  readonly isActiveTask = computed(() => {
    const st = this.task()?.status;
    return st === 'running' || st === 'waiting_input' || st === 'assigned' || st === 'queued';
  });

  private term: Terminal | null = null;
  private webgl: WebglAddon | null = null;
  /** Set once xterm is created so effects can react to it (afterNextRender runs after them). */
  private readonly termReady = signal<Terminal | null>(null);
  private fitAddon: FitAddon | null = null;
  private resizeObserver: ResizeObserver | null = null;
  private resizeDebounce: ReturnType<typeof setTimeout> | null = null;
  private unsubPty: (() => void) | null = null;

  constructor() {
    afterNextRender(() => {
      this.initTerminal();
    });

    effect(() => {
      const term = this.termReady();
      const visible = this.visible();
      if (!term) return;
      // Browsers cap live WebGL contexts (~16), so only visible panes hold one;
      // hidden panes fall back to xterm's DOM renderer.
      untracked(() => (visible ? this.enableWebgl(term) : this.disableWebgl()));
      if (visible) {
        setTimeout(() => this.fitAndNotify(), 25);
      }
    });

    effect(() => {
      const term = this.termReady();
      const fs = clampFontSize(this.fleetStore.settings().terminal_font_size);
      if (term && term.options.fontSize !== fs) {
        term.options.fontSize = fs;
        untracked(() => this.fitAndNotify());
      }
    });

    // (Re)bind the PTY stream whenever the taskId input changes.
    effect(() => {
      const term = this.termReady();
      const id = this.taskId();
      if (term) {
        untracked(() => this.attachPty(term, id));
      }
    });
  }

  private enableWebgl(term: Terminal): void {
    if (this.webgl) return;
    try {
      const webgl = new WebglAddon();
      webgl.onContextLoss(() => this.disableWebgl());
      term.loadAddon(webgl);
      this.webgl = webgl;
    } catch {
      // No WebGL: keep the DOM renderer.
    }
  }

  private disableWebgl(): void {
    const webgl = this.webgl;
    this.webgl = null;
    try {
      webgl?.dispose();
    } catch {
      // already disposed
    }
  }

  private attachPty(term: Terminal, taskId: string): void {
    this.unsubPty?.();
    this.unsubPty = null;
    term.reset();
    if (!taskId) return;
    this.unsubPty = this.socket.subscribePty(taskId, (ev) => {
      if (ev.frameType === FRAME_PTY_SNAPSHOT) {
        term.reset();
        if (ev.data.byteLength > 0) {
          term.write(ev.data);
        }
      } else if (ev.frameType === FRAME_PTY_DATA) {
        term.write(ev.data);
      }
    });
    this.fitAndNotify();
  }

  private initTerminal(): void {
    const hostEl = this.termHost()?.nativeElement;
    if (!hostEl) return;

    const term = new Terminal({
      cursorBlink: true,
      fontSize: clampFontSize(this.fleetStore.settings().terminal_font_size),
      lineHeight: 1.25,
      // xterm rasterizes glyphs onto a canvas, which skips the browser's
      // -webkit-font-smoothing and renders noticeably heavier than DOM text,
      // especially on HiDPI screens. Light/SemiBold matches the visual weight
      // of Regular/Bold elsewhere in the UI.
      fontWeight: TERMINAL_FONT_WEIGHT,
      fontWeightBold: TERMINAL_FONT_WEIGHT_BOLD,
      fontFamily: '"JetBrainsMono Nerd Font Mono", "JetBrainsMono Nerd Font", "JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace',
      theme: {
        background: '#111214',
        foreground: '#e4e4e7',
        cursor: '#d4d4d8',
        selectionBackground: '#4a668a55',
        black: '#18191c',
        red: '#e05d5d',
        green: '#69b879',
        yellow: '#d9a043',
        blue: '#60a5fa',
        magenta: '#b894f6',
        cyan: '#56c2c6',
        white: '#d4d4d8',
      },
      scrollback: 10000,
    });

    const fitAddon = new FitAddon();
    term.loadAddon(fitAddon);
    term.loadAddon(new WebLinksAddon());
    term.open(hostEl);


    this.term = term;
    this.fitAddon = fitAddon;
    fitAddon.fit();

    // Glyphs rasterized before the Light web font arrives are cached in the
    // texture atlas at the fallback weight; redraw once it has loaded.
    document.fonts
      ?.load(`${TERMINAL_FONT_WEIGHT} ${term.options.fontSize}px "JetBrains Mono"`)
      .then(() => {
        if (this.term === term) {
          term.clearTextureAtlas();
          term.refresh(0, term.rows - 1);
        }
      })
      .catch(() => undefined);

    term.onData((data) => {
      if (!this.interactiveMode()) {
        this.showReadOnlyToast();
        return;
      }
      this.socket.sendPtyStdin(this.taskId(), data);
    });

    this.resizeObserver = new ResizeObserver(() => {
      if (this.resizeDebounce) clearTimeout(this.resizeDebounce);
      this.resizeDebounce = setTimeout(() => this.fitAndNotify(), 100);
    });
    this.resizeObserver.observe(hostEl);
    // Publishing the terminal triggers the font-size and PTY-binding effects.
    this.termReady.set(term);
  }

  private fitAndNotify(): void {
    if (!this.fitAddon || !this.term) return;
    try {
      this.fitAddon.fit();
      const cols = this.term.cols;
      const rows = this.term.rows;
      if (cols > 0 && rows > 0) {
        this.socket.sendPtyResize(this.taskId(), cols, rows);
      }
    } catch {
      // Ignore while hidden
    }
  }

  private showReadOnlyToast(): void {
    this.readOnlyToast.set(true);
    if (this.toastTimer) clearTimeout(this.toastTimer);
    this.toastTimer = setTimeout(() => this.readOnlyToast.set(false), 3200);
  }

  toggleInteractive(): void {
    this.tabsStore.toggleInteractiveMode(this.taskId());
    this.readOnlyToast.set(false);
    // Audit signal only (fire-and-forget): the toggle itself stays client-side.
    this.api.setTaskControl(this.taskId(), this.interactiveMode()).catch(() => undefined);
    if (this.interactiveMode()) {
      this.term?.focus();
    }
  }

  sendKey(seq: string): void {
    if (!this.interactiveMode()) return;
    this.socket.sendPtyStdin(this.taskId(), seq);
  }

  async cancelTask(): Promise<void> {
    try {
      await this.api.cancelTask(this.taskId());
    } catch {
      // ignore
    }
  }

  ngOnDestroy(): void {
    if (this.toastTimer) clearTimeout(this.toastTimer);
    if (this.resizeDebounce) clearTimeout(this.resizeDebounce);
    this.resizeObserver?.disconnect();
    this.unsubPty?.();
    this.disableWebgl();
    this.term?.dispose();
  }
}

export const DEFAULT_TERMINAL_FONT_SIZE = 13;

/** Settings allow 10-24px; anything else (unset, corrupt) falls back to the default. */
export const TERMINAL_FONT_WEIGHT = 300;
export const TERMINAL_FONT_WEIGHT_BOLD = 600;

export function clampFontSize(size: number | undefined): number {
  return typeof size === 'number' && size >= 10 && size <= 24 ? size : DEFAULT_TERMINAL_FONT_SIZE;
}

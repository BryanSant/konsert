import { Component, OnDestroy, effect, inject, input, model, signal, untracked } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { FSEntry } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { IconComponent } from './icon.component';
import { TooltipDirective } from './tooltip.directive';

let nextPickerId = 0;

@Component({
  selector: 'app-remote-dir-picker',
  standalone: true,
  imports: [TooltipDirective, FormsModule, IconComponent],
  template: `
    <div class="relative">
      <div class="flex items-center gap-1.5">
        <div class="relative flex-1 min-w-0">
          <input
            type="text"
            [name]="inputName()"
            [attr.id]="inputId() || null"
            [attr.aria-label]="inputId() ? null : 'Directory path'"
            role="combobox"
            aria-autocomplete="list"
            [attr.aria-expanded]="suggestionsOpen() && !browseOpen() && suggestions().length > 0"
            [attr.aria-controls]="listboxId"
            [attr.aria-activedescendant]="suggestionsOpen() && suggestions().length > 0 ? listboxId + '-' + activeIdx() : null"
            [ngModel]="value()"
            (ngModelChange)="onInputChange($event)"
            (focus)="onFocus()"
            (blur)="onBlur()"
            (keydown)="onKeydown($event)"
            [placeholder]="placeholder()"
            autocomplete="off"
            spellcheck="false"
            class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 font-mono-ide text-[12px] text-[#ececed] placeholder-[#5e6068] focus:border-[#4a668a] focus:outline-none"
          />
        </div>

        <button
          type="button"
          (click)="toggleBrowse()"
          ksTooltip="Browse remote machine folders"
          class="ks-btn shrink-0"
          [class.border-[#4a668a]]="browseOpen()"
          [class.text-[#ececed]]="browseOpen()"
        >
          <app-icon name="folder" [size]="13" />
          <span>Browse</span>
        </button>
      </div>

      <!-- Type-ahead Suggestions Dropdown -->
      @if (suggestionsOpen() && !browseOpen() && (suggestions().length > 0 || loadingSuggestions())) {
        <div
          class="absolute left-0 right-0 top-full mt-1 z-30 rounded-lg bg-[#18191d] shadow-lg overflow-hidden"
          (mousedown)="$event.preventDefault()"
        >
          @if (loadingSuggestions() && suggestions().length === 0) {
            <div class="px-3 py-2 text-[11px] text-[#8a8c94]">Searching remote host...</div>
          } @else {
            <div class="max-h-48 overflow-y-auto py-1" role="listbox" [id]="listboxId" aria-label="Matching folders">
              @for (entry of suggestions(); track entry.path; let i = $index) {
                <button
                  type="button"
                  role="option"
                  tabindex="-1"
                  [id]="listboxId + '-' + i"
                  [attr.aria-selected]="activeIdx() === i"
                  (mouseenter)="activeIdx.set(i)"
                  (click)="pickSuggestion(entry)"
                  class="w-full flex items-center justify-between gap-2 px-2.5 py-1.5 text-left text-[12px] transition cursor-pointer"
                  [class.bg-[#272a33]]="activeIdx() === i"
                  [class.text-[#f4f4f5]]="activeIdx() === i"
                  [class.text-[#c8cad0]]="activeIdx() !== i"
                >
                  <div class="flex items-center gap-2 min-w-0">
                    <app-icon [name]="entry.is_git_repo ? 'git-branch' : 'folder'" [size]="12" [class.text-[#69b879]]="entry.is_git_repo" [class.text-[#8a8c94]]="!entry.is_git_repo" />
                    <span class="font-mono-ide truncate">{{ entry.path }}</span>
                  </div>
                  @if (entry.is_git_repo) {
                    <span class="rounded bg-[#69b879]/15 px-1.5 py-0.5 text-[9px] font-mono-ide text-[#8cd49b] shrink-0">git</span>
                  }
                </button>
              }
            </div>
          }
        </div>
      }

      <!-- Inline Remote Host Folder Browser -->
      @if (browseOpen()) {
        <div class="mt-2 rounded-lg bg-[#131418] overflow-hidden">
          <div class="flex items-center justify-between gap-2 px-2.5 py-1.5 border-b border-white/[0.06] bg-[#17181d]">
            <div class="flex items-center gap-1.5 min-w-0">
              @if (browseParent()) {
                <button
                  type="button"
                  (click)="navigateBrowse(browseParent())"
                  ksTooltip="Parent directory" aria-label="Parent directory"
                  class="ks-btn ks-btn-sm ks-btn-soft font-mono-ide shrink-0"
                >
                  ..
                </button>
              }
              <span class="font-mono-ide text-[11px] text-[#b4b6be] truncate" [ksTooltip]="browseCurrentDir()">
                {{ browseCurrentDir() || value() || '~' }}
              </span>
            </div>
            <div class="flex items-center gap-1.5 shrink-0">
              <button
                type="button"
                (click)="selectCurrentBrowseDir()"
                class="rounded bg-[#262933] hover:bg-[#313542] px-2 py-0.5 text-[10px] font-medium text-[#ececed] transition cursor-pointer"
              >
                Use folder
              </button>
              <button
                type="button"
                (click)="browseOpen.set(false)"
                aria-label="Close folder browser"
                class="ks-icon-btn ks-icon-btn-sm"
              >
                <app-icon name="close" [size]="13" />
              </button>
            </div>
          </div>

          @if (browseError()) {
            <div class="px-3 py-2.5 text-[11px] text-[#f09595]">{{ browseError() }}</div>
          } @else if (loadingBrowse()) {
            <div class="px-3 py-3 text-[11px] text-[#8a8c94]">Listing remote directories...</div>
          } @else if (browseEntries().length === 0) {
            <div class="px-3 py-3 text-[11px] text-[#86888f]">No subdirectories here.</div>
          } @else {
            <div class="max-h-44 overflow-y-auto py-1">
              @for (entry of browseEntries(); track entry.path) {
                <div class="group flex items-center justify-between gap-2 px-2.5 py-1 hover:bg-[#1e2027] transition">
                  <button
                    type="button"
                    (click)="navigateBrowse(entry.path)"
                    class="flex items-center gap-2 min-w-0 flex-1 text-left cursor-pointer"
                  >
                    <app-icon
                      [name]="entry.is_git_repo ? 'git-branch' : 'folder'"
                      [size]="12"
                      [class.text-[#69b879]]="entry.is_git_repo"
                      [class.text-[#8a8c94]]="!entry.is_git_repo"
                    />
                    <span class="font-mono-ide text-[11px] text-[#d4d4d8] group-hover:text-[#f4f4f5] truncate">{{ entry.name }}</span>
                    @if (entry.is_git_repo) {
                      <span class="rounded bg-[#69b879]/15 px-1 py-0.5 text-[9px] font-mono-ide text-[#8cd49b] shrink-0">git</span>
                    }
                  </button>
                  <button
                    type="button"
                    (click)="selectAndClose(entry.path)"
                    class="ks-btn ks-btn-sm ks-btn-soft opacity-0 group-hover:opacity-100 shrink-0"
                  >
                    Select
                  </button>
                </div>
              }
            </div>
          }
        </div>
      }
    </div>
  `,
})
export class RemoteDirPickerComponent implements OnDestroy {
  readonly workerId = input<string>('');
  readonly placeholder = input<string>('~ or /path/to/repo');
  readonly inputName = input<string>('remoteDir');
  /** id for the text input, so a parent <label for> can name it. */
  readonly inputId = input<string>('');
  readonly listboxId = `ks-dir-suggest-${nextPickerId++}`;
  readonly value = model<string>('~');

  private readonly api = inject(ApiClientService);

  readonly suggestions = signal<FSEntry[]>([]);
  readonly suggestionsOpen = signal(false);
  readonly loadingSuggestions = signal(false);
  readonly activeIdx = signal(0);

  readonly browseOpen = signal(false);
  readonly browseCurrentDir = signal('');
  readonly browseParent = signal('');
  readonly browseEntries = signal<FSEntry[]>([]);
  readonly loadingBrowse = signal(false);
  readonly browseError = signal('');

  private debounceTimer: ReturnType<typeof setTimeout> | null = null;
  private blurTimer: ReturnType<typeof setTimeout> | null = null;
  private suggestSeq = 0;
  private browseSeq = 0;

  constructor() {
    // Re-fetch the browse view only when the target machine changes while the browser
    // is open. value()/browseOpen() are read untracked: navigation and toggling already
    // load explicitly, and typing must not trigger a browse fetch per keystroke.
    let lastWorkerId: string | undefined;
    effect(() => {
      const wid = this.workerId();
      untracked(() => {
        const changed = lastWorkerId !== undefined && lastWorkerId !== wid;
        lastWorkerId = wid;
        if (!changed) return;
        this.suggestSeq++; // drop suggestions in flight for the previous machine
        this.loadingSuggestions.set(false);
        if (this.browseOpen()) {
          void this.loadBrowse(this.value() || '~', wid);
        }
      });
    });
  }

  onInputChange(next: string): void {
    this.value.set(next);
    this.suggestionsOpen.set(true);
    this.scheduleSuggestions(next);
  }

  onFocus(): void {
    if (this.blurTimer) {
      clearTimeout(this.blurTimer);
      this.blurTimer = null;
    }
    if (!this.browseOpen()) {
      this.suggestionsOpen.set(true);
      this.scheduleSuggestions(this.value() || '~');
    }
  }

  onBlur(): void {
    this.blurTimer = setTimeout(() => {
      this.suggestionsOpen.set(false);
    }, 150);
  }

  onKeydown(ev: KeyboardEvent): void {
    const list = this.suggestions();
    if (!this.suggestionsOpen() || list.length === 0) return;

    if (ev.key === 'ArrowDown') {
      ev.preventDefault();
      this.activeIdx.update((i) => (i + 1) % list.length);
    } else if (ev.key === 'ArrowUp') {
      ev.preventDefault();
      this.activeIdx.update((i) => (i - 1 + list.length) % list.length);
    } else if (ev.key === 'Tab') {
      ev.preventDefault();
      const target = list[this.activeIdx()] ?? list[0];
      if (target) {
        const nextPath = target.path.endsWith('/') ? target.path : `${target.path}/`;
        this.value.set(nextPath);
        this.scheduleSuggestions(nextPath);
      }
    } else if (ev.key === 'Enter') {
      const target = list[this.activeIdx()];
      if (target && target.path !== this.value()) {
        ev.preventDefault();
        this.pickSuggestion(target);
      }
    } else if (ev.key === 'Escape') {
      // Close the suggestions only; don't let the enclosing dialog close too.
      ev.preventDefault();
      ev.stopPropagation();
      this.suggestionsOpen.set(false);
    }
  }

  pickSuggestion(entry: FSEntry): void {
    this.value.set(entry.path);
    this.suggestionsOpen.set(false);
  }

  toggleBrowse(): void {
    const next = !this.browseOpen();
    this.browseOpen.set(next);
    if (next) {
      this.suggestionsOpen.set(false);
      void this.loadBrowse(this.value().trim() || '~', this.workerId());
    }
  }

  navigateBrowse(path: string): void {
    this.value.set(path);
    void this.loadBrowse(path, this.workerId());
  }

  selectCurrentBrowseDir(): void {
    if (this.browseCurrentDir()) {
      this.value.set(this.browseCurrentDir());
    }
    this.browseOpen.set(false);
  }

  selectAndClose(path: string): void {
    this.value.set(path);
    this.browseOpen.set(false);
  }

  private scheduleSuggestions(rawPath: string): void {
    if (this.debounceTimer) clearTimeout(this.debounceTimer);
    this.debounceTimer = setTimeout(() => {
      void this.fetchSuggestions(rawPath);
    }, 150);
  }

  private async fetchSuggestions(rawPath: string): Promise<void> {
    const seq = ++this.suggestSeq;
    this.loadingSuggestions.set(true);
    try {
      const res = await this.api.listRemoteFS({
        workerId: this.workerId() || undefined,
        path: rawPath.trim() || '~',
        dirsOnly: true,
      });
      if (seq !== this.suggestSeq) return;
      if (res.error) {
        this.suggestions.set([]);
      } else {
        this.suggestions.set((res.entries ?? []).slice(0, 12));
        this.activeIdx.set(0);
      }
    } catch {
      if (seq === this.suggestSeq) {
        this.suggestions.set([]);
      }
    } finally {
      if (seq === this.suggestSeq) {
        this.loadingSuggestions.set(false);
      }
    }
  }

  private async loadBrowse(path: string, workerId: string): Promise<void> {
    const seq = ++this.browseSeq;
    this.loadingBrowse.set(true);
    this.browseError.set('');
    try {
      const res = await this.api.listRemoteFS({
        workerId: workerId || undefined,
        path: path || '~',
        dirsOnly: true,
      });
      if (seq !== this.browseSeq) return; // a newer navigation superseded this one
      if (res.error) {
        this.browseError.set(res.error);
        this.browseEntries.set([]);
      } else {
        this.browseCurrentDir.set(res.current_dir || path);
        this.browseParent.set(res.parent_dir || '');
        this.browseEntries.set(res.entries ?? []);
      }
    } catch (err) {
      if (seq !== this.browseSeq) return;
      this.browseError.set(err instanceof Error ? err.message : 'Unable to browse remote host');
      this.browseEntries.set([]);
    } finally {
      if (seq === this.browseSeq) this.loadingBrowse.set(false);
    }
  }

  ngOnDestroy(): void {
    if (this.debounceTimer) clearTimeout(this.debounceTimer);
    if (this.blurTimer) clearTimeout(this.blurTimer);
  }
}

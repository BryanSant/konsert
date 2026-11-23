import {
  ChangeDetectionStrategy,
  Component,
  ElementRef,
  computed,
  inject,
  output,
  signal,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Repo, SessionKind, Worker, Worktree } from '../core/models/konsert.models';
import { FleetStore, basenamePath } from '../core/state/fleet.store';
import { ORCHESTRATOR_TAB_ID, TabsStore } from '../core/state/tabs.store';
import { IconComponent } from '../shared/icon.component';
import { ModalDialogComponent } from '../shared/modal-dialog.component';

/** Shell-level commands the palette can't perform itself. */
export type PaletteCommand =
  | { kind: 'open-session'; sessionKind: SessionKind; worker: Worker | null }
  | { kind: 'open-worktree'; repo: Repo; worktree: Worktree | null }
  | { kind: 'toggle-split' }
  | { kind: 'add-machine' }
  | { kind: 'settings' }
  | { kind: 'show-left' }
  | { kind: 'toggle-left' }
  | { kind: 'toggle-right' };

type PaletteSection = 'sessions' | 'taxonomy' | 'machines' | 'actions';

interface PaletteItem {
  id: string;
  section: PaletteSection;
  label: string;
  sublabel?: string;
  icon: string;
  dotOnline?: boolean;
  pill?: string;
  run: () => void;
}

export interface IndexedPaletteItem extends PaletteItem {
  flatIndex: number;
}

const SECTION_TITLES: Record<PaletteSection, string> = {
  sessions: 'Recent Sessions & Terminals',
  taxonomy: 'Spaces, Projects, Repos & Worktrees',
  machines: 'Machines',
  actions: 'Actions',
};
const SECTION_ORDER: PaletteSection[] = ['sessions', 'taxonomy', 'machines', 'actions'];

@Component({
  selector: 'app-command-palette',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [FormsModule, IconComponent, ModalDialogComponent],
  template: `
    <app-modal-dialog label="Command palette" align="top" panelClass="w-full max-w-xl rounded-xl border border-[#31333d] bg-[#191a1e]/95 backdrop-blur-2xl overflow-hidden" (close)="close.emit()">
      <!-- Inner Rounded Search Input -->
      <div class="p-2.5 border-b border-[#25272e]">
        <div class="flex items-center gap-2.5 rounded-lg border border-[#2c2e37] bg-[#141518] px-3 py-2 focus-within:border-[#454958] transition">
          <app-icon name="search" [size]="15" class="text-[#787a82]" />
          <input
            type="text"
            role="combobox"
            aria-label="Search sessions, machines and actions"
            aria-autocomplete="list"
            aria-expanded="true"
            aria-controls="ks-palette-listbox"
            [attr.aria-activedescendant]="items().length ? optionId(activeIndex()) : null"
            [ngModel]="query()"
            (ngModelChange)="onQuery($event)"
            (keydown)="onKeydown($event)"
            placeholder="Search spaces, projects, repos, worktrees, machines, sessions..."
            autofocus
            class="flex-1 bg-transparent text-[13px] text-[#ececed] placeholder-[#686a72] focus:outline-none"
          />
          <span class="rounded border border-[#2d3039] bg-[#1c1e24] px-2 py-0.5 text-[11px] text-[#8e9098]" aria-live="polite" [attr.aria-label]="items().length + ' results'">
            {{ items().length }}
          </span>
        </div>
      </div>

      <!-- Grouped Results -->
      <div class="max-h-[360px] overflow-y-auto px-2 py-2 space-y-3 text-[12px]" id="ks-palette-listbox" [attr.role]="items().length ? 'listbox' : null" [attr.aria-label]="items().length ? 'Results' : null">
        @if (items().length === 0) {
          <div class="py-8 text-center text-[#686a72]" role="status">No matching sessions, machines, or actions.</div>
        }

        @for (group of groups(); track group.section) {
          <div class="space-y-0.5" role="group" [attr.aria-labelledby]="'ks-palette-group-' + group.section">
            <div class="px-2.5 pb-1 text-[10px] font-semibold uppercase tracking-wider text-[#686a72]" [id]="'ks-palette-group-' + group.section">
              {{ group.title }}
            </div>
            @for (item of group.items; track item.id) {
              <button
                type="button"
                role="option"
                tabindex="-1"
                [id]="optionId(item.flatIndex)"
                [attr.aria-selected]="activeIndex() === item.flatIndex"
                (mouseenter)="activeIndex.set(item.flatIndex)"
                (click)="run(item)"
                class="w-full flex items-center justify-between gap-2 rounded-lg px-2.5 py-2 text-left transition-colors duration-100 cursor-pointer"
                [class.bg-[#2c2e36]]="activeIndex() === item.flatIndex"
                [class.text-[#f4f4f5]]="activeIndex() === item.flatIndex"
                [class.text-[#c8cad0]]="activeIndex() !== item.flatIndex"
              >
                <div class="flex items-center gap-2.5 min-w-0">
                  @if (item.section === 'machines') {
                    <span class="h-2 w-2 rounded-full shrink-0" [class.bg-[#69b879]]="item.dotOnline" [class.bg-[#4c4e56]]="!item.dotOnline"></span>
                  } @else if (item.section === 'sessions') {
                    <app-icon [name]="item.icon" [size]="14" />
                  } @else {
                    <app-icon [name]="item.icon" [size]="13" class="text-[#8e9098]" />
                  }
                  <span class="truncate" [class.font-medium]="item.section !== 'actions'">{{ item.label }}</span>
                  @if (item.sublabel) {
                    @if (item.section === 'machines') {
                      <span class="rounded border border-[#32343d] bg-[#202127] px-1.5 py-0.5 text-[10px] text-[#8e9098]">{{ item.sublabel }}</span>
                    } @else {
                      <span class="text-[11px] text-[#787a82]" [class.shrink-0]="item.section === 'sessions'" [class.truncate]="item.section !== 'sessions'">{{ item.sublabel }}</span>
                    }
                  }
                </div>
                @if (item.pill) {
                  @if (item.section === 'actions') {
                    <span class="ks-kbd">{{ item.pill }}</span>
                  } @else {
                    <span class="rounded border border-[#343640] bg-[#1a1b20] px-1.5 py-0.5 text-[10px] font-mono-ide text-[#a1a3ab] shrink-0">{{ item.pill }}</span>
                  }
                }
              </button>
            }
          </div>
        }
      </div>

      <!-- Orca-Style Keyboard Hint Footer -->
      <div class="flex items-center justify-end gap-3 px-3 py-2 border-t border-[#24262d] bg-[#15161a]/90 text-[11px] text-[#6d7078]">
        <span class="inline-flex items-center gap-1"><span class="ks-kbd">Enter</span> Open</span>
        <span class="inline-flex items-center gap-1"><span class="ks-kbd">Esc</span> Close</span>
        <span class="inline-flex items-center gap-1"><span class="ks-kbd">↑↓</span> Move</span>
        <span class="inline-flex items-center gap-1"><span class="ks-kbd">⌘J</span> Scratch</span>
      </div>
    </app-modal-dialog>
  `,
})
export class CommandPaletteComponent {
  private readonly fleet = inject(FleetStore);
  private readonly tabs = inject(TabsStore);
  private readonly host = inject<ElementRef<HTMLElement>>(ElementRef);

  readonly command = output<PaletteCommand>();
  readonly close = output<void>();

  readonly query = signal('');
  readonly activeIndex = signal(0);

  /** Every palette entry, unfiltered. */
  private readonly allItems = computed<PaletteItem[]>(() => {
    const items: PaletteItem[] = [];
    const emit = (cmd: PaletteCommand) => () => this.command.emit(cmd);

    for (const r of this.fleet.agentsRoster().slice(0, 6)) {
      const dir = basenamePath(r.task.effective_dir || r.task.workdir);
      items.push({
        id: `sess-${r.task.id}`,
        section: 'sessions',
        label: r.task.prompt ? r.task.prompt.slice(0, 48) : `${r.task.agent} · ${dir || '~'}`,
        sublabel: r.task.status,
        icon: r.task.agent,
        pill: r.task.branch ? `${r.workerName} · ${r.task.branch}` : r.workerName,
        run: () => this.tabs.openTaskTab(r.task, r.workerName),
      });
    }

    const wsById = new Map(this.fleet.workspaces().map((ws) => [ws.id, ws]));
    const projById = new Map(this.fleet.projects().map((p) => [p.id, p]));
    const repoById = new Map(this.fleet.repos().map((r) => [r.id, r]));
    const workersById = this.fleet.workersById();

    for (const ws of this.fleet.workspaces()) {
      items.push({
        id: `tax-ws-${ws.id}`,
        section: 'taxonomy',
        label: ws.name,
        sublabel: ws.is_default ? 'Default Space' : 'Space',
        icon: 'workspace',
        pill: 'Space',
        run: emit({ kind: 'show-left' }),
      });
    }
    for (const proj of this.fleet.projects()) {
      items.push({
        id: `tax-proj-${proj.id}`,
        section: 'taxonomy',
        label: proj.name,
        sublabel: wsById.get(proj.workspace_id)?.name || 'Default',
        icon: 'project',
        pill: 'Project',
        run: emit({ kind: 'show-left' }),
      });
    }
    for (const repo of this.fleet.repos()) {
      const projName = projById.get(repo.project_id)?.name || 'Default';
      items.push({
        id: `tax-repo-${repo.id}`,
        section: 'taxonomy',
        label: repo.name,
        sublabel: `${projName} · ${repo.path}`,
        icon: 'repo',
        pill: workersById[repo.worker_id]?.name || repo.worker_id,
        run: emit({ kind: 'open-worktree', repo, worktree: null }),
      });
    }
    for (const wt of this.fleet.worktrees()) {
      const repo = repoById.get(wt.repo_id);
      if (!repo) continue;
      items.push({
        id: `tax-wt-${wt.id}`,
        section: 'taxonomy',
        label: `${repo.name} · ${wt.branch || basenamePath(wt.path)}`,
        sublabel: wt.path,
        icon: 'git-branch',
        pill: wt.is_primary ? 'primary' : 'worktree',
        run: emit({ kind: 'open-worktree', repo, worktree: wt }),
      });
    }

    for (const w of this.fleet.workers()) {
      items.push({
        id: `machine-${w.id}`,
        section: 'machines',
        label: w.name,
        sublabel: w.isolation,
        icon: w.isolation === 'container' ? 'container' : 'server',
        dotOnline: w.status === 'online',
        pill: `${w.os}/${w.arch}`,
        run: emit({ kind: 'open-session', sessionKind: 'chat', worker: w }),
      });
    }

    const action = (id: string, label: string, icon: string, run: () => void, pill?: string): PaletteItem => ({
      id,
      section: 'actions',
      label,
      icon,
      pill,
      run,
    });
    items.push(
      action('act-chat', 'New Chat Session on a Machine', 'orchestrator', emit({ kind: 'open-session', sessionKind: 'chat', worker: null }), 'Chat'),
      action('act-terminal', 'New Terminal on a Machine', 'shell', emit({ kind: 'open-session', sessionKind: 'terminal', worker: null }), 'Term'),
      action('act-split', 'Toggle Side-by-Side Split Terminal', 'split', emit({ kind: 'toggle-split' }), '⌘\\'),
      action('act-scratch', 'Toggle Floating Scratch Workspace', 'scratch', () => this.tabs.toggleScratch(), '⌘J'),
      action('act-orch', 'Focus Orchestrator Chat', 'orchestrator', () => this.tabs.selectTab(ORCHESTRATOR_TAB_ID)),
      action('act-add', 'Add Machine (SSH or konsertd)', 'plus', emit({ kind: 'add-machine' })),
      action('act-settings', 'Open Settings (LLM, Terminal, Alerts)', 'settings', emit({ kind: 'settings' })),
      action('act-left', 'Toggle Left Sidebar', 'sidebar-left', emit({ kind: 'toggle-left' })),
      action('act-right', 'Toggle Right Inspector', 'sidebar-right', emit({ kind: 'toggle-right' })),
    );
    return items;
  });

  /** Filtered entries in display order, each tagged with its keyboard index. */
  readonly items = computed<IndexedPaletteItem[]>(() => {
    const q = this.query().trim().toLowerCase();
    const matched = q
      ? this.allItems().filter(
          (i) =>
            i.label.toLowerCase().includes(q) ||
            (i.sublabel || '').toLowerCase().includes(q) ||
            (i.pill || '').toLowerCase().includes(q),
        )
      : this.allItems();
    // Sections are already contiguous and in SECTION_ORDER, so flat order == visual order.
    return matched.map((item, idx) => ({ ...item, flatIndex: idx }));
  });

  readonly groups = computed(() =>
    SECTION_ORDER.map((section) => ({
      section,
      title: SECTION_TITLES[section],
      items: this.items().filter((i) => i.section === section),
    })).filter((g) => g.items.length > 0),
  );

  onQuery(val: string): void {
    this.query.set(val);
    this.activeIndex.set(0);
  }

  onKeydown(ev: KeyboardEvent): void {
    const list = this.items();
    if (list.length === 0) return;
    if (ev.key === 'ArrowDown') {
      ev.preventDefault();
      this.moveTo((this.activeIndex() + 1) % list.length);
    } else if (ev.key === 'ArrowUp') {
      ev.preventDefault();
      this.moveTo((this.activeIndex() - 1 + list.length) % list.length);
    } else if (ev.key === 'Home') {
      ev.preventDefault();
      this.moveTo(0);
    } else if (ev.key === 'End') {
      ev.preventDefault();
      this.moveTo(list.length - 1);
    } else if (ev.key === 'Enter') {
      ev.preventDefault();
      const target = list[this.activeIndex()] ?? list[0];
      if (target) this.run(target);
    }
  }

  optionId(index: number): string {
    return `ks-palette-opt-${index}`;
  }

  /** Moves the active option and keeps it visible in the scrolling list. */
  private moveTo(index: number): void {
    this.activeIndex.set(index);
    const id = this.optionId(index);
    requestAnimationFrame(() => {
      const el = this.host.nativeElement.querySelector<HTMLElement>(`#${id}`);
      el?.scrollIntoView?.({ block: 'nearest' });
    });
  }

  run(item: PaletteItem): void {
    this.close.emit();
    item.run();
  }
}

import { ChangeDetectionStrategy, Component, computed, inject, output, signal } from '@angular/core';
import { Router } from '@angular/router';
import { FleetStore } from '../core/state/fleet.store';
import { QuestionsStore } from '../core/state/questions.store';
import { WorkStore } from '../core/state/work.store';
import { IconComponent } from '../shared/icon.component';
import { ModalDialogComponent } from '../shared/modal-dialog.component';
import { MODES } from '../app.routes';
import { ModeUiStore } from './mode-ui.store';

interface Item {
  id: string;
  group: string;
  label: string;
  sub?: string;
  icon: string;
  keys?: string;
  run: () => void;
}

/** ⌘K: jump to a mode, a session, a machine, or run an action. */
@Component({
  selector: 'ks-mode-palette',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, ModalDialogComponent],
  template: `
    <app-modal-dialog label="Search and commands" align="top" panelClass="w-full max-w-xl rounded-[var(--ks-radius-xl)] bg-[var(--ks-surface-2)] overflow-hidden" (close)="close.emit()">
      <div class="p-2.5 border-b border-[var(--ks-hairline)]">
        <div class="flex items-center gap-2.5 rounded-[var(--ks-radius-md)] bg-[var(--ks-surface-1)] px-3 h-10">
          <app-icon name="search" [size]="15" />
          <input
            type="text"
            role="combobox"
            aria-label="Search work, machines, or run a command"
            aria-autocomplete="list"
            aria-expanded="true"
            aria-controls="ks-mode-palette-list"
            [attr.aria-activedescendant]="items().length ? 'ks-mp-' + active() : null"
            placeholder="Search work, machines, or run a command"
            autofocus
            class="flex-1 bg-transparent text-[13px] text-[var(--ks-text)] placeholder:text-[var(--ks-text-muted)] outline-none"
            [value]="query()"
            (input)="query.set($any($event.target).value); active.set(0)"
            (keydown)="onKey($event)"
          />
        </div>
      </div>
      <div id="ks-mode-palette-list" class="max-h-[380px] overflow-y-auto p-2 text-[12.5px]" [attr.role]="items().length ? 'listbox' : null" aria-label="Results">
        @if (items().length === 0) {
          <p class="py-8 text-center text-[var(--ks-text-muted)]" role="status">Nothing matches.</p>
        }
        @for (it of items(); track it.id; let i = $index) {
          @if (i === 0 || items()[i - 1].group !== it.group) {
            <div class="px-2.5 pt-2 pb-1 text-[10.5px] font-semibold uppercase tracking-wider text-[var(--ks-text-muted)]" aria-hidden="true">{{ it.group }}</div>
          }
          <button
            type="button"
            role="option"
            tabindex="-1"
            [id]="'ks-mp-' + i"
            [attr.aria-selected]="active() === i"
            class="w-full flex items-center gap-2.5 rounded-[var(--ks-radius-md)] px-2.5 py-2 text-left"
            [class.bg-[var(--ks-raised-strong)]]="active() === i"
            (mouseenter)="active.set(i)"
            (click)="run(it)"
          >
            <app-icon [name]="it.icon" [size]="14" />
            <span class="truncate text-[var(--ks-text)]">{{ it.label }}</span>
            @if (it.sub) {
              <span class="truncate text-[11.5px] text-[var(--ks-text-muted)]">{{ it.sub }}</span>
            }
            <span class="flex-1"></span>
            @if (it.keys) {
              <span class="ks-kbd">{{ it.keys }}</span>
            }
          </button>
        }
      </div>
    </app-modal-dialog>
  `,
})
export class ModePaletteComponent {
  readonly close = output<void>();
  private readonly router = inject(Router);
  private readonly ui = inject(ModeUiStore);
  private readonly work = inject(WorkStore);
  private readonly fleet = inject(FleetStore);
  private readonly questions = inject(QuestionsStore);

  readonly query = signal('');
  readonly active = signal(0);

  private readonly all = computed<Item[]>(() => {
    const go = (path: unknown[]) => () => void this.router.navigate(path);
    const out: Item[] = MODES.map((m) => ({
      id: `mode-${m.id}`,
      group: 'Modes',
      label: `Go to ${m.label}`,
      icon: m.id === 'command' ? 'orchestrator' : m.id === 'work' ? 'list-todo' : m.id === 'fleet' ? 'server' : 'split',
      keys: m.key,
      run: go(['/', m.id]),
    }));
    for (const it of this.work.items().slice(0, 60)) {
      out.push({
        id: `s-${it.id}`,
        group: 'Sessions',
        label: it.title,
        sub: [it.sid, it.agent, it.machine, it.status].filter(Boolean).join(' · '),
        icon: it.sessionKind === 'terminal' ? 'shell' : it.agent,
        run: go(['/command', it.sid]),
      });
    }
    for (const w of this.fleet.workers()) {
      out.push({ id: `m-${w.id}`, group: 'Machines', label: w.name, sub: `${w.sid} · ${w.status}`, icon: 'server', run: go(['/fleet', w.sid]) });
    }
    const act = (id: string, label: string, icon: string, run: () => void): Item => ({ id, group: 'Actions', label, icon, run });
    out.push(
      act('new-chat', 'New agent chat…', 'plus', () => this.ui.newSession('chat')),
      act('new-term', 'New terminal…', 'shell', () => this.ui.newSession('terminal')),
      act('questions', 'Open all questions', 'question', () => this.questions.open()),
      act('add-machine', 'Add a machine…', 'server', () => this.ui.addMachineOpen.set(true)),
      act('settings', 'Settings', 'settings', () => this.ui.settingsOpen.set(true)),
      act('devices', 'Devices (signed-in browsers, phones, CLIs)…', 'monitor', () => this.ui.devicesOpen.set('list')),
      act('add-device', 'Add a device (sign in a phone or browser)…', 'phone', () => this.ui.devicesOpen.set('add')),
      act('connect-mcp', 'Connect an MCP client…', 'key', () => this.ui.devicesOpen.set('mcp')),
      act('inbox', 'Needs-you inbox (phone view)', 'bell', go(['/inbox'])),
      act('projects', 'Projects and repos…', 'project', () => this.ui.projectsOpen.set(true)),
      act('audit', 'Audit log', 'shield', () => this.ui.auditOpen.set(true)),
    );
    return out;
  });

  readonly items = computed(() => {
    const q = this.query().trim().toLowerCase();
    if (!q) return this.all();
    return this.all().filter((i) => `${i.label} ${i.sub ?? ''} ${i.group}`.toLowerCase().includes(q));
  });

  onKey(ev: KeyboardEvent): void {
    const n = this.items().length;
    if (!n) return;
    switch (ev.key) {
      case 'ArrowDown':
        ev.preventDefault();
        this.active.set((this.active() + 1) % n);
        break;
      case 'ArrowUp':
        ev.preventDefault();
        this.active.set((this.active() - 1 + n) % n);
        break;
      case 'Home':
        ev.preventDefault();
        this.active.set(0);
        break;
      case 'End':
        ev.preventDefault();
        this.active.set(n - 1);
        break;
      case 'Enter': {
        ev.preventDefault();
        const it = this.items()[this.active()];
        if (it) this.run(it);
        break;
      }
    }
  }

  run(it: Item): void {
    this.close.emit();
    it.run();
  }
}

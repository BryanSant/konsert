import { ChangeDetectionStrategy, Component, OnDestroy, OnInit, computed, effect, inject, untracked } from '@angular/core';
import { Router, RouterOutlet } from '@angular/router';
import { SyreSocketService } from '../core/services/syre-socket.service';
import { ControlStore } from '../core/state/control.store';
import { FleetStore } from '../core/state/fleet.store';
import { TicketStore } from '../core/state/ticket.store';
import { QuestionsStore } from '../core/state/questions.store';
import { SetupStore } from '../core/state/setup.store';
import { TakenControlStore } from '../core/state/taken-control.store';
import { CommanderStore } from '../core/state/commander.store';
import { SessionNavService } from '../core/services/session-nav.service';
import { CommandTabsStore } from './command/command-tabs.store';
import { ReloginDialogComponent } from '../features/chat/relogin-dialog.component';
import { NeedsYouDialogComponent } from '../features/needs-you/needs-you-dialog.component';
import { NeedsYouStore } from '../core/state/needs-you.store';
import { AddMachineModalComponent } from '../layout/add-machine-modal.component';
import { HotkeyService } from '../layout/hotkey.service';
import { MachineDetailModalComponent } from '../layout/machine-detail-modal.component';
import { SessionDispatchModalComponent } from '../layout/session-dispatch-modal.component';
import { SettingsModalComponent } from '../layout/settings-modal.component';
import { ModePaletteComponent } from './mode-palette.component';
import { ProjectsDialogComponent } from '../features/projects/projects-dialog.component';
import { AuditLogComponent } from '../layout/audit-log.component';
import { DevicesDialogComponent } from '../features/auth/devices-dialog.component';
import { ModalDialogComponent } from '../shared/modal-dialog.component';
import { IconComponent } from '../shared/icon.component';
import { ModeUiStore } from './mode-ui.store';

/**
 * Everything authenticated: starts the live connection and the
 * stores, and hosts the dialogs every mode (and the mobile inbox) can open.
 */
@Component({
  selector: 'sy-workspace-root',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [
    RouterOutlet,
    NeedsYouDialogComponent,
    ReloginDialogComponent,
    AddMachineModalComponent,
    SettingsModalComponent,
    SessionDispatchModalComponent,
    MachineDetailModalComponent,
    ModePaletteComponent,
    ProjectsDialogComponent,
    AuditLogComponent,
    DevicesDialogComponent,
    ModalDialogComponent,
    IconComponent,
  ],
  host: { class: 'block h-dvh w-screen overflow-hidden bg-[var(--sy-bg-canvas)] text-[var(--sy-text)]' },
  template: `
    <router-outlet />

    <sy-relogin-dialog />
    @if (needsYou.isOpen()) {
      <sy-needs-you-dialog />
    }
    @if (ui.addMachineOpen()) {
      <app-add-machine-modal
        [firstRun]="setup.needsSetup()"
        (close)="closeAddMachine()"
        (inspectMachine)="closeAddMachine(); ui.inspectedWorkerId.set($event)"
      />
    }
    @if (ui.settingsOpen()) {
      <app-settings-modal
        [focusSection]="ui.settingsSection()"
        (close)="ui.closeSettings()"
        (openDevices)="ui.closeSettings(); ui.devicesOpen.set($event)"
      />
    }
    @if (ui.sessionRequest(); as req) {
      <app-session-dispatch-modal [request]="req" (close)="ui.sessionRequest.set(null)" (started)="onStarted($event)" />
    }
    @if (inspectedWorker(); as w) {
      <app-machine-detail-modal
        [worker]="w"
        (close)="ui.inspectedWorkerId.set(null)"
        (removed)="ui.inspectedWorkerId.set(null)"
        (startChat)="ui.inspectedWorkerId.set(null); ui.newSession('chat', $event)"
        (startTerminal)="ui.inspectedWorkerId.set(null); ui.newSession('terminal', $event)"
      />
    }
    @if (ui.projectsOpen()) {
      <sy-projects-dialog (close)="ui.projectsOpen.set(false)" (chat)="ui.projectsOpen.set(false); ui.sessionRequest.set({ kind: 'chat', worker: null, repo: $event.repo })" />
    }
    @if (ui.auditOpen()) {
      <app-modal-dialog label="Audit log" panelClass="w-full max-w-3xl h-[min(760px,88vh)] flex flex-col rounded-[var(--sy-radius-xl)] bg-[var(--sy-surface-2)] overflow-hidden" (close)="ui.auditOpen.set(false)">
        <div class="flex items-center gap-2 px-4 h-12 shrink-0 border-b border-[var(--sy-hairline)]">
          <h2 class="m-0 flex-1 text-[14px] font-semibold">Audit log</h2>
          <button type="button" class="sy-icon-btn" aria-label="Close" (click)="ui.auditOpen.set(false)"><app-icon name="close" [size]="15" /></button>
        </div>
        <app-audit-log class="flex-1 min-h-0" />
      </app-modal-dialog>
    }
    @if (ui.devicesOpen(); as view) {
      <sy-devices-dialog [initialView]="view" (close)="ui.devicesOpen.set(null)" />
    }
    @if (ui.paletteOpen()) {
      <sy-mode-palette (close)="ui.paletteOpen.set(false)" />
    }
  `,
})
export class WorkspaceRootComponent implements OnInit, OnDestroy {
  readonly ui = inject(ModeUiStore);
  readonly fleet = inject(FleetStore);
  private readonly tickets = inject(TicketStore);
  readonly setup = inject(SetupStore);
  readonly questions = inject(QuestionsStore);
  readonly needsYou = inject(NeedsYouStore);
  private readonly control = inject(ControlStore);
  private readonly socket = inject(SyreSocketService);
  private readonly taken = inject(TakenControlStore);
  private readonly orch = inject(CommanderStore);
  private readonly commandTabs = inject(CommandTabsStore);
  private readonly nav = inject(SessionNavService);
  private readonly router = inject(Router);

  readonly inspectedWorker = computed(() => {
    const id = this.ui.inspectedWorkerId();
    return id ? (this.fleet.workersById()[id] ?? null) : null;
  });

  constructor() {
    inject(HotkeyService).register((action) => {
      if (action.kind === 'palette') {
        this.ui.paletteOpen.set(true);
        return true;
      }
      if (action.kind === 'escape') this.ui.paletteOpen.set(false);
      return false;
    });
    // A shell that exits while watched closes its tab (and its Take Control), like a
    // desktop terminal.
    effect(() => {
      const tasks = this.fleet.tasks();
      untracked(() => {
        for (const id of this.taken.exitedShells(tasks)) {
          const sid = this.fleet.tasksById()[id]?.sid ?? '';
          this.commandTabs.close(id);
          if (this.nav.isOpen(sid)) void this.router.navigate(['/command']);
        }
      });
    });
  }

  ngOnInit(): void {
    void this.fleet
      .refresh()
      .catch(() => undefined)
      .finally(() => this.ui.fleetLoaded.set(true));
    void this.tickets.load().catch(() => undefined).finally(() => this.tickets.loaded.set(true));
    void this.fleet.loadSettings();
    void this.control.load();
    void this.questions.load();
    void this.orch
      .loadSessions()
      .catch(() => undefined)
      .finally(() => this.ui.chatsLoaded.set(true));
    void this.setup.load();
    this.socket.connect();
  }

  ngOnDestroy(): void {
    this.socket.disconnect();
  }

  closeAddMachine(): void {
    this.ui.addMachineOpen.set(false);
    void this.setup.dismiss();
  }

  onStarted(taskId: string): void {
    const t = this.fleet.tasksById()[taskId];
    if (t) this.ui.openInCommand(t.sid);
  }
}

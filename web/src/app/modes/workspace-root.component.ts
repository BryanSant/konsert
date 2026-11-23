import { ChangeDetectionStrategy, Component, OnDestroy, OnInit, computed, effect, inject, untracked } from '@angular/core';
import { RouterOutlet } from '@angular/router';
import { KonsertSocketService } from '../core/services/konsert-socket.service';
import { ControlStore } from '../core/state/control.store';
import { FleetStore } from '../core/state/fleet.store';
import { QuestionsStore } from '../core/state/questions.store';
import { SetupStore } from '../core/state/setup.store';
import { TabsStore } from '../core/state/tabs.store';
import { ReloginDialogComponent } from '../features/chat/relogin-dialog.component';
import { QuestionsPanelComponent } from '../features/questions/questions-panel.component';
import { AddMachineModalComponent } from '../layout/add-machine-modal.component';
import { HotkeyService } from '../layout/hotkey.service';
import { MachineDetailModalComponent } from '../layout/machine-detail-modal.component';
import { SessionDispatchModalComponent } from '../layout/session-dispatch-modal.component';
import { SettingsModalComponent } from '../layout/settings-modal.component';
import { ModePaletteComponent } from './mode-palette.component';
import { ModeUiStore } from './mode-ui.store';

/**
 * Everything authenticated outside the classic shell: starts the live connection and the
 * stores, and hosts the dialogs every mode (and the mobile inbox) can open.
 */
@Component({
  selector: 'ks-workspace-root',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [
    RouterOutlet,
    QuestionsPanelComponent,
    ReloginDialogComponent,
    AddMachineModalComponent,
    SettingsModalComponent,
    SessionDispatchModalComponent,
    MachineDetailModalComponent,
    ModePaletteComponent,
  ],
  host: { class: 'block h-dvh w-screen overflow-hidden bg-[var(--ks-bg-canvas)] text-[var(--ks-text)]' },
  template: `
    <router-outlet />

    <ks-relogin-dialog />
    @if (questions.panelOpen()) {
      <ks-questions-panel />
    }
    @if (ui.addMachineOpen()) {
      <app-add-machine-modal
        [firstRun]="setup.needsSetup()"
        (close)="closeAddMachine()"
        (inspectMachine)="closeAddMachine(); ui.inspectedWorkerId.set($event)"
      />
    }
    @if (ui.settingsOpen()) {
      <app-settings-modal (close)="ui.settingsOpen.set(false)" />
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
    @if (ui.paletteOpen()) {
      <ks-mode-palette (close)="ui.paletteOpen.set(false)" />
    }
  `,
})
export class WorkspaceRootComponent implements OnInit, OnDestroy {
  readonly ui = inject(ModeUiStore);
  readonly fleet = inject(FleetStore);
  readonly setup = inject(SetupStore);
  readonly questions = inject(QuestionsStore);
  private readonly control = inject(ControlStore);
  private readonly socket = inject(KonsertSocketService);
  private readonly tabs = inject(TabsStore);

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
    // Shells that exit while watched close their Take Control state too.
    effect(() => {
      const tasks = this.fleet.tasks();
      untracked(() => this.tabs.closeExitedShells(tasks));
    });
  }

  ngOnInit(): void {
    void this.fleet
      .refresh()
      .catch(() => undefined)
      .finally(() => this.ui.fleetLoaded.set(true));
    void this.fleet.loadSettings();
    void this.control.load();
    void this.questions.load();
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

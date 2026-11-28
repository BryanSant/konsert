import { Injectable, computed, inject, signal } from '@angular/core';
import { Router } from '@angular/router';
import { SessionKind, Worker } from '../core/models/syre.models';
import { FleetStore } from '../core/state/fleet.store';
import { WorkItem } from '../core/state/work-logic';
import { SessionDispatchRequest } from '../layout/session-dispatch-modal.component';
import type { DevicesView } from '../features/auth/devices-dialog.component';
import type { MachinesDialogState, MachinesMode } from '../shared/machines/machines-dialog.component';

/**
 * Page-level UI state shared by the modes: which global dialogs are open and whether the
 * first fleet snapshot has arrived (so first run never flashes for a fleet that exists).
 */
@Injectable({ providedIn: 'root' })
export class ModeUiStore {
  private readonly router = inject(Router);
  private readonly fleet = inject(FleetStore);

  readonly fleetLoaded = signal(false);
  readonly chatsLoaded = signal(false);
  /** Both fleet (workers/tasks) and chat session lists have finished their initial load. */
  readonly workLoaded = computed(() => this.fleetLoaded() && this.chatsLoaded());
  readonly paletteOpen = signal(false);
  readonly settingsOpen = signal(false);
  readonly settingsSection = signal<'forges' | 'commander' | null>(null);
  /** The Machines dialog and the mode it shows (browse, select, add); null when closed. */
  readonly machinesDialog = signal<MachinesDialogState | null>(null);
  readonly sessionRequest = signal<SessionDispatchRequest | null>(null);
  readonly inspectedWorkerId = signal<string | null>(null);
  readonly projectsOpen = signal(false);
  readonly auditOpen = signal(false);
  /** Settings → Devices, opened on a view (list, Add a device, Connect an MCP client). */
  readonly devicesOpen = signal<DevicesView | null>(null);

  /** No machines at all once the fleet has loaded: first run. */
  readonly firstRun = computed(() => this.fleetLoaded() && this.fleet.workers().length === 0);
  readonly onlineCount = computed(() => this.fleet.workers().filter((w) => w.status === 'online').length);

  newSession(kind: SessionKind, worker: Worker | null = null): void {
    this.sessionRequest.set({ kind, worker });
  }

  /** Opens a session in Command (a tab next to the commander). */
  openInCommand(item: Pick<WorkItem, 'sid'> | string): void {
    const sid = typeof item === 'string' ? item : item.sid;
    void this.router.navigate(['/command', sid]);
  }

  /** Opens the Machines dialog: browse by default, 'add' for every "Add machine" link. */
  openMachines(mode: MachinesMode = 'browse', opts: Omit<MachinesDialogState, 'mode'> = {}): void {
    this.machinesDialog.set({ mode, ...opts });
  }

  openSettings(section: 'forges' | 'commander' | null = null): void {
    this.settingsSection.set(section);
    this.settingsOpen.set(true);
  }

  closeSettings(): void {
    this.settingsOpen.set(false);
    this.settingsSection.set(null);
  }

  closeAll(): void {
    this.paletteOpen.set(false);
    this.settingsOpen.set(false);
    this.settingsSection.set(null);
    this.machinesDialog.set(null);
    this.sessionRequest.set(null);
    this.inspectedWorkerId.set(null);
    this.projectsOpen.set(false);
    this.auditOpen.set(false);
    this.devicesOpen.set(null);
  }
}

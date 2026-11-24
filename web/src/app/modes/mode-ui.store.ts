import { Injectable, computed, inject, signal } from '@angular/core';
import { Router } from '@angular/router';
import { SessionKind, Worker } from '../core/models/konsert.models';
import { FleetStore } from '../core/state/fleet.store';
import { WorkItem } from '../core/state/work-logic';
import { SessionDispatchRequest } from '../layout/session-dispatch-modal.component';

/**
 * Page-level UI state shared by the modes: which global dialogs are open and whether the
 * first fleet snapshot has arrived (so first run never flashes for a fleet that exists).
 */
@Injectable({ providedIn: 'root' })
export class ModeUiStore {
  private readonly router = inject(Router);
  private readonly fleet = inject(FleetStore);

  readonly fleetLoaded = signal(false);
  readonly paletteOpen = signal(false);
  readonly settingsOpen = signal(false);
  readonly addMachineOpen = signal(false);
  readonly sessionRequest = signal<SessionDispatchRequest | null>(null);
  readonly inspectedWorkerId = signal<string | null>(null);
  readonly projectsOpen = signal(false);
  readonly auditOpen = signal(false);

  /** No machines at all once the fleet has loaded: first run. */
  readonly firstRun = computed(() => this.fleetLoaded() && this.fleet.workers().length === 0);
  readonly onlineCount = computed(() => this.fleet.workers().filter((w) => w.status === 'online').length);

  newSession(kind: SessionKind, worker: Worker | null = null): void {
    this.sessionRequest.set({ kind, worker });
  }

  /** Opens a session in Command (a tab next to the orchestrator). */
  openInCommand(item: Pick<WorkItem, 'sid'> | string): void {
    const sid = typeof item === 'string' ? item : item.sid;
    void this.router.navigate(['/command', sid]);
  }

  closeAll(): void {
    this.paletteOpen.set(false);
    this.settingsOpen.set(false);
    this.addMachineOpen.set(false);
    this.sessionRequest.set(null);
    this.inspectedWorkerId.set(null);
    this.projectsOpen.set(false);
    this.auditOpen.set(false);
  }
}

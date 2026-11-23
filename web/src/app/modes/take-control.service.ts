import { Injectable, inject } from '@angular/core';
import { ApiClientService } from '../core/services/api-client.service';
import { ControlStore } from '../core/state/control.store';
import { TakenControlStore } from '../core/state/taken-control.store';

/**
 * Take Control / Hand back for a task's terminal Taking
 * control routes typing to the PTY and has the hub hold the session (nothing automatic
 * touches it); handing back releases the hold and the agents are told.
 */
@Injectable({ providedIn: 'root' })
export class TakeControlService {
  private readonly tabs = inject(TakenControlStore);
  private readonly api = inject(ApiClientService);
  private readonly control = inject(ControlStore);

  has(taskId: string): boolean {
    return this.tabs.isInteractive(taskId);
  }

  async set(taskId: string, on: boolean): Promise<void> {
    this.tabs.setInteractiveMode(taskId, on);
    try {
      await this.api.setTaskControl(taskId, on);
      void this.control.loadHolds();
    } catch {
      // The local toggle still applies; the hub's hold is best effort.
    }
  }

  toggle(taskId: string): Promise<void> {
    return this.set(taskId, !this.has(taskId));
  }
}

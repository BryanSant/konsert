import { ChangeDetectionStrategy, Component, OnInit, inject, input, output, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Worker } from '../core/models/konsert.models';
import { ApiClientService } from '../core/services/api-client.service';
import { FleetStore } from '../core/state/fleet.store';
import { TabsStore } from '../core/state/tabs.store';
import { RemoteDirPickerComponent } from '../shared/remote-dir-picker.component';
import { ModalDialogComponent } from '../shared/modal-dialog.component';

/** Herdr-style per-machine persisted Space, opened with a first tab. */

@Component({
  selector: 'app-new-space-modal',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [FormsModule, RemoteDirPickerComponent, ModalDialogComponent],
  template: `
    <app-modal-dialog labelledBy="new-space-title" panelClass="w-full max-w-lg rounded-xl border border-[#2d3038] bg-[#1b1c20] p-5 space-y-4" (close)="close.emit()">
      <div class="flex items-center justify-between">
        <div>
          <h3 class="text-[14px] font-semibold text-[#ececed]" id="new-space-title">New Space</h3>
          <p class="text-[11px] text-[#8e9098]">Save a folder or worktree on a machine with multi-tab sessions.</p>
        </div>
        <button type="button" (click)="close.emit()" aria-label="Close dialog" class="ks-kbd cursor-pointer">Esc</button>
      </div>

      <form (ngSubmit)="submit()" class="space-y-3 text-[12px]">
        <div>
          <label for="new-space-modal-machine" class="block text-[11px] text-[#8e9098] mb-1">Machine</label>
          <select id="new-space-modal-machine"
            name="spWorker"
            [ngModel]="workerId()"
            (ngModelChange)="workerId.set($event)"
            class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed]"
          >
            @for (w of fleet.workers(); track w.id) {
              <option [value]="w.id">{{ w.name }} ({{ w.status }})</option>
            }
          </select>
        </div>

        <div>
          <label for="new-space-modal-directory-path" class="block text-[11px] text-[#8e9098] mb-1">Directory Path</label>
          <app-remote-dir-picker inputId="new-space-modal-directory-path"
            [workerId]="workerId()"
            [value]="path()"
            (valueChange)="path.set($event)"
            inputName="spPath"
            placeholder="~ or /home/user/src/my-repo"
          />
        </div>

        <div class="grid grid-cols-2 gap-3">
          <div>
            <label for="new-space-modal-space-name-optional" class="block text-[11px] text-[#8e9098] mb-1">Space Name (optional)</label>
            <input id="new-space-modal-space-name-optional"
              type="text"
              name="spName"
              [ngModel]="name()"
              (ngModelChange)="name.set($event)"
              placeholder="auto from folder"
              class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed]"
            />
          </div>
          <div>
            <label for="new-space-modal-first-tab" class="block text-[11px] text-[#8e9098] mb-1">First Tab</label>
            <select id="new-space-modal-first-tab"
              name="spAgent"
              [ngModel]="initialAgent()"
              (ngModelChange)="initialAgent.set($event)"
              class="w-full rounded-md border border-[#2c2e36] bg-[#111214] px-2.5 py-2 text-[#ececed]"
            >
              <option value="shell">shell (zsh / bash)</option>
              <option value="claude">claude</option>
              <option value="agy">agy</option>
              <option value="grok">grok</option>
              <option value="mock">mock</option>
            </select>
          </div>
        </div>

        @if (error()) {
          <div class="rounded-md border border-[#e05d5d]/30 bg-[#e05d5d]/10 px-3 py-2 text-[#f09595]">
            {{ error() }}
          </div>
        }

        <div class="flex justify-end gap-2 pt-1">
          <button type="button" (click)="close.emit()" class="rounded-md border border-[#2c2e36] px-3 py-1.5 text-[#a1a3ab] cursor-pointer">
            Cancel
          </button>
          <button
            type="submit"
            [disabled]="creating() || !workerId() || !path().trim()"
            class="rounded-md bg-[#ececed] hover:bg-white disabled:opacity-40 px-4 py-1.5 font-medium text-[#111214] cursor-pointer"
          >
            {{ creating() ? 'Creating...' : 'Create Space' }}
          </button>
        </div>
      </form>
    </app-modal-dialog>
  `,
})
export class NewSpaceModalComponent implements OnInit {
  private readonly api = inject(ApiClientService);
  private readonly tabs = inject(TabsStore);
  readonly fleet = inject(FleetStore);

  /** Preferred machine; defaults to the first online one. */
  readonly worker = input<Worker | null>(null);
  readonly close = output<void>();

  readonly workerId = signal('');
  readonly path = signal('~');
  readonly name = signal('');
  readonly initialAgent = signal('shell');
  readonly creating = signal(false);
  readonly error = signal('');

  ngOnInit(): void {
    const target =
      this.worker() || this.fleet.workers().find((w) => w.status === 'online') || this.fleet.workers()[0];
    this.workerId.set(target?.id || '');
  }

  async submit(): Promise<void> {
    this.creating.set(true);
    this.error.set('');
    try {
      const sp = await this.api.createSpace({
        worker_id: this.workerId(),
        path: this.path().trim() || '~',
        name: this.name().trim() || undefined,
      });
      this.fleet.upsertSpace(sp);
      const res = await this.api.dispatchTask({
        space_id: sp.id,
        agent: this.initialAgent(),
        mode: 'interactive',
        origin: 'direct',
        permission_mode: this.fleet.settings().default_permission_mode,
      });
      await this.fleet.refresh();
      const task = this.fleet.tasksById()[res.task_id];
      const w = this.fleet.workersById()[sp.worker_id];
      if (task) {
        this.tabs.openTaskTab(task, w?.name);
      }
      this.close.emit();
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Failed to create space');
    } finally {
      this.creating.set(false);
    }
  }
}

import { ChangeDetectionStrategy, Component, effect, inject, input, signal, untracked } from '@angular/core';
import { MachinePolicy, Worker } from '../../core/models/syre.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { DAYS, PolicyDraft, WORK_CLASSES, draftFromPolicy, policyFromDraft, timeZones } from './machine-policy';

/**
 * A machine's policy (SYR-V4RER): every field of protocol.MachinePolicy but the role,
 * which machine detail edits on its own row. Saved with PUT /api/workers/{id}/policy;
 * the hub's validation errors show here.
 */
@Component({
  selector: 'sy-machine-policy-editor',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'block' },
  template: `
    <form novalidate class="flex flex-col gap-2.5 text-[11.5px]" data-testid="machine-policy" (submit)="$event.preventDefault(); save()">
      <div class="grid grid-cols-2 sm:grid-cols-3 gap-2">
        <label class="flex flex-col gap-0.5 text-[#86888f]">
          <span>Max sessions</span>
          <input type="number" min="0" step="1" class="sy-field sy-field-sm" data-testid="policy-max-sessions" placeholder="no limit" [value]="d().maxSessions" (input)="set('maxSessions', $any($event.target).value)" />
        </label>
        <label class="flex flex-col gap-0.5 text-[#86888f]">
          <span>Max load average</span>
          <input type="number" min="0" step="0.1" class="sy-field sy-field-sm" data-testid="policy-max-load" placeholder="no limit" [value]="d().maxLoad" (input)="set('maxLoad', $any($event.target).value)" />
        </label>
        <label class="flex flex-col gap-0.5 text-[#86888f]">
          <span>Cores kept for you</span>
          <input type="number" min="0" step="0.5" class="sy-field sy-field-sm" data-testid="policy-reserve-cpu" placeholder="none" [value]="d().reserveCpu" (input)="set('reserveCpu', $any($event.target).value)" />
        </label>
        <label class="flex flex-col gap-0.5 text-[#86888f]">
          <span>Free RAM (MB)</span>
          <input type="number" min="0" step="256" class="sy-field sy-field-sm" data-testid="policy-min-free-ram" placeholder="none" [value]="d().minFreeRamMb" (input)="set('minFreeRamMb', $any($event.target).value)" />
        </label>
        <label class="flex flex-col gap-0.5 text-[#86888f]">
          <span>Reserved RAM (MB)</span>
          <input type="number" min="0" step="256" class="sy-field sy-field-sm" data-testid="policy-reserve-ram" placeholder="none" [value]="d().reserveRamMb" (input)="set('reserveRamMb', $any($event.target).value)" />
        </label>
      </div>
      <p class="m-0 text-[#86888f]">RAM kept free comes on top of the guardrails' survival reserve, never instead of it.</p>

      <fieldset class="flex flex-wrap items-center gap-3 m-0 p-0 border-0">
        <legend class="text-[#86888f] mb-1">Runs</legend>
        @for (c of workClasses; track c.id) {
          <label class="inline-flex items-center gap-1.5 text-[#c8cad0]">
            <input type="checkbox" [attr.data-testid]="'policy-work-' + c.id" [checked]="workOn(c.id)" (change)="toggleWork(c.id, $any($event.target).checked)" />
            {{ c.label }}
          </label>
        }
      </fieldset>

      <fieldset class="flex flex-col gap-1.5 m-0 p-0 border-0" data-testid="policy-quiet-hours">
        <legend class="text-[#86888f] mb-1">Quiet hours: no new work</legend>
        @for (w of d().quietHours; track $index; let i = $index) {
          <div class="flex flex-wrap items-center gap-1.5">
            <input type="time" class="sy-field sy-field-sm" [attr.aria-label]="'Quiet window ' + (i + 1) + ' start'" [attr.data-testid]="'quiet-start-' + i" [value]="w.start" (input)="setWindow(i, 'start', $any($event.target).value)" />
            <span class="text-[#86888f]">to</span>
            <input type="time" class="sy-field sy-field-sm" [attr.aria-label]="'Quiet window ' + (i + 1) + ' end'" [attr.data-testid]="'quiet-end-' + i" [value]="w.end" (input)="setWindow(i, 'end', $any($event.target).value)" />
            <span class="flex gap-0.5" role="group" [attr.aria-label]="'Days of quiet window ' + (i + 1)">
              @for (day of days; track day) {
                <button type="button" class="sy-btn sy-btn-sm !px-1.5 capitalize" [attr.aria-pressed]="w.days.includes(day)" [class.sy-btn-soft]="w.days.includes(day)" (click)="toggleDay(i, day)">{{ day.slice(0, 2) }}</button>
              }
            </span>
            <button type="button" class="sy-btn sy-btn-sm" [attr.aria-label]="'Remove quiet window ' + (i + 1)" (click)="removeWindow(i)">Remove</button>
          </div>
        }
        <div class="flex flex-wrap items-center gap-2">
          <button type="button" class="sy-btn sy-btn-sm" data-testid="quiet-add" (click)="addWindow()">Add quiet hours</button>
          @if (d().quietHours.length) {
            <label class="inline-flex items-center gap-1.5 text-[#86888f]">
              Time zone
              <select class="sy-field sy-field-sm max-w-[200px]" data-testid="policy-time-zone" [value]="d().timeZone" (change)="set('timeZone', $any($event.target).value)">
                @for (z of zones; track z) {
                  <option [value]="z" [selected]="z === d().timeZone">{{ z }}</option>
                }
              </select>
            </label>
            <span class="text-[#86888f]">No days picked: every day.</span>
          }
        </div>
      </fieldset>

      <div class="flex items-center gap-2">
        <button type="submit" class="sy-btn sy-btn-sm sy-btn-soft" data-testid="policy-save" [disabled]="busy()">{{ busy() ? 'Saving…' : 'Save policy' }}</button>
        @if (saved()) {
          <span class="text-[#8fd49d]" role="status" data-testid="policy-saved">Saved</span>
        }
      </div>
      @if (error()) {
        <p role="alert" class="m-0 text-[#f09595]" data-testid="policy-error">{{ error() }}</p>
      }
    </form>
  `,
})
export class MachinePolicyEditorComponent {
  private readonly api = inject(ApiClientService);
  readonly worker = input.required<Worker>();

  readonly workClasses = WORK_CLASSES;
  readonly days = DAYS;
  readonly zones = timeZones();
  readonly d = signal<PolicyDraft>(draftFromPolicy(null));
  readonly busy = signal(false);
  readonly error = signal('');
  readonly saved = signal(false);
  private loadedFor = '';

  constructor() {
    // Seed from the machine's policy when it changes machine (not on every heartbeat).
    effect(() => {
      const w = this.worker();
      untracked(() => {
        if (w.id === this.loadedFor) return;
        this.loadedFor = w.id;
        this.d.set(draftFromPolicy(w.policy));
        this.error.set('');
      });
    });
  }

  set<K extends keyof PolicyDraft>(key: K, value: PolicyDraft[K]): void {
    this.d.update((d) => ({ ...d, [key]: value }));
    this.saved.set(false);
  }

  /** No class picked means all of them. */
  workOn(id: string): boolean {
    const w = this.d().allowedWork;
    return w.length === 0 || w.includes(id);
  }

  toggleWork(id: string, on: boolean): void {
    const all = WORK_CLASSES.map((c) => c.id as string);
    const cur = this.d().allowedWork.length ? this.d().allowedWork : all;
    const next = on ? [...new Set([...cur, id])] : cur.filter((x) => x !== id);
    this.set('allowedWork', next.length === all.length ? [] : next);
  }

  addWindow(): void {
    this.set('quietHours', [...this.d().quietHours, { start: '22:00', end: '07:00', days: [] }]);
  }

  removeWindow(i: number): void {
    this.set(
      'quietHours',
      this.d().quietHours.filter((_, j) => j !== i),
    );
  }

  setWindow(i: number, key: 'start' | 'end', value: string): void {
    this.set(
      'quietHours',
      this.d().quietHours.map((w, j) => (j === i ? { ...w, [key]: value } : w)),
    );
  }

  toggleDay(i: number, day: string): void {
    this.set(
      'quietHours',
      this.d().quietHours.map((w, j) => (j === i ? { ...w, days: w.days.includes(day) ? w.days.filter((x) => x !== day) : [...w.days, day] } : w)),
    );
  }

  async save(): Promise<void> {
    this.error.set('');
    this.saved.set(false);
    this.busy.set(true);
    try {
      // The role and anything else the hub has: read fresh so a role change isn't undone.
      const { policy: current } = await this.api.getWorkerPolicy(this.worker().id);
      const res = policyFromDraft(this.d(), current as MachinePolicy);
      if (res.error || !res.policy) {
        this.error.set(res.error ?? 'Could not build the policy');
        return;
      }
      await this.api.setWorkerPolicy(this.worker().id, res.policy);
      this.saved.set(true);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not save the policy');
    } finally {
      this.busy.set(false);
    }
  }
}

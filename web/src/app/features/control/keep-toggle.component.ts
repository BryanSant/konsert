import { ChangeDetectionStrategy, Component, inject, input, signal } from '@angular/core';
import { ApiClientService } from '../../core/services/api-client.service';
import { FleetStore } from '../../core/state/fleet.store';
import { CommanderStore } from '../../core/state/commander.store';
import { IconComponent } from '../../shared/icon.component';
import { TooltipDirective } from '../../shared/tooltip.directive';

/**
 * Keep (pin) toggle of a task or chat (SYR-CSCWP): pinned work is never compacted by
 * history retention. The hub pushes the updated record to every browser; the response
 * is applied here too so the button flips at once.
 */
@Component({
  selector: 'sy-keep-toggle',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective],
  template: `
    <button
      type="button"
      [class]="compact() ? 'sy-icon-btn' : 'sy-btn sy-btn-sm'"
      [class.text-[var(--sy-accent)]]="keep()"
      [attr.aria-pressed]="keep()"
      [attr.aria-label]="keep() ? 'Kept: never compacted (click to stop keeping)' : 'Keep: never compact this ' + kind()"
      [syTooltip]="keep() ? 'Kept in full: history retention never compacts it' : 'Keep in full: history retention never compacts it'"
      data-testid="toggle-keep"
      [disabled]="busy()"
      (click)="toggle()"
    >
      <app-icon name="bookmark" [size]="compact() ? 14 : 12" [filled]="keep()" />
      @if (!compact()) {
        <span>{{ keep() ? 'Kept' : 'Keep' }}</span>
      }
    </button>
    @if (error()) {
      <span class="text-[11px] text-[var(--sy-danger)]" role="alert">{{ error() }}</span>
    }
  `,
  host: { class: 'inline-flex items-center gap-1' },
})
export class KeepToggleComponent {
  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);
  private readonly orch = inject(CommanderStore);

  readonly kind = input.required<'task' | 'chat'>();
  readonly id = input.required<string>();
  readonly keep = input(false);
  readonly compact = input(false);

  readonly busy = signal(false);
  readonly error = signal('');

  async toggle(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      const next = !this.keep();
      if (this.kind() === 'task') {
        const res = await this.api.setKeep('task', this.id(), next);
        if (res.task) this.fleet.upsertTask(res.task);
      } else {
        const res = await this.api.setKeep('chat', this.id(), next);
        if (res.chat) this.orch.upsertSession(res.chat);
      }
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : 'Could not change Keep');
    } finally {
      this.busy.set(false);
    }
  }
}

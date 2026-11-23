import { ChangeDetectionStrategy, Component, computed, inject, input, signal } from '@angular/core';
import { VETO_REASONS, VetoReason } from '../../core/models/control.models';
import { ControlStore } from '../../core/state/control.store';
import { IconComponent } from '../../shared/icon.component';
import { PopoverComponent, PopoverTriggerDirective } from '../../shared/popover.component';
import { TooltipDirective } from '../../shared/tooltip.directive';

export type SessionKindForControls = 'task' | 'chat';

const TASK_SUSPENDABLE = new Set(['running', 'waiting_input', 'idle', 'assigned']);
const TASK_ACTIVE = new Set([
  'queued',
  'assigned',
  'running',
  'waiting_input',
  'idle',
  'suspended',
]);
const CHAT_SUSPENDABLE = new Set(['working', 'needs_you', 'starting']);
const CHAT_VETOABLE = new Set(['working', 'needs_you', 'starting', 'suspended']);

/** Which intervention controls apply to a session in a given status. */
export function sessionActions(
  kind: SessionKindForControls,
  status: string,
): {
  suspend: boolean;
  resume: boolean;
  cancel: boolean;
  veto: boolean;
} {
  if (kind === 'task') {
    return {
      suspend: TASK_SUSPENDABLE.has(status),
      resume: status === 'suspended',
      cancel: TASK_ACTIVE.has(status),
      veto: TASK_ACTIVE.has(status),
    };
  }
  return {
    suspend: CHAT_SUSPENDABLE.has(status),
    resume: status === 'suspended',
    cancel: false,
    veto: CHAT_VETOABLE.has(status),
  };
}

/**
 * Intervention controls shown on every task and chat header (PRD §5.8a): Suspend/Resume,
 * Cancel (tasks; chats keep their own Stop), and Veto with a reason. Flat until hovered.
 */
@Component({
  selector: 'ks-session-controls',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, PopoverComponent, PopoverTriggerDirective, TooltipDirective],
  host: { class: 'inline-flex items-center gap-1 shrink-0' },
  template: `
    @if (actions().suspend) {
      <button
        type="button"
        class="ks-btn ks-btn-sm"
        data-testid="session-suspend"
        [disabled]="busy()"
        (click)="suspend()"
        ksTooltip="Freeze its processes without losing work"
        aria-label="Suspend"
      >
        <app-icon name="pause" [size]="11" />
        @if (!compact()) {
          <span>Suspend</span>
        }
      </button>
    }
    @if (actions().resume) {
      <button
        type="button"
        class="ks-btn ks-btn-sm text-[var(--ks-violet)]"
        data-testid="session-resume"
        [disabled]="busy()"
        (click)="resume()"
        ksTooltip="Continue where it was frozen"
        aria-label="Resume"
      >
        <app-icon name="play" [size]="11" />
        @if (!compact()) {
          <span>Resume</span>
        }
      </button>
    }
    @if (actions().cancel && !hideCancel()) {
      <button
        type="button"
        class="ks-btn ks-btn-sm ks-btn-danger"
        data-testid="session-cancel"
        [disabled]="busy()"
        (click)="cancel()"
        ksTooltip="Stop the remote process tree"
        aria-label="Cancel"
      >
        <app-icon name="stop" [size]="10" />
        @if (!compact()) {
          <span>Cancel</span>
        }
      </button>
    }
    @if (showHold()) {
      @if (hold(); as h) {
        <button
          type="button"
          class="ks-btn ks-btn-sm text-[#e6b763]"
          data-testid="session-release-hold"
          [disabled]="control.isBusy(h.id)"
          (click)="control.releaseHold(h)"
          ksTooltip="Let automatic actions (retries, orchestrator steering) touch it again"
          aria-label="Release hold"
        >
          <app-icon name="unlock" [size]="11" />
          @if (!compact()) {
            <span>Release hold</span>
          }
        </button>
      } @else {
        <button
          type="button"
          class="ks-btn ks-btn-sm"
          data-testid="session-hold-btn"
          [disabled]="busy()"
          (click)="placeHold()"
          ksTooltip="Nothing automatic touches it (retries, orchestrator steering) until you release it; it keeps running"
          aria-label="Hold"
        >
          <app-icon name="lock" [size]="11" />
          @if (!compact()) {
            <span>Hold</span>
          }
        </button>
      }
    }
    @if (actions().veto || vetoAlways()) {
      <ks-popover
        label="Veto"
        role="dialog"
        placement="bottom-end"
        panelClass="w-[280px] p-3"
        [closeOnSelect]="false"
        [(open)]="vetoOpen"
      >
        <button
          type="button"
          ksPopoverTrigger
          class="ks-btn ks-btn-sm ks-btn-danger"
          data-testid="session-veto"
          [disabled]="busy()"
          ksTooltip="Stop it and tell the orchestrator why, so it is not simply retried"
          aria-label="Veto"
        >
          <app-icon name="ban" [size]="11" />
          @if (!compact()) {
            <span>Veto</span>
          }
        </button>
        <form class="space-y-2.5" (submit)="$event.preventDefault(); veto()">
          <div class="text-[12px] font-semibold text-[var(--ks-text)]">Veto: why?</div>
          <div class="flex flex-wrap gap-1.5" role="radiogroup" aria-label="Veto reason">
            @for (r of reasons; track r.id) {
              <button
                type="button"
                role="radio"
                class="ks-chip"
                [attr.aria-checked]="reason() === r.id"
                [class.bg-[var(--ks-red-soft)]]="reason() === r.id"
                [class.text-[#f6b8b8]]="reason() === r.id"
                [attr.data-testid]="'veto-reason-' + r.id"
                (click)="reason.set(r.id)"
              >
                {{ r.label }}
              </button>
            }
          </div>
          <label class="ks-sr-only" [attr.for]="noteId">Note for the orchestrator</label>
          <textarea
            [id]="noteId"
            rows="2"
            data-testid="veto-note"
            placeholder="Optional note for the orchestrator"
            class="ks-field w-full resize-none text-[12px]"
            [value]="note()"
            (input)="note.set($any($event.target).value)"
          ></textarea>
          <div class="flex justify-end gap-1.5">
            <button type="button" class="ks-btn ks-btn-sm" (click)="vetoOpen.set(false)">
              Cancel
            </button>
            <button
              type="submit"
              class="ks-btn ks-btn-sm ks-btn-primary"
              data-testid="veto-submit"
              [disabled]="!reason() || busy()"
            >
              Veto
            </button>
          </div>
        </form>
      </ks-popover>
    }
  `,
})
export class SessionControlsComponent {
  readonly kind = input.required<SessionKindForControls>();
  readonly sessionId = input.required<string>();
  readonly status = input<string>('');
  /** Icon-only buttons on narrow headers. */
  readonly compact = input(false);
  /** Also offer Hold / Release hold. */
  readonly showHold = input(false);
  /** Offer Veto even once the session finished (reviews: "don't do it this way again"). */
  readonly vetoAlways = input(false);
  /** Leave out Cancel (where the host already offers Stop). */
  readonly hideCancel = input(false);

  readonly control = inject(ControlStore);
  readonly hold = computed(() => this.control.sessionHold(this.sessionId()));

  private static seq = 0;
  readonly noteId = `ks-veto-note-${++SessionControlsComponent.seq}`;
  readonly reasons = VETO_REASONS;
  readonly vetoOpen = signal(false);
  readonly reason = signal<VetoReason | ''>('');
  readonly note = signal('');

  readonly actions = computed(() => sessionActions(this.kind(), this.status() || ''));
  readonly busy = computed(() => this.control.isBusy(this.sessionId()));

  suspend(): Promise<boolean> {
    const id = this.sessionId();
    return this.kind() === 'task' ? this.control.suspendTask(id) : this.control.suspendChat(id);
  }

  resume(): Promise<boolean> {
    const id = this.sessionId();
    return this.kind() === 'task' ? this.control.resumeTask(id) : this.control.resumeChat(id);
  }

  async placeHold(): Promise<boolean> {
    return !!(await this.control.holdSession(this.kind(), this.sessionId()));
  }

  cancel(): Promise<boolean> {
    return this.control.cancelTask(this.sessionId());
  }

  async veto(): Promise<boolean> {
    const reason = this.reason();
    if (!reason) return false;
    const id = this.sessionId();
    const note = this.note().trim() || undefined;
    const ok =
      this.kind() === 'task'
        ? await this.control.vetoTask(id, reason, note)
        : await this.control.vetoChat(id, reason, note);
    if (ok) {
      this.vetoOpen.set(false);
      this.reason.set('');
      this.note.set('');
    }
    return ok;
  }
}

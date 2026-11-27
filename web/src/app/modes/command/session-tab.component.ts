import { ChangeDetectionStrategy, Component, computed, inject, input, signal } from '@angular/core';
import { sidUrl } from '../../core/routing/sid-links';
import { AgentChatStore } from '../../core/state/agent-chat.store';
import { WorkItem } from '../../core/state/work-logic';
import { WallStore } from '../wall/wall.store';
import { SessionChatComponent } from '../../features/chat/session-chat.component';
import { SessionControlsComponent } from '../../features/control/session-controls.component';
import { KeepToggleComponent } from '../../features/control/keep-toggle.component';
import { WorkStore } from '../../core/state/work.store';
import { XtermPaneComponent } from '../../features/terminal/xterm-pane.component';
import { IconComponent } from '../../shared/icon.component';
import { TooltipDirective } from '../../shared/tooltip.directive';
import { ControlLabelComponent } from '../control-label.component';
import { ItemSpinnerComponent } from '../item-spinner.component';
import { TakeControlService } from '../take-control.service';

/**
 * An agent session as a Command tab: the GOAL / NOW strip with its controls, and the
 * session itself (the live terminal for agent CLIs and shells, the transcript and a reply
 * box for agent chats).
 */
@Component({
  selector: 'sy-session-tab',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [ItemSpinnerComponent, ControlLabelComponent, SessionControlsComponent, KeepToggleComponent, XtermPaneComponent, SessionChatComponent, IconComponent, TooltipDirective],
  host: { class: 'flex flex-col min-h-0 h-full' },
  template: `
    @let it = item();
    <div class="flex flex-wrap items-center gap-x-4 gap-y-1.5 px-4 py-2 shrink-0 bg-[var(--sy-bg-chrome)] border-b border-[var(--sy-hairline)] text-[12.5px]" data-testid="session-strip">
      <sy-item-spinner [item]="it" [size]="22" class="shrink-0" />
      <sy-control-label [label]="it.control" />
      <span class="min-w-0 max-w-[40%] truncate"><span class="font-mono-ide text-[10.5px] text-[var(--sy-text-muted)] mr-1">GOAL</span>{{ it.goal }}</span>
      <span class="min-w-0 flex-1 truncate" aria-live="polite" [syTooltip]="it.nowTitle"><span class="font-mono-ide text-[10.5px] text-[var(--sy-text-muted)] mr-1">NOW</span>{{ it.now }}</span>
      <span class="hidden xl:inline font-mono-ide text-[11px] text-[var(--sy-text-muted)]">{{ it.sid }} · {{ it.machine }}</span>
      @if (it.observed) {
        <span class="text-[11.5px] text-[var(--sy-text-muted)]">observed: started outside Syre</span>
      }
      <!-- Wraps on a phone: one unwrapped row is wider than the screen, and focusing the
           terminal then scrolls the whole tab sideways under the quick keys (SYR-WKJM6). -->
      <div class="flex flex-wrap items-center gap-1 min-w-0 max-w-full">
        <sy-session-controls [kind]="it.kind" [sessionId]="it.id" [status]="it.status" [showHold]="it.live" />
        @if (it.kind === 'chat' && (it.status === 'working' || it.status === 'starting' || it.status === 'needs_you')) {
          <button type="button" class="sy-btn sy-btn-sm sy-btn-danger" data-testid="session-stop-turn" (click)="agentChat.cancel(it.id)" aria-label="Stop the turn">
            <app-icon name="stop" [size]="10" />
            Stop
          </button>
        }
        @if (it.kind === 'task' && it.live && !it.observed) {
          <button
            type="button"
            class="sy-btn sy-btn-sm"
            data-testid="take-control"
            [attr.aria-pressed]="hasControl()"
            [class.text-[#cfe2ff]]="hasControl()"
            (click)="takeControl.toggle(it.id)"
            [syTooltip]="hasControl() ? 'Hand the terminal back to the agent' : 'Type into this terminal yourself; nothing automatic touches it meanwhile'"
          >
            <app-icon [name]="hasControl() ? 'unlock' : 'hand'" [size]="11" />
            {{ hasControl() ? 'Hand back' : 'Take control' }}
          </button>
        }
        @if (it.sid) {
          <button
            type="button"
            class="sy-icon-btn sy-icon-btn-sm"
            [attr.aria-label]="copiedLink() ? 'Link copied' : 'Copy link'"
            [syTooltip]="copiedLink() ? 'Copied link' : 'Copy link to this session'"
            data-testid="copy-session-link"
            (click)="copyLink(it.sid)"
          >
            <app-icon [name]="copiedLink() ? 'check' : 'link'" [size]="13" />
          </button>
        }
        <button
          type="button"
          class="sy-icon-btn sy-icon-btn-sm"
          [attr.aria-pressed]="pinned()"
          [attr.aria-label]="pinned() ? 'Unpin from the Wall' : 'Pin to the Wall'"
          [syTooltip]="pinned() ? 'Unpin from the Wall' : 'Pin to the Wall'"
          data-testid="pin-to-wall"
          (click)="wall.togglePin(it.id)"
        >
          <app-icon name="split" [size]="13" />
        </button>
        <sy-keep-toggle [kind]="it.kind" [id]="it.id" [keep]="work.isKept(it.id)" [compact]="true" />
      </div>
    </div>
    <div class="flex-1 min-h-0">
      @if (it.kind === 'chat') {
        <sy-session-chat class="h-full" [chatId]="it.id" />
      } @else {
        @defer (on immediate) {
          <app-xterm-pane class="block h-full" [taskId]="it.id" [visible]="true" [bare]="true" />
        } @placeholder {
          <div class="h-full flex items-center justify-center text-[12px] text-[var(--sy-text-muted)]" role="status">Loading terminal…</div>
        }
      }
    </div>
  `,
})
export class SessionTabComponent {
  readonly item = input.required<WorkItem>();
  readonly agentChat = inject(AgentChatStore);
  readonly takeControl = inject(TakeControlService);
  readonly wall = inject(WallStore);
  readonly work = inject(WorkStore);
  readonly hasControl = computed(() => this.takeControl.has(this.item().id));
  readonly pinned = computed(() => this.wall.isPinned(this.item().id));
  readonly copiedLink = signal(false);

  async copyLink(sid: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(sidUrl(sid));
      this.copiedLink.set(true);
      setTimeout(() => this.copiedLink.set(false), 1600);
    } catch {
      // ignore
    }
  }
}

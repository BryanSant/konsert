import { ChangeDetectionStrategy, Component, DestroyRef, computed, effect, inject, input, untracked } from '@angular/core';
import { ChatTranscriptComponent } from '../../features/chat/transcript/chat-transcript.component';
import { OBSERVED_RENEW_MS, ObservedTranscriptStore } from '../../core/state/observed-transcript.store';

/**
 * An observed session's live transcript, read-only (SYR-XHAHC): prompts, replies and tool
 * calls as the agent writes them to its own session file, relayed within a couple of
 * seconds. When the machine can't provide it, says so plainly.
 */
@Component({
  selector: 'sy-observed-transcript',
  standalone: true,
  imports: [ChatTranscriptComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (view(); as v) {
      @if (v.loading) {
        <div class="h-full flex items-center justify-center text-[12px] text-[var(--sy-text-muted)]" role="status">Reading the session's transcript…</div>
      } @else if (!v.available) {
        <div class="h-full flex items-center justify-center p-6" data-testid="observed-transcript-unavailable">
          <p class="m-0 max-w-md text-center text-[12.5px] text-[var(--sy-text-secondary)]">
            <span class="block font-medium text-[var(--sy-text)]">Transcript unavailable</span>
            {{ v.reason || 'The machine running this session can not provide its transcript.' }}
          </p>
        </div>
      } @else {
        <div class="h-full flex flex-col min-h-0" data-testid="observed-transcript">
          <p class="m-0 shrink-0 px-4 py-1.5 text-[11px] text-[var(--sy-text-muted)] border-b border-[var(--sy-hairline)]">
            Read-only: relayed from the agent's own session file{{ v.truncated ? ' (latest part only)' : '' }}. Steer it from its own terminal, or with the controls above.
          </p>
          <sy-chat-transcript class="flex-1 min-h-0" [items]="v.transcript.items" [status]="v.transcript.status" [agent]="agent()" />
        </div>
      }
    }
  `,
})
export class ObservedTranscriptComponent {
  private readonly store = inject(ObservedTranscriptStore);
  readonly taskId = input.required<string>();
  readonly agent = input('');
  readonly view = computed(() => this.store.views()[this.taskId()]);

  constructor() {
    const destroyRef = inject(DestroyRef);
    let timer: ReturnType<typeof setInterval> | null = null;
    effect((onCleanup) => {
      const id = this.taskId();
      // untracked: load reads the views it updates; tracking them would reload (and drop
      // live events) on every change.
      untracked(() => void this.store.load(id));
      // While the tab is open, keep the machine's watch alive (and catch up after gaps).
      timer = setInterval(() => void this.store.load(id), OBSERVED_RENEW_MS);
      onCleanup(() => {
        if (timer) clearInterval(timer);
      });
    });
    destroyRef.onDestroy(() => {
      if (timer) clearInterval(timer);
    });
  }
}

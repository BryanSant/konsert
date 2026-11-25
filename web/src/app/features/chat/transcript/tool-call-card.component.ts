import { ChangeDetectionStrategy, Component, computed, input, signal } from '@angular/core';
import { AgentToolCall } from '../../../core/models/agent-chat.models';
import { IconComponent } from '../../../shared/icon.component';

export function toolIcon(kind: string | undefined, name = ''): string {
  switch (kind) {
    case 'read':
      return 'file';
    case 'edit':
      return 'file-edit';
    case 'execute':
      return 'shell';
    case 'search':
      return 'search';
    case 'fetch':
      return 'globe';
    case 'think':
      return 'brain';
  }
  const n = name.toLowerCase();
  if (/bash|shell|exec|run/.test(n)) return 'shell';
  if (/edit|write|patch/.test(n)) return 'file-edit';
  if (/read|view|cat/.test(n)) return 'file';
  if (/grep|glob|search|find/.test(n)) return 'search';
  if (/fetch|web|http/.test(n)) return 'globe';
  return 'wrench';
}

let nextId = 0;

/** One tool call: icon by kind, title, live status, expandable input/output and paths. */
@Component({
  selector: 'sy-tool-call-card',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent],
  template: `
    <div class="rounded-[var(--sy-radius-lg)] bg-[var(--sy-surface-1)] text-[12px]" [attr.data-tool-status]="call().status">
      <button
        type="button"
        class="flex w-full items-center gap-2.5 rounded-[var(--sy-radius-lg)] px-3 py-2 text-left cursor-pointer hover:bg-[var(--sy-raised)] transition-colors sy-focusable"
        [attr.aria-expanded]="expanded()"
        [attr.aria-controls]="bodyId"
        (click)="expanded.set(!expanded())"
      >
        <span class="text-[var(--sy-text-secondary)] shrink-0"><app-icon [name]="icon()" [size]="14" /></span>
        <span class="flex-1 min-w-0 truncate text-[var(--sy-text)]">{{ call().title || call().name }}</span>
        @if (call().title && call().name) {
          <span class="hidden sm:inline font-mono-ide text-[10.5px] text-[var(--sy-text-muted)] shrink-0">{{ call().name }}</span>
        }
        <span class="shrink-0 inline-flex items-center" [attr.aria-label]="statusLabel()" role="img">
          @switch (statusClass()) {
            @case ('running') {
              <span class="sy-spinner text-[var(--sy-blue)]" style="width: 11px; height: 11px"></span>
            }
            @case ('completed') {
              <span class="text-[var(--sy-green)]"><app-icon name="check" [size]="13" /></span>
            }
            @case ('failed') {
              <span class="text-[var(--sy-red)]"><app-icon name="x-circle" [size]="13" /></span>
            }
            @default {
              <span class="h-2 w-2 rounded-full border border-[var(--sy-text-muted)]"></span>
            }
          }
        </span>
        <span class="text-[var(--sy-text-muted)] shrink-0 transition-transform" [class.rotate-90]="expanded()">
          <app-icon name="chevron-right" [size]="11" />
        </span>
      </button>
      @if (expanded()) {
        <div [id]="bodyId" class="px-3 pb-3 space-y-2">
          @if (call().paths?.length) {
            <div class="flex flex-wrap gap-1">
              @for (p of call().paths; track p) {
                <span class="rounded-full bg-[var(--sy-raised)] px-2 py-0.5 font-mono-ide text-[10.5px] text-[var(--sy-text-secondary)] max-w-full truncate">{{ p }}</span>
              }
            </div>
          }
          @if (call().input) {
            <div>
              <div class="mb-1 text-[10.5px] uppercase tracking-wide text-[var(--sy-text-muted)]">Input</div>
              <pre class="max-h-56 overflow-auto rounded-[var(--sy-radius-md)] bg-black/30 p-2.5 font-mono-ide text-[11px] leading-relaxed text-[#c9cbd2] whitespace-pre-wrap break-words">{{ pretty(call().input!) }}</pre>
            </div>
          }
          @if (call().output) {
            <div>
              <div class="mb-1 text-[10.5px] uppercase tracking-wide text-[var(--sy-text-muted)]">Output</div>
              <pre class="max-h-72 overflow-auto rounded-[var(--sy-radius-md)] bg-black/30 p-2.5 font-mono-ide text-[11px] leading-relaxed text-[#c9cbd2] whitespace-pre-wrap break-words">{{ call().output }}</pre>
            </div>
          }
          @if (!call().input && !call().output && !call().paths?.length) {
            <div class="text-[11.5px] text-[var(--sy-text-muted)]">No details reported.</div>
          }
        </div>
      }
    </div>
  `,
})
export class ToolCallCardComponent {
  readonly call = input.required<AgentToolCall>();
  readonly expanded = signal(false);
  readonly bodyId = `sy-tool-${++nextId}`;

  readonly icon = computed(() => toolIcon(this.call().kind, this.call().name));
  readonly statusClass = computed(() => {
    const s = this.call().status;
    if (s === 'running' || s === 'pending') return 'running';
    if (s === 'completed') return 'completed';
    if (s === 'failed') return 'failed';
    return 'other';
  });
  readonly statusLabel = computed(() => {
    switch (this.statusClass()) {
      case 'running':
        return this.call().status === 'pending' ? 'Pending' : 'Running';
      case 'completed':
        return 'Completed';
      case 'failed':
        return 'Failed';
      default:
        return 'Stopped';
    }
  });

  pretty(raw: string): string {
    try {
      return JSON.stringify(JSON.parse(raw), null, 2);
    } catch {
      return raw;
    }
  }
}

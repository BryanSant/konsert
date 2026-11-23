import { ChangeDetectionStrategy, Component, ElementRef, computed, inject, output } from '@angular/core';
import { NotificationService } from '../core/services/notification.service';
import { FleetStore } from '../core/state/fleet.store';
import { TabsStore } from '../core/state/tabs.store';
import { IconComponent } from '../shared/icon.component';
import { TooltipDirective } from '../shared/tooltip.directive';

/** Herdr-style tab strip in the top bar (scoped to the active Space when one is selected). */
@Component({
  selector: 'app-tab-strip',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [TooltipDirective, IconComponent],
  host: { class: 'flex-1 min-w-0 h-full' },
  template: `
    <div class="flex-1 flex items-center gap-1.5 px-3 h-full overflow-x-auto [scrollbar-width:none] min-w-0">
      @if (activeSpace(); as sp) {
        <span class="inline-flex items-center gap-1.5 rounded-[var(--ks-radius-sm)] bg-[var(--ks-raised)] px-2 py-1 text-[11.5px] text-[#b4b6be] shrink-0 mr-1">
          <span class="h-3.5 w-3.5 rounded flex items-center justify-center text-[9px] font-semibold bg-[#1f8a8c]/85 text-white">
            {{ sp.name === '~' ? '~' : sp.name.slice(0, 1).toUpperCase() }}
          </span>
          <span class="font-medium text-[#e4e4e7]">{{ sp.name }}</span>
        </span>
      }

      <div role="tablist" aria-label="Open sessions" class="flex items-center gap-1" (keydown)="onKeydown($event)">
        @for (tab of tabs.visibleTabs(); track tab.id) {
          @let selected = tabs.activeTabId() === tab.id;
          <div
            role="presentation"
            (click)="selectTab(tab.id)"
            class="group flex items-center gap-2 h-[30px] px-3 text-[12.5px] rounded-[var(--ks-radius-md)] cursor-pointer shrink-0 transition-all duration-150 focus-within:ring-1 focus-within:ring-[#4a668a]"
            [class.bg-[var(--ks-raised)]]="selected || tabs.splitTabId() === tab.id"
            [class.text-[#f4f4f5]]="selected"
            [class.text-[#b4b6be]]="tabs.splitTabId() === tab.id && !selected"
            [class.text-[#8e9098]]="!selected && tabs.splitTabId() !== tab.id"
            [class.hover:bg-white/[0.04]]="!selected && tabs.splitTabId() !== tab.id"
            [class.hover:text-[#d4d4d8]]="!selected && tabs.splitTabId() !== tab.id"
          >
            <button
              type="button"
              role="tab"
              [id]="'ks-tab-' + tab.id"
              [attr.aria-selected]="selected"
              [attr.aria-controls]="'ks-tabpanel-' + tab.id"
              [tabindex]="selected ? 0 : -1"
              class="flex items-center gap-1.5 bg-transparent text-inherit cursor-pointer focus:outline-none"
            >
              @if (tab.kind === 'orchestrator') {
                <app-icon name="orchestrator" [size]="13" class="text-[#60a5fa]" />
              } @else {
                <app-icon [name]="tab.agent || 'shell'" [size]="13" />
              }
              <span class="max-w-[150px] truncate">{{ tab.title }}</span>
              @if (notifications.isUnread(tab.id) && !selected) {
                <span class="h-1.5 w-1.5 rounded-full bg-[#d9a043] shrink-0" role="img" aria-label="Attention needed" ksTooltip="Attention needed"></span>
                <span class="ks-sr-only">(needs attention)</span>
              }
              @if (tabs.splitTabId() === tab.id) {
                <span class="rounded bg-[#1a1c22] px-1 text-[9px] font-mono-ide text-[#8e9098]">split</span>
              }
            </button>
            @if (!tab.pinned) {
              <!-- Mouse affordance; keyboard users close the focused tab with Delete. -->
              <button
                type="button"
                tabindex="-1"
                aria-hidden="true"
                ksTooltip="Close tab (Delete)" aria-label="Close tab (Delete)"
                (click)="$event.stopPropagation(); tabs.closeTab(tab.id)"
                class="ks-icon-btn ks-icon-btn-sm !w-5 !h-5 -mr-1 opacity-60 group-hover:opacity-100"
              >
                <app-icon name="close" [size]="11" />
              </button>
            }
          </div>
        }
      </div>

      <button
        type="button"
        (click)="newTab.emit()"
        [ksTooltip]="activeSpace() ? 'New session in ' + activeSpace()!.name : 'New chat or terminal session'"
        [attr.aria-label]="activeSpace() ? 'New session in ' + activeSpace()!.name : 'New chat or terminal session'"
        class="ks-icon-btn shrink-0"
      >
        <app-icon name="plus" [size]="15" />
      </button>

      @if (tabs.activeTab()?.kind === 'terminal' || tabs.splitTabId()) {
        <button
          type="button"
          (click)="toggleSplit.emit()"
          [ksTooltip]="tabs.splitTabId() ? splitCloseTip : splitOpenTip"
          [attr.aria-label]="tabs.splitTabId() ? 'Close split pane' : 'Split terminal side by side'"
          [attr.aria-pressed]="!!tabs.splitTabId()"
          class="ks-icon-btn hidden md:inline-flex shrink-0"
        >
          <app-icon name="split" [size]="15" />
        </button>
      }
    </div>
  `,
})
export class TabStripComponent {
  readonly tabs = inject(TabsStore);
  readonly notifications = inject(NotificationService);
  private readonly fleet = inject(FleetStore);
  private readonly host = inject<ElementRef<HTMLElement>>(ElementRef);

  readonly splitOpenTip = 'Split terminal side-by-side (⌘\\)';
  readonly splitCloseTip = 'Close split pane (⌘\\)';
  readonly newTab = output<void>();
  readonly toggleSplit = output<void>();

  readonly activeSpace = computed(() => {
    const sid = this.tabs.activeSpaceId();
    return sid ? this.fleet.spacesById()[sid] : undefined;
  });

  selectTab(id: string): void {
    this.tabs.selectTab(id);
    this.notifications.markRead(id);
  }

  /** WAI-ARIA tabs pattern: arrows/Home/End move and activate, Delete closes. */
  onKeydown(ev: KeyboardEvent): void {
    const list = this.tabs.visibleTabs();
    if (list.length === 0) return;
    const current = Math.max(
      0,
      list.findIndex((t) => t.id === this.tabs.activeTabId()),
    );
    let next = -1;
    switch (ev.key) {
      case 'ArrowRight':
        next = (current + 1) % list.length;
        break;
      case 'ArrowLeft':
        next = (current - 1 + list.length) % list.length;
        break;
      case 'Home':
        next = 0;
        break;
      case 'End':
        next = list.length - 1;
        break;
      case 'Delete':
      case 'Backspace': {
        const tab = list[current];
        if (tab && !tab.pinned) {
          ev.preventDefault();
          this.tabs.closeTab(tab.id);
          this.focusTab(this.tabs.activeTabId());
        }
        return;
      }
      default:
        return;
    }
    ev.preventDefault();
    const target = list[next];
    this.selectTab(target.id);
    this.focusTab(target.id);
  }

  private focusTab(id: string): void {
    requestAnimationFrame(() => {
      this.host.nativeElement.querySelector<HTMLElement>(`[id="ks-tab-${CSS.escape(id)}"]`)?.focus();
    });
  }
}

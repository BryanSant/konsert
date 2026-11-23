import { ChangeDetectionStrategy, Component, computed, inject, output } from '@angular/core';
import { NotificationService } from '../core/services/notification.service';
import { FleetStore } from '../core/state/fleet.store';
import { TabsStore } from '../core/state/tabs.store';
import { IconComponent } from '../shared/icon.component';

/** Herdr-style tab strip in the top bar (scoped to the active Space when one is selected). */
@Component({
  selector: 'app-tab-strip',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent],
  host: { class: 'flex-1 min-w-0 h-full' },
  template: `
    <div class="flex-1 flex items-center gap-1 px-2 h-full overflow-x-auto min-w-0">
      @if (activeSpace(); as sp) {
        <span class="inline-flex items-center gap-1.5 rounded bg-[#1d2026] border border-[#2c2f38] px-2 py-0.5 text-[11px] text-[#b4b6be] shrink-0 mr-1">
          <span class="h-3.5 w-3.5 rounded flex items-center justify-center text-[9px] font-semibold bg-[#1f8a8c]/85 text-white">
            {{ sp.name === '~' ? '~' : sp.name.slice(0, 1).toUpperCase() }}
          </span>
          <span class="font-medium text-[#e4e4e7]">{{ sp.name }}</span>
        </span>
      }

      @for (tab of tabs.visibleTabs(); track tab.id) {
        <div
          (click)="selectTab(tab.id)"
          class="group flex items-center gap-1.5 px-2.5 py-1 text-[12px] rounded-md cursor-pointer shrink-0 transition-all duration-150"
          [class.bg-[#24262d]]="tabs.activeTabId() === tab.id || tabs.splitTabId() === tab.id"
          [class.text-[#f4f4f5]]="tabs.activeTabId() === tab.id"
          [class.text-[#b4b6be]]="tabs.splitTabId() === tab.id && tabs.activeTabId() !== tab.id"
          [class.text-[#8e9098]]="tabs.activeTabId() !== tab.id && tabs.splitTabId() !== tab.id"
          [class.hover:bg-[#1e2026]]="tabs.activeTabId() !== tab.id && tabs.splitTabId() !== tab.id"
          [class.hover:text-[#d4d4d8]]="tabs.activeTabId() !== tab.id && tabs.splitTabId() !== tab.id"
        >
          @if (tab.kind === 'orchestrator') {
            <app-icon name="orchestrator" [size]="12" class="text-[#60a5fa]" />
          } @else {
            <app-icon [name]="tab.agent || 'shell'" [size]="12" />
          }
          <span class="max-w-[150px] truncate">{{ tab.title }}</span>
          @if (notifications.isUnread(tab.id) && tabs.activeTabId() !== tab.id) {
            <span class="h-1.5 w-1.5 rounded-full bg-[#d9a043] shrink-0" title="Attention needed"></span>
          }
          @if (tabs.splitTabId() === tab.id) {
            <span class="rounded bg-[#1a1c22] border border-[#32353f] px-1 text-[9px] font-mono-ide text-[#8e9098]">split</span>
          }
          @if (!tab.pinned) {
            <button
              type="button"
              (click)="$event.stopPropagation(); tabs.closeTab(tab.id)"
              class="rounded hover:bg-white/10 p-0.5 text-[#686a72] hover:text-[#ececed] transition cursor-pointer"
            >
              <app-icon name="close" [size]="11" />
            </button>
          }
        </div>
      }

      <button
        type="button"
        (click)="newTab.emit()"
        [title]="activeSpace() ? 'New session in ' + activeSpace()!.name : 'New chat or terminal session'"
        class="rounded-md p-1 text-[#787a82] hover:text-[#ececed] hover:bg-[#23252b] active:scale-95 transition cursor-pointer shrink-0"
      >
        <app-icon name="plus" [size]="14" />
      </button>

      @if (tabs.activeTab()?.kind === 'terminal' || tabs.splitTabId()) {
        <button
          type="button"
          (click)="toggleSplit.emit()"
          [title]="tabs.splitTabId() ? 'Close split pane (⌘\\)' : 'Split terminal side-by-side (⌘\\)'"
          class="hidden md:inline-flex rounded-md p-1 active:scale-95 transition cursor-pointer shrink-0"
          [class.bg-[#24262d]]="!!tabs.splitTabId()"
          [class.text-[#ececed]]="!!tabs.splitTabId()"
          [class.text-[#787a82]]="!tabs.splitTabId()"
          [class.hover:text-[#ececed]]="!tabs.splitTabId()"
          [class.hover:bg-[#23252b]]="!tabs.splitTabId()"
        >
          <app-icon name="split" [size]="14" />
        </button>
      }
    </div>
  `,
})
export class TabStripComponent {
  readonly tabs = inject(TabsStore);
  readonly notifications = inject(NotificationService);
  private readonly fleet = inject(FleetStore);

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
}

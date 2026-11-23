import { ChangeDetectionStrategy, Component, computed, inject, input, signal } from '@angular/core';
import { AgentRelogin } from '../../core/models/agent-chat.models';
import { FleetStore } from '../../core/state/fleet.store';
import { IconComponent } from '../../shared/icon.component';
import { TooltipDirective } from '../../shared/tooltip.directive';
import { ReloginService } from './relogin.service';

/** "Run this on <host>" with copy, plus "Open terminal on <host>". */
@Component({
  selector: 'ks-relogin-help',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective],
  template: `
    <div class="space-y-2.5">
      @if (command()) {
        <div class="text-[11.5px] text-[var(--ks-text-secondary)]">Run this on {{ hostName() }}:</div>
        <div class="flex items-center gap-1 rounded-[var(--ks-radius-md)] bg-black/30 pl-3 pr-1 py-1">
          <code class="flex-1 min-w-0 truncate font-mono-ide text-[12px] text-[#e6e6e8]" data-testid="relogin-command">{{ command() }}</code>
          <button
            type="button"
            class="ks-icon-btn ks-icon-btn-sm"
            [attr.aria-label]="copied() ? 'Copied' : 'Copy command'"
            [ksTooltip]="copied() ? 'Copied' : 'Copy command'"
            (click)="copy()"
          >
            <app-icon [name]="copied() ? 'check' : 'copy'" [size]="13" />
          </button>
        </div>
      } @else {
        <div class="text-[11.5px] text-[var(--ks-text-secondary)]">Sign in to this agent's CLI on {{ hostName() }}.</div>
      }
      @if (relogin()?.note) {
        <div class="text-[11.5px] text-[var(--ks-text-muted)]">{{ relogin()!.note }}</div>
      }
      <div class="flex flex-wrap items-center gap-1.5">
        <button type="button" class="ks-btn ks-btn-sm ks-btn-soft" [disabled]="opening() || !workerId()" (click)="openTerminal()">
          <app-icon name="shell" [size]="12" />
          {{ opening() ? 'Opening…' : 'Open terminal on ' + hostName() }}
        </button>
        @if (relogin()?.url) {
          <a class="ks-btn ks-btn-sm" [href]="relogin()!.url" target="_blank" rel="noopener noreferrer">
            <app-icon name="globe" [size]="12" />
            Help
          </a>
        }
      </div>
      @if (error()) {
        <div role="alert" class="text-[11.5px] text-[#f3a5a5]">{{ error() }}</div>
      }
    </div>
  `,
})
export class ReloginHelpComponent {
  readonly relogin = input<AgentRelogin | undefined>(undefined);
  readonly workerId = input('');
  readonly accountId = input('');

  private readonly svc = inject(ReloginService);
  private readonly fleet = inject(FleetStore);
  readonly copied = signal(false);
  readonly opening = signal(false);
  readonly error = signal('');

  readonly command = computed(() => this.svc.commandLine(this.relogin()));
  readonly hostName = computed(() => this.fleet.workersById()[this.workerId()]?.name || 'the host');

  async copy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(this.command());
      this.copied.set(true);
      setTimeout(() => this.copied.set(false), 1500);
    } catch {
      this.error.set('Copy failed: select the command and copy it manually.');
    }
  }

  async openTerminal(): Promise<void> {
    this.opening.set(true);
    this.error.set('');
    try {
      await this.svc.openTerminal(this.workerId(), this.accountId(), this.relogin());
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not open a terminal');
    } finally {
      this.opening.set(false);
    }
  }
}

import { ChangeDetectionStrategy, Component, DestroyRef, computed, inject, output, signal } from '@angular/core';
import { ApiClientService } from '../../core/services/api-client.service';
import { FleetStore } from '../../core/state/fleet.store';
import { SetupStore } from '../../core/state/setup.store';
import { localHostSummary } from '../../layout/add-machine-modal.component';
import { IconComponent } from '../../shared/icon.component';
import { ModeUiStore } from '../mode-ui.store';

export const FIRST_RUN_SUGGESTIONS = [
  "Summarize what's running on my machines",
  'Fix the failing tests in this repo',
  'Review my open PRs',
  'Add my desktop over SSH',
];

/**
 * Command on a fresh hub: what Syre is, and "Add this machine" (the hub starts this
 * machine's syred, as the Add Machine dialog does). The commander needs a machine
 * to run on, so the composer stays disabled until one is online.
 */
@Component({
  selector: 'sy-first-run',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent],
  host: { class: 'flex flex-col min-h-0 h-full overflow-y-auto' },
  template: `
    <div class="flex-1 w-full max-w-[760px] mx-auto px-6 pt-10 pb-4 flex flex-col gap-5" data-testid="first-run">
      <h1 class="m-0 text-[26px] font-semibold tracking-tight text-[var(--sy-text)]">Welcome to Syre</h1>
      <p class="m-0 text-[15px] leading-relaxed text-[var(--sy-text-secondary)]">
        Syre runs coding agents on your own machines with the subscriptions you already have. You chat with the commander here; it sends
        work to agents across your machines, and you can watch, answer their questions, or take over at any time.
      </p>

      @if (online()) {
        <div class="flex items-center gap-2 whitespace-nowrap rounded-[var(--sy-radius-lg)] bg-[var(--sy-green-soft)] px-4 py-3.5 text-[13.5px] text-[#cdebd3]" role="status" data-testid="first-run-online">
          <app-icon name="check" [size]="13" />
          <span class="min-w-0 truncate">{{ online()!.name }} is online. Ask the commander for anything below.</span>
        </div>
      } @else {
        <section class="rounded-[var(--sy-radius-lg)] bg-[var(--sy-surface-1)] px-5 py-4 flex flex-col gap-3" aria-labelledby="sy-first-run-step">
          <h2 id="sy-first-run-step" class="m-0 text-[13px] font-normal text-[var(--sy-text-secondary)]">Step 1 · The commander needs a machine to run on</h2>
          <div class="flex flex-wrap items-center gap-3">
            <div class="flex-1 min-w-[220px]">
              <div class="font-semibold text-[var(--sy-text)]">This machine{{ local()?.hostname ? ' (' + local()!.hostname + ')' : '' }}</div>
              <div class="mt-0.5 text-[12.5px] text-[var(--sy-text-secondary)]" data-testid="first-run-local">{{ localLine() }}</div>
            </div>
            <button
              type="button"
              class="sy-btn sy-btn-primary h-10 px-4"
              data-testid="first-run-add-local"
              [disabled]="busy() || starting() || !canAdd()"
              (click)="addLocal()"
            >
              @if (busy() || starting()) {
                <span class="sy-spinner" style="width: 12px; height: 12px"></span>
                Starting syred…
              } @else {
                Add this machine
              }
            </button>
          </div>
          @if (error()) {
            <p role="alert" class="m-0 text-[12.5px] text-[#f3a5a5]">{{ error() }}</p>
          }
          <p class="m-0 text-[12.5px] text-[var(--sy-text-muted)]">
            or <button type="button" class="underline text-[var(--sy-blue)] hover:text-[#cfe2ff]" data-testid="first-run-ssh" (click)="ui.addMachineOpen.set(true)">add another machine over SSH</button>
          </p>
        </section>
      }

      <div class="flex flex-wrap gap-2">
        @for (s of suggestions; track s) {
          <button type="button" class="sy-btn sy-btn-soft" [disabled]="!online()" (click)="start.emit(s)">{{ s }}</button>
        }
      </div>
    </div>

    <form class="w-full max-w-[760px] mx-auto px-6 pb-5 shrink-0" (submit)="$event.preventDefault(); submit()">
      <div class="rounded-[var(--sy-radius-xl)] bg-[var(--sy-surface-2)] px-4 py-3 shadow-[var(--sy-shadow-composer)]" [class.opacity-60]="!online()">
        <label class="sy-sr-only" for="sy-first-run-composer">Message the commander</label>
        <textarea
          id="sy-first-run-composer"
          rows="2"
          class="w-full resize-none bg-transparent text-[14px] text-[var(--sy-text)] placeholder:text-[var(--sy-text-muted)] outline-none"
          data-testid="first-run-composer"
          [disabled]="!online()"
          [placeholder]="online() ? 'Ask anything, queue new work, @ a machine or file…' : 'Add a machine to start chatting with the commander'"
          [value]="draft()"
          (input)="draft.set($any($event.target).value)"
          (keydown.enter)="onEnter($any($event))"
        ></textarea>
      </div>
    </form>
  `,
})
export class FirstRunComponent {
  /** The user is ready to chat (a suggestion or their own first message). */
  readonly start = output<string>();

  readonly ui = inject(ModeUiStore);
  private readonly setup = inject(SetupStore);
  private readonly fleet = inject(FleetStore);
  private readonly api = inject(ApiClientService);
  readonly suggestions = FIRST_RUN_SUGGESTIONS;

  readonly busy = signal(false);
  readonly error = signal('');
  readonly draft = signal('');
  /** The machine being added, followed until it is online. */
  private readonly addingId = signal('');
  private timer: ReturnType<typeof setTimeout> | null = null;

  readonly local = this.setup.local;
  readonly online = computed(() => this.fleet.workers().find((w) => w.status === 'online') ?? null);
  readonly starting = computed(() => {
    const id = this.addingId();
    if (!id) return false;
    const w = this.fleet.workersById()[id];
    return w?.status !== 'online' && w?.launch_state !== 'failed';
  });
  readonly canAdd = computed(() => {
    const lh = this.local();
    return !lh || lh.connected || lh.syred_found || !!lh.worker_id;
  });
  readonly localLine = computed(() => {
    const lh = this.local();
    if (!lh) return this.setup.loading() ? 'Checking this machine…' : this.setup.error() || 'syred not found yet';
    const w = this.addingId() ? this.fleet.workersById()[this.addingId()] : undefined;
    if (w?.launch_state === 'failed') return `Could not start syred: ${w.launch_error || 'unknown error'}`;
    return localHostSummary(lh);
  });

  constructor() {
    if (!this.setup.status()) void this.setup.load();
    inject(DestroyRef).onDestroy(() => this.timer && clearTimeout(this.timer));
  }

  async addLocal(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set('');
    try {
      const res = await this.api.addLocalWorker();
      this.setup.markCompleted();
      this.addingId.set(res.worker_id);
      await this.fleet.refresh();
      this.follow();
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Could not add this machine');
    } finally {
      this.busy.set(false);
    }
  }

  /** The socket usually reports the machine first; poll as a fallback. */
  private follow(): void {
    if (!this.starting()) {
      void this.setup.load();
      return;
    }
    this.timer = setTimeout(() => void this.fleet.refresh().then(() => this.follow()), 2000);
  }

  onEnter(ev: KeyboardEvent): void {
    if (ev.shiftKey || ev.isComposing) return;
    ev.preventDefault();
    this.submit();
  }

  submit(): void {
    const t = this.draft().trim();
    if (!t || !this.online()) return;
    this.start.emit(t);
  }
}

import {
  ChangeDetectionStrategy,
  Component,
  OnInit,
  computed,
  effect,
  inject,
  input,
  output,
  signal,
  untracked,
} from '@angular/core';
import { ApiClientService } from '../../core/services/api-client.service';
import { FleetStore } from '../../core/state/fleet.store';
import {
  WorkspaceFolders,
  coverageText,
  displayFolder,
  normalizeFolder,
  osSpecificWarning,
} from './workspace-folder';

let nextId = 0;

/**
 * A home-relative folder for every machine (SYR-W3SVZ): a combobox, not a file browser.
 * Suggestions come from the machines' home surveys with how many have each; a typo shows
 * "on 0 of 5 machines". Paths that leave home are refused; a folder tied to one OS asks
 * for confirmation. Emits the folder ("src") once it is committed.
 */
@Component({
  selector: 'sy-workspace-folder-input',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'flex flex-col gap-1 min-w-0' },
  template: `
    <input
      type="text"
      class="sy-field sy-field-sm w-full font-mono-ide"
      [attr.list]="listId"
      [attr.aria-label]="label()"
      [attr.aria-invalid]="!!problem() || null"
      data-testid="workspace-folder-input"
      placeholder="~/src"
      spellcheck="false"
      autocomplete="off"
      [value]="draft()"
      (input)="draft.set($any($event.target).value); confirmed.set(false)"
      (keydown.enter)="$event.preventDefault(); commit()"
      (blur)="commit()"
    />
    <datalist [id]="listId">
      @for (s of folders()?.suggestions ?? []; track s.path) {
        <option [value]="show(s.path)">
          on {{ s.count }} of {{ s.total }} machines{{ s.repos ? ' · ' + s.repos + ' repos' : '' }}
        </option>
      }
    </datalist>
    @if (problem()) {
      <p class="m-0 text-[11.5px] text-[#f09595]" role="alert" data-testid="workspace-folder-error">
        {{ problem() }}
      </p>
    } @else {
      @if (coverage()) {
        <p
          class="m-0 text-[11.5px] text-[var(--sy-text-muted)]"
          data-testid="workspace-folder-coverage"
        >
          {{ coverage() }}
        </p>
      }
      @if (warning(); as w) {
        <div
          class="flex flex-wrap items-center gap-2 text-[11.5px] text-[#e6b763]"
          data-testid="workspace-folder-warning"
        >
          <span>{{ w }}</span>
          @if (!confirmed()) {
            <button
              type="button"
              class="sy-btn sy-btn-sm"
              data-testid="workspace-folder-confirm"
              (click)="confirmed.set(true); commit()"
            >
              Use it anyway
            </button>
          }
        </div>
      }
    }
  `,
})
export class WorkspaceFolderInputComponent implements OnInit {
  private readonly api = inject(ApiClientService);
  private readonly fleet = inject(FleetStore);

  /** The committed folder ("src"; '' shows the fleet's effective default). */
  readonly value = input('');
  readonly label = input('Workspace folder');
  readonly valueChange = output<string>();

  readonly listId = `sy-wsf-${nextId++}`;
  readonly folders = signal<WorkspaceFolders | null>(null);
  readonly draft = signal('');
  readonly confirmed = signal(false);

  private readonly parsed = computed(() => normalizeFolder(this.draft()));
  readonly problem = computed(() => this.parsed().error ?? '');
  readonly warning = computed(() => osSpecificWarning(this.parsed().path));
  readonly coverage = computed(() => {
    const names = Object.fromEntries(this.fleet.workers().map((w) => [w.id, w.name]));
    return coverageText(this.parsed().path, this.folders(), names);
  });

  constructor() {
    // Show the committed value; '' shows what the fleet uses.
    effect(() => {
      const v = this.value() || this.folders()?.effective || '';
      untracked(() => {
        this.draft.set(v ? this.show(v) : '');
        this.confirmed.set(!!v);
      });
    });
  }

  async ngOnInit(): Promise<void> {
    try {
      this.folders.set(await this.api.getWorkspaceFolders());
    } catch {
      // No suggestions; the field still takes a folder.
    }
  }

  show(path: string): string {
    return displayFolder(path);
  }

  commit(): void {
    const p = this.parsed();
    if (p.error || !p.path) return;
    if (this.warning() && !this.confirmed()) return;
    if (p.path !== (this.value() || this.folders()?.effective || '')) this.valueChange.emit(p.path);
  }
}

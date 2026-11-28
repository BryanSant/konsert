import { ChangeDetectionStrategy, Component, OnInit, computed, inject } from '@angular/core';
import { PushPrefs } from '../../core/models/syre.models';
import { PushService } from '../../core/services/push.service';

type Category = NonNullable<PushPrefs['off']>[number];

const CATEGORIES: { id: Category; label: string }[] = [
  { id: 'waiting', label: 'A session waits for you' },
  { id: 'question', label: 'Questions (P1 at once, others after a while)' },
  { id: 'failed', label: 'A session failed' },
  { id: 'completed', label: 'A session finished' },
];

/** Settings → Devices: Web Push on this device (SYR-Z15J6). */
@Component({
  selector: 'sy-push-settings',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section
      class="mb-4 rounded-[var(--sy-radius-md)] border border-[var(--sy-hairline)] px-3 py-2.5 space-y-2"
      aria-labelledby="push-heading"
      data-testid="push-settings"
    >
      <h3 id="push-heading" class="m-0 text-[12.5px] font-semibold text-[var(--sy-text)]">
        Notifications on this device
      </h3>
      @if (!push.supported()) {
        <p class="m-0 text-[var(--sy-text-muted)]" data-testid="push-unsupported">
          This browser can't receive notifications from Syre here. Open Syre as an installed app or
          in Chrome, Edge or Safari.
        </p>
      } @else if (push.needsInstall()) {
        <p class="m-0 text-[var(--sy-text-muted)]" data-testid="push-install-first">
          On iPhone and iPad, add Syre to the Home Screen first (Share, then Add to Home Screen),
          open it from there, and turn notifications on.
        </p>
      } @else if (!push.subscribed()) {
        <p class="m-0 text-[var(--sy-text-muted)]">
          Get notified when a session waits for you, fails or finishes, and of questions that need
          you, even with no Syre window open.
        </p>
        <button
          type="button"
          class="sy-btn sy-btn-primary sy-btn-sm"
          data-testid="push-enable"
          [disabled]="push.busy()"
          (click)="push.enable()"
        >
          Turn on notifications
        </button>
      } @else {
        <fieldset class="m-0 p-0 border-0 space-y-1">
          <legend class="text-[var(--sy-text-muted)] mb-1">Notify me when</legend>
          @for (c of categories; track c.id) {
            <label class="flex items-center gap-2">
              <input
                type="checkbox"
                [checked]="isOn(c.id)"
                (change)="toggle(c.id, $any($event.target).checked)"
                [attr.data-testid]="'push-cat-' + c.id"
              />
              {{ c.label }}
            </label>
          }
        </fieldset>
        <div class="flex flex-wrap items-center gap-2">
          <label class="flex items-center gap-2">
            <input
              type="checkbox"
              data-testid="push-quiet"
              [checked]="!!push.prefs().quiet"
              (change)="setQuiet($any($event.target).checked)"
            />
            Quiet hours
          </label>
          @if (push.prefs().quiet; as q) {
            <input
              type="time"
              class="sy-field sy-field-sm"
              aria-label="Quiet from"
              data-testid="push-quiet-start"
              [value]="q.start"
              (change)="setQuietTime('start', $any($event.target).value)"
            />
            <span>to</span>
            <input
              type="time"
              class="sy-field sy-field-sm"
              aria-label="Quiet until"
              data-testid="push-quiet-end"
              [value]="q.end"
              (change)="setQuietTime('end', $any($event.target).value)"
            />
            <span class="text-[var(--sy-text-muted)]">{{ push.prefs().time_zone }}</span>
          }
        </div>
        <div class="flex flex-wrap gap-2">
          <button
            type="button"
            class="sy-btn sy-btn-sm"
            data-testid="push-test"
            (click)="push.test()"
          >
            Send test notification
          </button>
          <button
            type="button"
            class="sy-btn sy-btn-sm"
            data-testid="push-disable"
            [disabled]="push.busy()"
            (click)="push.disable()"
          >
            Turn off
          </button>
        </div>
        @if (push.testResult()) {
          <p class="m-0 text-[var(--sy-text-muted)]" role="status" data-testid="push-test-result">
            {{ push.testResult() }}
          </p>
        }
      }
      @if (push.error()) {
        <p class="m-0 text-[#f09595]" role="alert" data-testid="push-error">{{ push.error() }}</p>
      }
    </section>
  `,
})
export class PushSettingsComponent implements OnInit {
  readonly push = inject(PushService);
  readonly categories = CATEGORIES;
  private readonly off = computed(() => new Set(this.push.prefs().off ?? []));

  ngOnInit(): void {
    void this.push.load();
  }

  isOn(c: Category): boolean {
    return !this.off().has(c);
  }

  toggle(c: Category, on: boolean): void {
    const off = new Set(this.push.prefs().off ?? []);
    if (on) off.delete(c);
    else off.add(c);
    void this.push.savePrefs({ ...this.push.prefs(), off: [...off] });
  }

  setQuiet(on: boolean): void {
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
    const { quiet: _q, ...rest } = this.push.prefs();
    void this.push.savePrefs(
      on ? { ...rest, quiet: { start: '22:00', end: '07:00' }, time_zone: tz } : rest,
    );
  }

  setQuietTime(which: 'start' | 'end', value: string): void {
    const p = this.push.prefs();
    if (!p.quiet || !value) return;
    void this.push.savePrefs({ ...p, quiet: { ...p.quiet, [which]: value } });
  }
}

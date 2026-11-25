import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { IconComponent } from './icon.component';
import { TooltipDirective } from './tooltip.directive';
import { SpinnerDetail, SpinnerMachine, SpinnerState, describeMachine, describeSpinner, heatFill, machineInitials, orbitDots, rippleSeconds } from './spinner-logic';

/**
 * The live-work spinner (signed-off design, style A). The avatar fill is the machine and
 * how hot it runs (a slowly drifting gradient, cool blue/teal → amber → red, hot drifts
 * faster); the ring around it is what the agent is doing:
 *
 *   thinking  violet comet, breathing      working  white segments spinning
 *   speaking  mint ripple (speed by tok/s)  asking   solid amber + "?"
 *   held      dashed blue + pause           you      solid blue + "›"
 *   queued    hollow avatar, dotted ring    done     grey ring + green check, neutral fill
 *   error     red + "!"                     coarse   (observed sessions) half strength
 *   waiting   plain grey: the parent only waits on its sub-agents
 *
 * With `detail` it also shows what runs beside the main loop: a shell badge "❯N" at the
 * bottom-left (its outline pulses while any shell is busy) and up to four sub-agent dots
 * orbiting the ring in their own state colours (violet thinking, white working, green
 * speaking, amber asking), "+N" top-right beyond four. Counts show only when non-zero.
 *
 * A second machine peeks out behind the first. With `glyph` the centre shows the agent's
 * mark instead of machine initials (Fleet rows, where the row is the machine). The host has
 * an accessible name and a hover title (machine, heat %, cause, the activity word); callers
 * always put a text label next to it too. prefers-reduced-motion stops every animation.
 */
@Component({
  selector: 'sy-spinner',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective],
  host: {
    role: 'img',
    class: 'sy-activity-spinner',
    '[attr.aria-label]': 'description()',
    '[attr.data-spinner]': 'state()',
    '[class.sy-spinner-two]': 'second() !== null',
    '[class.sy-spinner-coarse]': 'coarse()',
    '[style.--sy-sz]': "size() + 'px'",
    '[style.--sy-ripple]': 'ripple()',
  },
  template: `
    <span class="sy-sp-wrap" [syTooltip]="description()">
    @if (second(); as m2) {
      <span class="sy-sp-second" aria-hidden="true" [style.background]="m2.fill.background" [style.animation-duration]="m2.fill.drift">{{ m2.initials }}</span>
    }
    @if (state() === 'speaking') {
      <span class="sy-sp-ripple" aria-hidden="true"></span>
    }
    <span class="sy-sp-ring" [attr.data-ring]="state()" aria-hidden="true"></span>
    <span
      class="sy-sp-avatar"
      aria-hidden="true"
      [class.sy-sp-hollow]="state() === 'queued'"
      [class.sy-sp-neutral]="state() === 'done' || glyph()"
      [class.sy-sp-drift]="state() !== 'queued' && state() !== 'done' && !glyph()"
      [style.background]="avatarBg()"
      [style.animation-duration]="first()?.fill?.drift ?? null"
    >
      @if (glyph()) {
        <app-icon [name]="agent() || 'shell'" [size]="glyphSize()" />
      } @else {
        {{ first()?.initials ?? '··' }}
      }
    </span>
    @if (orbit(); as o) {
      @if (o.dots.length) {
        <span class="sy-sp-orbit" aria-hidden="true" data-testid="spinner-orbit">
          @for (d of o.dots; track $index) {
            <span class="sy-sp-dot" [attr.data-dot]="d.state" [style.transform]="'rotate(' + d.angle + 'deg) translateY(calc(var(--sy-sz) * -0.633))'"></span>
          }
        </span>
      }
      @if (o.more) {
        <span class="sy-sp-more" aria-hidden="true" data-testid="spinner-more">+{{ o.more }}</span>
      }
    }
    @if (detail()?.shells; as n) {
      <span class="sy-sp-shells" [class.sy-sp-shells-busy]="(detail()?.shellsBusy ?? 0) > 0" aria-hidden="true" data-testid="spinner-shells">❯{{ n }}</span>
    }
    @if (badge(); as b) {
      <span class="sy-sp-badge" [attr.data-badge]="state()" aria-hidden="true">{{ b }}</span>
    }
    </span>
  `,
  styles: `
    :host {
      --sy-sz: 30px;
      position: relative;
      display: inline-block;
      flex: none;
      width: var(--sy-sz);
      height: var(--sy-sz);
      vertical-align: middle;
    }
    .sy-sp-wrap {
      position: absolute;
      inset: 0;
    }
    :host(.sy-spinner-two) {
      margin-left: calc(var(--sy-sz) * 0.33);
    }
    .sy-sp-ring,
    .sy-sp-ripple {
      position: absolute;
      inset: 0;
      border-radius: 50%;
    }
    .sy-sp-ring {
      /* A 2px ring: the fill is masked to its outer band, so it sits on any surface. */
      -webkit-mask: radial-gradient(farthest-side, transparent calc(100% - 2.5px), #000 calc(100% - 2px));
      mask: radial-gradient(farthest-side, transparent calc(100% - 2.5px), #000 calc(100% - 2px));
    }
    :host(.sy-spinner-coarse) .sy-sp-ring,
    :host(.sy-spinner-coarse) .sy-sp-ripple {
      opacity: 0.5;
    }
    .sy-sp-ring[data-ring='thinking'] {
      background: conic-gradient(from 0deg, transparent 0 50%, #b9a8ff 100%);
      animation:
        sy-spin 2.6s linear infinite,
        sy-breath 2.6s ease-in-out infinite;
    }
    .sy-sp-ring[data-ring='working'] {
      background: repeating-conic-gradient(#e6e8ee 0 46deg, transparent 46deg 120deg);
      animation: sy-spin 1s linear infinite;
    }
    .sy-sp-ring[data-ring='speaking'] {
      background: #8fe0b0;
    }
    .sy-sp-ripple {
      animation: sy-ripple var(--sy-ripple, 0.9s) ease-out infinite;
    }
    .sy-sp-ring[data-ring='asking'] {
      background: #e0a84c;
    }
    .sy-sp-ring[data-ring='held'] {
      background: repeating-conic-gradient(#6aaefa 0 22deg, transparent 22deg 45deg);
    }
    .sy-sp-ring[data-ring='you'] {
      background: #2f6fd6;
    }
    .sy-sp-ring[data-ring='queued'] {
      background: repeating-conic-gradient(rgba(255, 255, 255, 0.3) 0 10deg, transparent 10deg 30deg);
    }
    .sy-sp-ring[data-ring='done'] {
      background: rgba(255, 255, 255, 0.16);
    }
    .sy-sp-ring[data-ring='waiting'] {
      background: rgba(255, 255, 255, 0.22);
    }
    .sy-sp-orbit {
      position: absolute;
      inset: calc(var(--sy-sz) * -0.133);
      border-radius: 50%;
      animation: sy-spin 9s linear infinite;
      pointer-events: none;
    }
    .sy-sp-dot {
      position: absolute;
      left: 50%;
      top: 50%;
      width: max(4px, calc(var(--sy-sz) * 0.2));
      height: max(4px, calc(var(--sy-sz) * 0.2));
      margin: calc(max(4px, calc(var(--sy-sz) * 0.2)) / -2) 0 0 calc(max(4px, calc(var(--sy-sz) * 0.2)) / -2);
      border-radius: 50%;
      box-shadow: 0 0 0 1.2px var(--sy-spinner-hole, var(--sy-bg-chrome));
    }
    .sy-sp-dot[data-dot='thinking'] {
      background: #b9a8ff;
      animation: sy-breath 2s ease-in-out infinite;
    }
    .sy-sp-dot[data-dot='working'] {
      background: #e6e8ee;
      animation: sy-blink 0.8s ease-in-out infinite;
    }
    .sy-sp-dot[data-dot='speaking'] {
      background: #8fe0b0;
    }
    .sy-sp-dot[data-dot='asking'] {
      background: #e0a84c;
    }
    .sy-sp-more {
      position: absolute;
      right: calc(var(--sy-sz) * -0.3);
      top: calc(var(--sy-sz) * -0.23);
      padding: 1px 3px;
      border-radius: 6px;
      font: 700 max(6.5px, calc(var(--sy-sz) * 0.23)) / 1.2 'JetBrains Mono', ui-monospace, monospace;
      color: #c9cbd1;
      background: #2a2c31;
      box-shadow: 0 0 0 1.5px var(--sy-spinner-hole, var(--sy-bg-chrome));
    }
    .sy-sp-shells {
      position: absolute;
      left: calc(var(--sy-sz) * -0.133);
      bottom: calc(var(--sy-sz) * -0.133);
      min-width: max(11px, calc(var(--sy-sz) * 0.433));
      height: max(11px, calc(var(--sy-sz) * 0.433));
      padding: 0 2px;
      box-sizing: border-box;
      border-radius: 7px;
      display: flex;
      align-items: center;
      justify-content: center;
      font: 700 max(6.5px, calc(var(--sy-sz) * 0.25)) / 1 'JetBrains Mono', ui-monospace, monospace;
      color: #ececed;
      background: #2a2c31;
      box-shadow: 0 0 0 2px var(--sy-spinner-hole, var(--sy-bg-chrome));
    }
    .sy-sp-shells-busy {
      animation: sy-pulse 1.2s ease-in-out infinite;
    }
    .sy-sp-ring[data-ring='error'] {
      background: #e86b6b;
    }
    .sy-sp-avatar {
      position: absolute;
      inset: calc(var(--sy-sz) * 0.135);
      border-radius: 50%;
      display: flex;
      align-items: center;
      justify-content: center;
      font: 600 calc(var(--sy-sz) * 0.3) / 1 'JetBrains Mono', ui-monospace, monospace;
      color: #fff;
      text-shadow: 0 1px 2px rgba(0, 0, 0, 0.5);
      text-transform: lowercase;
      overflow: hidden;
    }
    .sy-sp-drift,
    .sy-sp-second {
      background-size: 300% 300% !important;
      animation-name: sy-drift;
      animation-timing-function: ease-in-out;
      animation-iteration-count: infinite;
      animation-direction: alternate;
    }
    .sy-sp-hollow {
      background: transparent !important;
      box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.22);
      color: #8f9199;
      text-shadow: none;
    }
    .sy-sp-neutral {
      background: #2a2c31 !important;
      color: #8f9199;
      text-shadow: none;
    }
    .sy-sp-second {
      position: absolute;
      left: calc(var(--sy-sz) * -0.33);
      top: calc(var(--sy-sz) * 0.33);
      width: calc(var(--sy-sz) * 0.6);
      height: calc(var(--sy-sz) * 0.6);
      border-radius: 50%;
      box-shadow: 0 0 0 2px var(--sy-spinner-hole, var(--sy-bg-chrome));
      display: flex;
      align-items: center;
      justify-content: center;
      font: 600 calc(var(--sy-sz) * 0.23) / 1 'JetBrains Mono', ui-monospace, monospace;
      color: #fff;
    }
    .sy-sp-badge {
      position: absolute;
      right: calc(var(--sy-sz) * -0.1);
      bottom: calc(var(--sy-sz) * -0.1);
      width: max(11px, calc(var(--sy-sz) * 0.43));
      height: max(11px, calc(var(--sy-sz) * 0.43));
      border-radius: 50%;
      font: 700 max(7px, calc(var(--sy-sz) * 0.27)) / 1 Inter, system-ui, sans-serif;
      display: flex;
      align-items: center;
      justify-content: center;
      box-shadow: 0 0 0 2px var(--sy-spinner-hole, var(--sy-bg-chrome));
    }
    .sy-sp-badge[data-badge='asking'] {
      background: #e0a84c;
      color: #1b1406;
    }
    .sy-sp-badge[data-badge='held'] {
      background: #6aaefa;
      color: #0d1a2a;
      font-size: max(6px, calc(var(--sy-sz) * 0.2));
    }
    .sy-sp-badge[data-badge='done'] {
      background: #69b879;
      color: #0b1a0f;
    }
    .sy-sp-badge[data-badge='error'] {
      background: #e86b6b;
      color: #1e0b0b;
    }
    .sy-sp-badge[data-badge='you'] {
      background: #2f6fd6;
      color: #fff;
    }
    @keyframes sy-spin {
      to {
        transform: rotate(360deg);
      }
    }
    @keyframes sy-drift {
      0% {
        background-position: 0% 50%;
      }
      100% {
        background-position: 100% 50%;
      }
    }
    @keyframes sy-breath {
      0%,
      100% {
        opacity: 0.4;
      }
      50% {
        opacity: 1;
      }
    }
    @keyframes sy-blink {
      0%,
      100% {
        opacity: 1;
      }
      50% {
        opacity: 0.35;
      }
    }
    @keyframes sy-pulse {
      0%,
      100% {
        box-shadow:
          0 0 0 2px var(--sy-spinner-hole, var(--sy-bg-chrome)),
          0 0 0 2px rgba(230, 232, 238, 0);
      }
      50% {
        box-shadow:
          0 0 0 2px var(--sy-spinner-hole, var(--sy-bg-chrome)),
          0 0 0 3.5px rgba(230, 232, 238, 0.75);
      }
    }
    @keyframes sy-ripple {
      0% {
        box-shadow: 0 0 0 0 rgba(143, 224, 176, 0.6);
      }
      100% {
        box-shadow: 0 0 0 calc(var(--sy-sz) * 0.2) rgba(143, 224, 176, 0);
      }
    }
    @media (prefers-reduced-motion: reduce) {
      .sy-sp-ring,
      .sy-sp-ripple,
      .sy-sp-drift,
      .sy-sp-second,
      .sy-sp-orbit,
      .sy-sp-dot,
      .sy-sp-shells {
        animation: none !important;
      }
      .sy-sp-ripple {
        box-shadow: 0 0 0 2px rgba(143, 224, 176, 0.35);
      }
    }
  `,
})
export class ActivitySpinnerComponent {
  readonly state = input<SpinnerState>('done');
  /** The machine(s) it runs on: the first fills the avatar, a second peeks behind. */
  readonly machines = input<readonly SpinnerMachine[]>([]);
  /** The agent (for the glyph and the accessible name). */
  readonly agent = input('');
  /** Show the agent's mark in the centre instead of machine initials. */
  readonly glyph = input(false);
  readonly coarse = input(false);
  readonly tps = input<number | undefined>(undefined);
  readonly size = input(30);
  /** Overrides the generated accessible name / title. */
  readonly label = input('');
  /** Shells and sub-agents beside the main loop (badge and orbiting dots). */
  readonly detail = input<SpinnerDetail | null>(null);
  readonly orbit = computed(() => {
    const d = this.detail();
    return d && d.subs.length ? orbitDots(d.subs) : null;
  });

  private readonly view = computed(() =>
    this.machines()
      .slice(0, 2)
      .map((m) => ({ initials: machineInitials(m.name), fill: heatFill(m.heat) })),
  );
  readonly first = computed(() => this.view()[0] ?? null);
  readonly second = computed(() => (this.glyph() ? null : (this.view()[1] ?? null)));
  readonly avatarBg = computed(() => (this.glyph() ? null : (this.first()?.fill.background ?? null)));
  readonly glyphSize = computed(() => Math.max(9, Math.round(this.size() * 0.42)));
  readonly ripple = computed(() => `${rippleSeconds(this.tps())}s`);

  readonly badge = computed(() => {
    switch (this.state()) {
      case 'asking':
        return '?';
      case 'held':
        return '❚❚';
      case 'done':
        return '✓';
      case 'error':
        return '!';
      case 'you':
        return '›';
      default:
        return '';
    }
  });

  readonly description = computed(
    () =>
      this.label() ||
      describeSpinner(this.state(), this.machines(), { coarse: this.coarse(), tps: this.tps(), agent: this.glyph() ? this.agent() : '', detail: this.detail() }),
  );
}

/**
 * The machine's heat avatar on its own (Fleet rows, the footer machine list): the same
 * drifting fill without an activity ring. Rounded square at Fleet size, a dot in the footer.
 */
@Component({
  selector: 'sy-heat-avatar',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: {
    role: 'img',
    '[attr.aria-label]': 'title()',
    '[style.width.px]': 'size()',
    '[style.height.px]': 'size()',
    '[style.border-radius]': "shape() === 'square' ? '12px' : '50%'",
    '[style.background]': 'fill().background',
    '[style.animation-duration]': 'fill().drift',
    '[style.font-size.px]': 'size() * 0.3',
    '[class.sy-ha-drift]': 'heat() != null',
  },
  imports: [TooltipDirective],
  template: `<span class="sy-ha-wrap" [syTooltip]="title()">@if (showInitials()) {<span aria-hidden="true">{{ initials() }}</span>}</span>`,
  styles: `
    :host {
      display: inline-flex;
      flex: none;
      align-items: center;
      justify-content: center;
      font-family: 'JetBrains Mono', ui-monospace, monospace;
      font-weight: 600;
      color: #fff;
      text-shadow: 0 1px 2px rgba(0, 0, 0, 0.5);
      vertical-align: middle;
    }
    .sy-ha-wrap {
      display: flex;
      align-items: center;
      justify-content: center;
      width: 100%;
      height: 100%;
    }
    :host(.sy-ha-drift) {
      background-size: 300% 300% !important;
      animation-name: sy-ha-drift;
      animation-timing-function: ease-in-out;
      animation-iteration-count: infinite;
      animation-direction: alternate;
    }
    @keyframes sy-ha-drift {
      0% {
        background-position: 0% 50%;
      }
      100% {
        background-position: 100% 50%;
      }
    }
    @media (prefers-reduced-motion: reduce) {
      :host(.sy-ha-drift) {
        animation: none !important;
      }
    }
  `,
})
export class HeatAvatarComponent {
  readonly machine = input.required<SpinnerMachine>();
  readonly size = input(44);
  readonly shape = input<'square' | 'round'>('round');
  readonly label = input('');
  readonly heat = computed(() => this.machine().heat);
  readonly fill = computed(() => heatFill(this.machine().heat));
  readonly initials = computed(() => machineInitials(this.machine().name));
  readonly showInitials = computed(() => this.size() >= 20);
  readonly title = computed(() => this.label() || describeMachine(this.machine()));
}

import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { IconComponent } from './icon.component';
import { TooltipDirective } from './tooltip.directive';
import { SpinnerMachine, SpinnerState, describeMachine, describeSpinner, heatFill, machineInitials, rippleSeconds } from './spinner-logic';

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
 *
 * A second machine peeks out behind the first. With `glyph` the centre shows the agent's
 * mark instead of machine initials (Fleet rows, where the row is the machine). The host has
 * an accessible name and a hover title (machine, heat %, cause, the activity word); callers
 * always put a text label next to it too. prefers-reduced-motion stops every animation.
 */
@Component({
  selector: 'ks-spinner',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective],
  host: {
    role: 'img',
    class: 'ks-spinner',
    '[attr.aria-label]': 'description()',
    '[attr.data-spinner]': 'state()',
    '[class.ks-spinner-two]': 'second() !== null',
    '[class.ks-spinner-coarse]': 'coarse()',
    '[style.--ks-sz.px]': 'size()',
    '[style.--ks-ripple]': 'ripple()',
  },
  template: `
    <span class="ks-sp-wrap" [ksTooltip]="description()">
    @if (second(); as m2) {
      <span class="ks-sp-second" aria-hidden="true" [style.background]="m2.fill.background" [style.animation-duration]="m2.fill.drift">{{ m2.initials }}</span>
    }
    @if (state() === 'speaking') {
      <span class="ks-sp-ripple" aria-hidden="true"></span>
    }
    <span class="ks-sp-ring" [attr.data-ring]="state()" aria-hidden="true"></span>
    <span
      class="ks-sp-avatar"
      aria-hidden="true"
      [class.ks-sp-hollow]="state() === 'queued'"
      [class.ks-sp-neutral]="state() === 'done' || glyph()"
      [class.ks-sp-drift]="state() !== 'queued' && state() !== 'done' && !glyph()"
      [style.background]="avatarBg()"
      [style.animation-duration]="first()?.fill?.drift ?? null"
    >
      @if (glyph()) {
        <app-icon [name]="agent() || 'shell'" [size]="glyphSize()" />
      } @else {
        {{ first()?.initials ?? '··' }}
      }
    </span>
    @if (badge(); as b) {
      <span class="ks-sp-badge" [attr.data-badge]="state()" aria-hidden="true">{{ b }}</span>
    }
    </span>
  `,
  styles: `
    :host {
      --ks-sz: 30px;
      position: relative;
      display: inline-block;
      flex: none;
      width: var(--ks-sz);
      height: var(--ks-sz);
      vertical-align: middle;
    }
    .ks-sp-wrap {
      position: absolute;
      inset: 0;
    }
    :host(.ks-spinner-two) {
      margin-left: calc(var(--ks-sz) * 0.33);
    }
    .ks-sp-ring,
    .ks-sp-ripple {
      position: absolute;
      inset: 0;
      border-radius: 50%;
    }
    .ks-sp-ring {
      /* A 2px ring: the fill is masked to its outer band, so it sits on any surface. */
      -webkit-mask: radial-gradient(farthest-side, transparent calc(100% - 2.5px), #000 calc(100% - 2px));
      mask: radial-gradient(farthest-side, transparent calc(100% - 2.5px), #000 calc(100% - 2px));
    }
    :host(.ks-spinner-coarse) .ks-sp-ring,
    :host(.ks-spinner-coarse) .ks-sp-ripple {
      opacity: 0.5;
    }
    .ks-sp-ring[data-ring='thinking'] {
      background: conic-gradient(from 0deg, transparent 0 50%, #b9a8ff 100%);
      animation:
        ks-spin 2.6s linear infinite,
        ks-breath 2.6s ease-in-out infinite;
    }
    .ks-sp-ring[data-ring='working'] {
      background: repeating-conic-gradient(#e6e8ee 0 46deg, transparent 46deg 120deg);
      animation: ks-spin 1s linear infinite;
    }
    .ks-sp-ring[data-ring='speaking'] {
      background: #8fe0b0;
    }
    .ks-sp-ripple {
      animation: ks-ripple var(--ks-ripple, 0.9s) ease-out infinite;
    }
    .ks-sp-ring[data-ring='asking'] {
      background: #e0a84c;
    }
    .ks-sp-ring[data-ring='held'] {
      background: repeating-conic-gradient(#6aaefa 0 22deg, transparent 22deg 45deg);
    }
    .ks-sp-ring[data-ring='you'] {
      background: #2f6fd6;
    }
    .ks-sp-ring[data-ring='queued'] {
      background: repeating-conic-gradient(rgba(255, 255, 255, 0.3) 0 10deg, transparent 10deg 30deg);
    }
    .ks-sp-ring[data-ring='done'] {
      background: rgba(255, 255, 255, 0.16);
    }
    .ks-sp-ring[data-ring='error'] {
      background: #e86b6b;
    }
    .ks-sp-avatar {
      position: absolute;
      inset: calc(var(--ks-sz) * 0.135);
      border-radius: 50%;
      display: flex;
      align-items: center;
      justify-content: center;
      font: 600 calc(var(--ks-sz) * 0.3) / 1 'JetBrains Mono', ui-monospace, monospace;
      color: #fff;
      text-shadow: 0 1px 2px rgba(0, 0, 0, 0.5);
      text-transform: lowercase;
      overflow: hidden;
    }
    .ks-sp-drift,
    .ks-sp-second {
      background-size: 300% 300% !important;
      animation-name: ks-drift;
      animation-timing-function: ease-in-out;
      animation-iteration-count: infinite;
      animation-direction: alternate;
    }
    .ks-sp-hollow {
      background: transparent !important;
      box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.22);
      color: #8f9199;
      text-shadow: none;
    }
    .ks-sp-neutral {
      background: #2a2c31 !important;
      color: #8f9199;
      text-shadow: none;
    }
    .ks-sp-second {
      position: absolute;
      left: calc(var(--ks-sz) * -0.33);
      top: calc(var(--ks-sz) * 0.33);
      width: calc(var(--ks-sz) * 0.6);
      height: calc(var(--ks-sz) * 0.6);
      border-radius: 50%;
      box-shadow: 0 0 0 2px var(--ks-spinner-hole, var(--ks-bg-chrome));
      display: flex;
      align-items: center;
      justify-content: center;
      font: 600 calc(var(--ks-sz) * 0.23) / 1 'JetBrains Mono', ui-monospace, monospace;
      color: #fff;
    }
    .ks-sp-badge {
      position: absolute;
      right: calc(var(--ks-sz) * -0.1);
      bottom: calc(var(--ks-sz) * -0.1);
      width: max(11px, calc(var(--ks-sz) * 0.43));
      height: max(11px, calc(var(--ks-sz) * 0.43));
      border-radius: 50%;
      font: 700 max(7px, calc(var(--ks-sz) * 0.27)) / 1 Inter, system-ui, sans-serif;
      display: flex;
      align-items: center;
      justify-content: center;
      box-shadow: 0 0 0 2px var(--ks-spinner-hole, var(--ks-bg-chrome));
    }
    .ks-sp-badge[data-badge='asking'] {
      background: #e0a84c;
      color: #1b1406;
    }
    .ks-sp-badge[data-badge='held'] {
      background: #6aaefa;
      color: #0d1a2a;
      font-size: max(6px, calc(var(--ks-sz) * 0.2));
    }
    .ks-sp-badge[data-badge='done'] {
      background: #69b879;
      color: #0b1a0f;
    }
    .ks-sp-badge[data-badge='error'] {
      background: #e86b6b;
      color: #1e0b0b;
    }
    .ks-sp-badge[data-badge='you'] {
      background: #2f6fd6;
      color: #fff;
    }
    @keyframes ks-spin {
      to {
        transform: rotate(360deg);
      }
    }
    @keyframes ks-drift {
      0% {
        background-position: 0% 50%;
      }
      100% {
        background-position: 100% 50%;
      }
    }
    @keyframes ks-breath {
      0%,
      100% {
        opacity: 0.4;
      }
      50% {
        opacity: 1;
      }
    }
    @keyframes ks-ripple {
      0% {
        box-shadow: 0 0 0 0 rgba(143, 224, 176, 0.6);
      }
      100% {
        box-shadow: 0 0 0 calc(var(--ks-sz) * 0.2) rgba(143, 224, 176, 0);
      }
    }
    @media (prefers-reduced-motion: reduce) {
      .ks-sp-ring,
      .ks-sp-ripple,
      .ks-sp-drift,
      .ks-sp-second {
        animation: none !important;
      }
      .ks-sp-ripple {
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
    () => this.label() || describeSpinner(this.state(), this.machines(), { coarse: this.coarse(), tps: this.tps(), agent: this.glyph() ? this.agent() : '' }),
  );
}

/**
 * The machine's heat avatar on its own (Fleet rows, the footer machine list): the same
 * drifting fill without an activity ring. Rounded square at Fleet size, a dot in the footer.
 */
@Component({
  selector: 'ks-heat-avatar',
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
    '[class.ks-ha-drift]': 'heat() != null',
  },
  imports: [TooltipDirective],
  template: `<span class="ks-ha-wrap" [ksTooltip]="title()">@if (showInitials()) {<span aria-hidden="true">{{ initials() }}</span>}</span>`,
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
    .ks-ha-wrap {
      display: flex;
      align-items: center;
      justify-content: center;
      width: 100%;
      height: 100%;
    }
    :host(.ks-ha-drift) {
      background-size: 300% 300% !important;
      animation-name: ks-ha-drift;
      animation-timing-function: ease-in-out;
      animation-iteration-count: infinite;
      animation-direction: alternate;
    }
    @keyframes ks-ha-drift {
      0% {
        background-position: 0% 50%;
      }
      100% {
        background-position: 100% 50%;
      }
    }
    @media (prefers-reduced-motion: reduce) {
      :host(.ks-ha-drift) {
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

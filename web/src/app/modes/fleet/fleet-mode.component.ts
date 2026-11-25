import { ChangeDetectionStrategy, Component, DestroyRef, ElementRef, afterRenderEffect, computed, effect, inject, signal, untracked } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { ActivatedRoute, Router } from '@angular/router';
import { map } from 'rxjs';
import { HeatHistory, MACHINE_HISTORY_RETENTION } from '../../core/models/activity.models';
import { Worker } from '../../core/models/konsert.models';
import { ApiClientService } from '../../core/services/api-client.service';
import { isVersionMismatch } from '../../core/models/version';
import { taskTitle } from '../../core/state/activity-logic';
import { ControlStore } from '../../core/state/control.store';
import { FleetStore } from '../../core/state/fleet.store';
import { WorkStore } from '../../core/state/work.store';
import { ActivitySpinnerComponent, HeatAvatarComponent } from '../../shared/activity-spinner.component';
import { IconComponent } from '../../shared/icon.component';
import { SpinnerLegendComponent } from '../../shared/spinner-legend.component';
import { SPINNER_LABEL_COLORS, SPINNER_WORDS, heatTextColor, spinnerMachine } from '../../shared/spinner-logic';
import { TooltipDirective } from '../../shared/tooltip.directive';
import { ModeUiStore } from '../mode-ui.store';
import {
  HEAT_RANGES,
  HEAT_STRIP_POINTS,
  HeatRange,
  MachineRow,
  MachineSession,
  ShellRow,
  askAvailability,
  describeStrip,
  heatStrip,
  machineRows,
  queueReasons,
  shellChip,
  shellRows,
  shortAge,
  spoolNote,
  stripTicks,
  subagentChip,
  subagentText,
  transportHint,
  transportIcon,
  transportLabel,
} from './fleet-logic';
import { SubagentActivity } from '../../core/models/activity.models';

const WAKE_REASON = 'Keep Awake from the Konsert dashboard';

/**
 * 3 Fleet, machines first (signed-off design): each row is a machine (heat avatar, name,
 * spec, state badge, heat word and cause, Drain / Suspend / Details) with the sessions
 * running on it as cards (the same spinner, the agent's mark in the centre since the row is
 * the machine), its load and free slots, and its heat strip over the chosen range (12 h
 * default, 24 h, 3 days, 1 week; from the hub's heat history). Below: the queue and why
 * each task is waiting.
 */
@Component({
  selector: 'ks-fleet-mode',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective, ActivitySpinnerComponent, HeatAvatarComponent, SpinnerLegendComponent],
  host: { class: 'block h-full overflow-y-auto' },
  template: `
    <div class="px-4 py-3 flex flex-col gap-2.5 max-w-[1500px]">
      <div class="flex flex-wrap items-center gap-2.5 text-[12.5px]">
        <h2 id="ks-fleet-machines" class="m-0 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--ks-text-secondary)]">Machines · {{ rows().length }}</h2>
        <span class="hidden lg:inline text-[var(--ks-text-secondary)]">Each row is a machine, with the project work running on it.</span>
        <span class="flex-1"></span>
        <span class="text-[var(--ks-text-secondary)] cursor-help underline decoration-dotted decoration-[var(--ks-text-muted)] underline-offset-2" id="ks-heat-range-label" [ksTooltip]="retention()" data-testid="heat-retention">Heat over the last</span>
        <div class="inline-flex gap-1" role="radiogroup" aria-labelledby="ks-heat-range-label">
          @for (r of ranges; track r.id) {
            <button
              type="button"
              role="radio"
              class="ks-btn ks-btn-sm ks-btn-soft"
              [attr.aria-checked]="range() === r.id"
              [class.!bg-[var(--ks-blue-soft)]]="range() === r.id"
              [class.!text-[#cfe2ff]]="range() === r.id"
              [attr.data-range]="r.id"
              (click)="range.set(r.id)"
            >
              {{ r.label }}
            </button>
          }
        </div>
        <ks-spinner-legend class="ml-1" />
        @if (wakeCapable()) {
          <button
            type="button"
            class="ks-btn ks-btn-sm ks-btn-soft ml-1"
            data-testid="keep-awake-all"
            [attr.aria-pressed]="allAwake()"
            [class.text-[#e6b763]]="allAwake()"
            (click)="setWakeAll(!allAwake())"
            ksTooltip="Keep every machine awake (caffeinate on macOS, systemd-inhibit on Linux, the Windows execution state), including machines that connect later"
          >
            <app-icon name="coffee" [size]="12" />
            {{ allAwake() ? 'All awake' : 'Keep all awake' }}
          </button>
        }
        <button type="button" class="ks-btn ks-btn-sm ks-btn-soft ml-1" data-testid="fleet-add-machine" (click)="ui.addMachineOpen.set(true)">
          <app-icon name="plus" [size]="12" />
          Add machine
        </button>
      </div>

      @if (wakeError()) {
        <p role="alert" class="m-0 rounded-[var(--ks-radius-md)] bg-[var(--ks-red-soft)] px-3 py-2 text-[12px] text-[#f3a5a5]">{{ wakeError() }}</p>
      }
      @if (rows().length === 0) {
        <p class="text-[12.5px] text-[var(--ks-text-muted)]" data-testid="fleet-machines">No machines yet. Add this machine or one over SSH to start.</p>
      } @else {
        <div class="flex flex-col gap-2.5" role="list" aria-labelledby="ks-fleet-machines" data-testid="fleet-machines">
          @for (r of rows(); track r.w.id) {
            <section
              role="listitem"
              class="ks-machine"
              [class.ks-machine-selected]="selectedSid() === r.w.sid"
              [class.ks-machine-off]="r.w.status !== 'online'"
              [attr.aria-labelledby]="'ks-m-' + r.w.id"
              [attr.data-machine]="r.w.sid"
            >
              <div class="flex gap-3 items-start min-w-0">
                <ks-heat-avatar [machine]="machineOf(r)" [size]="44" shape="square" />
                <div class="ks-minfo min-w-0 flex-1 flex flex-col gap-[3px]">
                  <!-- The name gets the room: badges wrap under it rather than cut it off. -->
                  <div class="ks-mtitle">
                    <h3 class="ks-mname m-0 min-w-0 max-w-full text-[14px] font-semibold" [id]="'ks-m-' + r.w.id">
                      <button type="button" class="ks-focusable rounded-sm text-left" [attr.aria-current]="selectedSid() === r.w.sid ? 'true' : null" (click)="select(r.w)">{{ r.w.name }}</button>
                    </h3>
                    <span class="ks-mbadges">
                      <span class="ks-badge" [attr.data-badge]="r.badge">{{ r.badge }}</span>
                      <span
                        class="ks-badge ks-transport"
                        role="img"
                        data-testid="machine-transport"
                        [attr.data-transport]="transportLabel(r.w)"
                        [attr.aria-label]="'transport ' + transportLabel(r.w)"
                        [ksTooltip]="transportLabel(r.w) + ': ' + transportHint(r.w)"
                        ><app-icon [name]="transportIcon(r.w)" [size]="10" /><span class="ks-transport-label" data-testid="transport-label">{{ transportLabel(r.w) }}</span></span
                      >
                      @if (versionMismatch(r.w); as note) {
                        <span class="ks-badge" data-badge="VERSION" role="img" data-testid="version-mismatch-badge" [attr.aria-label]="note" [ksTooltip]="note">VERSION</span>
                      }
                    </span>
                  </div>
                  <div class="text-[12px] text-[var(--ks-text-secondary)] truncate">{{ r.spec }}</div>
                  <div class="text-[12px]" [style.color]="heatColor(r)" data-testid="machine-heat">{{ r.heatText }}</div>
                  <div class="flex flex-wrap gap-1 mt-1">
                    @if (r.w.status === 'online') {
                      <button type="button" class="ks-btn ks-btn-sm" [disabled]="control.isBusy(r.w.id)" (click)="control.setDrained(r.w.id, !r.w.drained)" [ksTooltip]="r.w.drained ? 'Accept new work again' : 'Finish what runs, accept no new work'">
                        {{ r.w.drained ? 'Undrain' : 'Drain' }}
                      </button>
                      @if (r.suspended && !r.running) {
                        <button type="button" class="ks-btn ks-btn-sm" [disabled]="control.isBusy(r.w.id)" (click)="control.workerBulk(r.w.id, 'resume-all')" ksTooltip="Resume every suspended session on this machine">Resume</button>
                      } @else {
                        <button type="button" class="ks-btn ks-btn-sm" [disabled]="control.isBusy(r.w.id) || !r.running" (click)="control.workerBulk(r.w.id, 'suspend-all')" ksTooltip="Freeze every session on this machine (nothing is lost)">Suspend</button>
                      }
                    }
                    @if (r.w.status === 'online' && r.w.wake_lock?.supported) {
                      <button
                        type="button"
                        class="ks-btn ks-btn-sm"
                        [attr.aria-pressed]="wakeOn(r.w)"
                        [attr.data-testid]="'keep-awake-' + r.w.sid"
                        [class.text-[#e6b763]]="wakeOn(r.w)"
                        (click)="setWake(r.w, !wakeOn(r.w))"
                        [ksTooltip]="wakeTip(r.w)"
                      >
                        <app-icon name="coffee" [size]="11" />
                        {{ wakeOn(r.w) ? (r.w.wake_lock?.active ? 'Awake' : 'Waking…') : 'Keep awake' }}
                      </button>
                    }
                    @if (r.w.launch_state === 'failed') {
                      <button type="button" class="ks-btn ks-btn-sm" (click)="fleet.retryStart(r.w.id)" [ksTooltip]="'Start konsertd on ' + r.w.name + ' again'">Retry start</button>
                    }
                    <button type="button" class="ks-btn ks-btn-sm" [attr.data-testid]="'machine-details-' + r.w.sid" (click)="ui.inspectedWorkerId.set(r.w.id)">Details</button>
                  </div>
                </div>
              </div>

              <div class="flex flex-col gap-2 min-w-0">
                <ul class="m-0 p-0 list-none flex flex-wrap items-start gap-2" [attr.aria-label]="'Sessions on ' + r.w.name">
                  @for (s of r.sessions; track s.key) {
                    <li class="ks-sess-li" [class.ks-sess-li-wide]="!!s.detail" [class.ks-sess-done]="s.done" [attr.data-session-card]="s.sid">
                      <button type="button" class="ks-focusable ks-sess" [class.ks-sess-left]="s.done && !!s.detail?.shells" [attr.data-session]="s.sid" [attr.data-state]="s.state" [disabled]="!s.item" (click)="open(s)">
                        <ks-spinner [state]="s.state" [machines]="[machineOf(r)]" [glyph]="true" [agent]="s.agent" [coarse]="s.coarse" [tps]="s.tps" [detail]="s.detail" />
                        <span class="min-w-0 flex-1">
                          <span class="flex items-center gap-1.5 text-[11px] text-[var(--ks-text-secondary)] min-w-0">
                            <span class="w-[7px] h-[7px] rounded-[2px] shrink-0" [style.background]="s.projectColor" aria-hidden="true"></span>
                            <span class="truncate">{{ s.project }} · {{ s.agent }}</span>
                          </span>
                          <span class="block truncate text-[12.5px] text-[var(--ks-text)]">{{ s.title }}</span>
                          <span class="block truncate text-[11.5px]" [style.color]="colors[s.state]" [ksTooltip]="s.nowTitle" data-testid="fleet-now"><span class="ks-sr-only">{{ words[s.state] }}: </span>{{ s.now }}</span>
                        </span>
                      </button>
                      @if (s.detail) {
                        <div class="ks-sess-detail" data-testid="session-detail">
                          <div class="flex flex-wrap gap-1.5">
                            @if (shellChip(s); as c) {
                              <span class="ks-chip" [class.ks-chip-left]="s.done" data-testid="shell-chip">{{ c }}</span>
                            }
                            @if (subChip(s); as c) {
                              <span class="ks-chip ks-chip-sub" data-testid="subagent-chip">{{ c }}</span>
                            }
                          </div>
                          @if (s.shellList.length || s.subagents.length) {
                            <ul class="m-0 p-0 list-none grid gap-1" [attr.aria-label]="'Shells and sub-agents of ' + s.title">
                              @for (sh of rowsOf(s); track sh.pid) {
                                <li class="ks-row" data-testid="shell-row" [attr.data-pid]="sh.pid">
                                  <span class="ks-row-dot ks-row-shell" [class.ks-row-busy]="sh.busy" aria-hidden="true"></span>
                                  <span class="ks-row-kind">shell</span>
                                  <span class="ks-row-text" [ksTooltip]="sh.title">{{ sh.text }}</span>
                                  <span class="ks-row-age"><span class="ks-sr-only">{{ sh.busy ? 'busy for' : '' }} </span>{{ sh.age }}</span>
                                  <span class="flex gap-1 shrink-0">
                                    <button
                                      type="button"
                                      class="ks-btn ks-btn-sm ks-row-btn"
                                      data-testid="shell-ask"
                                      [disabled]="!askOf(s).ok || busy(s.key)"
                                      [ksTooltip]="askOf(s).why"
                                      [attr.aria-label]="'Ask the agent to stop ' + sh.text"
                                      (click)="ask(s, [sh])"
                                    >
                                      Ask to stop
                                    </button>
                                    <button
                                      type="button"
                                      class="ks-btn ks-btn-sm ks-row-btn ks-row-kill"
                                      data-testid="shell-kill"
                                      [disabled]="busy(s.key)"
                                      [ksTooltip]="'Kill pid ' + sh.pid + ' and everything it started'"
                                      [attr.aria-label]="'Kill ' + sh.text"
                                      (click)="kill(s, [sh])"
                                    >
                                      Kill
                                    </button>
                                  </span>
                                </li>
                              }
                              @for (a of s.subagents; track a.id) {
                                <li class="ks-row" data-testid="subagent-row" [attr.data-subagent]="a.id">
                                  <span class="ks-row-dot ks-row-sub" [attr.data-dot]="a.state" aria-hidden="true"></span>
                                  <span class="ks-row-kind" [style.color]="colors[a.state]">sub-agent</span>
                                  <span class="ks-row-text" [ksTooltip]="subText(a)"><span class="ks-sr-only">{{ words[a.state] }}: </span>{{ subText(a) }}</span>
                                  <span class="ks-row-age">{{ ageOf(a.since) }}</span>
                                </li>
                              }
                            </ul>
                          }
                          @if ((s.detail.shells ?? 0) > 1) {
                            <div class="flex flex-wrap items-center gap-1.5">
                              <button type="button" class="ks-btn ks-btn-sm ks-row-kill" data-testid="shell-kill-all" [disabled]="busy(s.key)" (click)="kill(s, [])">Clean up all ({{ s.detail.shells }})</button>
                              <button type="button" class="ks-btn ks-btn-sm" data-testid="shell-ask-all" [disabled]="!askOf(s).ok || busy(s.key)" [ksTooltip]="askOf(s).why" (click)="ask(s, [])">Ask the agent to clean up</button>
                            </div>
                          }
                          @if (notes()[s.key]; as n) {
                            <p class="m-0 text-[11.5px]" [class.text-[#f3a5a5]]="n.error" [class.text-[var(--ks-text-secondary)]]="!n.error" role="status" data-testid="shell-note">{{ n.text }}</p>
                          }
                        </div>
                      }
                    </li>
                  }
                  @if (r.next) {
                    <li class="ks-extra ks-extra-next">next: {{ r.next }} (queued)</li>
                  }
                  @if (r.load) {
                    <li class="ks-extra" [attr.aria-label]="'Machine load: ' + r.load" ksTooltip="The whole machine: agents and everything else running on it">machine · {{ r.load }}</li>
                  }
                  @for (f of slots(r.free); track $index) {
                    <li class="ks-extra ks-extra-free">free slot</li>
                  }
                  @if (spoolNote(r.w); as note) {
                    <li class="ks-extra" [class.ks-extra-alert]="r.w.spool?.full" data-testid="machine-spool">{{ note }}</li>
                  }
                  @if (r.w.reconnecting_after_sleep) {
                    <li class="ks-extra" data-testid="machine-reconnecting">reconnecting after sleep…</li>
                  } @else if (r.w.status !== 'online') {
                    <li class="ks-extra">{{ offlineText(r.w) }}</li>
                  } @else if (!r.sessions.length && !r.free && !r.load) {
                    <li class="ks-extra">nothing running</li>
                  }
                </ul>
                <div class="flex items-center gap-2.5">
                  <span class="ks-strip" role="img" [attr.aria-label]="strips()[r.w.id]?.label" data-testid="heat-strip">
                    @for (c of strips()[r.w.id]?.cells; track $index) {
                      <span [class.ks-nodata]="!c.color" [style.background]="c.color"></span>
                    }
                  </span>
                  <span class="font-mono-ide text-[10.5px] text-[var(--ks-text-muted)] w-[110px] shrink-0">heat · last {{ range() }}</span>
                </div>
              </div>
            </section>
          }
          <div class="hidden min-[901px]:grid grid-cols-[minmax(0,290px)_minmax(0,1fr)] gap-x-[18px] px-[14px]" aria-hidden="true">
            <span></span>
            <span class="flex items-center gap-2.5">
              <span class="relative block flex-1 h-4 text-[10.5px] text-[var(--ks-text-muted)] font-mono-ide">
                @for (t of ticks(); track $index) {
                  <span class="absolute whitespace-nowrap" [class.-translate-x-1/2]="!$first && !$last" [class.-translate-x-full]="$last" [style.left.%]="t.left">{{ t.label }}</span>
                }
              </span>
              <span class="w-[110px] shrink-0"></span>
            </span>
          </div>
        </div>
      }

      <section class="rounded-[var(--ks-radius-lg)] bg-[var(--ks-surface-1)] px-[14px] py-3" aria-labelledby="ks-fleet-queue" data-testid="fleet-queue">
        <h2 id="ks-fleet-queue" class="m-0 mb-2 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--ks-text-secondary)]">Queue · why it's waiting</h2>
        @if (queue().length === 0) {
          <p class="m-0 text-[12.5px] text-[var(--ks-text-muted)]">Nothing is waiting for a machine.</p>
        } @else {
          <ul class="m-0 p-0 list-none flex flex-col gap-1.5">
            @for (q of queue(); track q.id) {
              <li class="flex flex-wrap items-center gap-x-3 gap-y-1 text-[12.5px]" [attr.data-queued]="q.sid">
                <span class="font-mono-ide text-[var(--ks-text-muted)]">{{ q.sid }}</span>
                <span class="text-[var(--ks-text)]">{{ q.title }}</span>
                @for (why of q.reasons; track $index) {
                  @if (!$first) {
                    <span class="text-[var(--ks-text-muted)]" aria-hidden="true">·</span>
                  }
                  <span [class.text-[var(--ks-amber)]]="why.tone === 'warn'" [class.text-[var(--ks-text-secondary)]]="why.tone === 'muted'">{{ why.text }}</span>
                }
                <span class="flex-1"></span>
                <button type="button" class="ks-btn ks-btn-sm" (click)="ui.openInCommand(q.sid)">Open</button>
              </li>
            }
          </ul>
        }
      </section>
    </div>
  `,
  styles: `
    :host {
      --ks-spinner-hole: #1c1d21;
    }
    .ks-machine {
      display: grid;
      grid-template-columns: minmax(0, 290px) minmax(0, 1fr);
      gap: 8px 18px;
      padding: 12px 14px;
      border-radius: var(--ks-radius-lg);
      background: var(--ks-surface-1);
    }
    @media (max-width: 900px) {
      .ks-machine {
        grid-template-columns: minmax(0, 1fr);
      }
    }
    .ks-machine-selected {
      box-shadow: inset 0 0 0 1px var(--ks-blue);
    }
    .ks-machine-off {
      opacity: 0.75;
    }
    .ks-sess-li {
      display: flex;
      flex-direction: column;
      flex: 1 1 230px;
      min-width: 230px;
      max-width: 320px;
      border-radius: 10px;
      background: #1c1d21;
    }
    .ks-sess-li-wide {
      flex: 1 1 380px;
      max-width: 560px;
    }
    .ks-sess-done > .ks-sess {
      opacity: 0.7;
    }
    .ks-sess-li:has(.ks-sess-left) {
      box-shadow: inset 0 0 0 1px rgba(224, 168, 76, 0.25);
    }
    .ks-sess-detail {
      display: grid;
      gap: 6px;
      padding: 0 12px 10px 50px;
    }
    .ks-chip {
      font: 11px/1.5 'JetBrains Mono', ui-monospace, monospace;
      padding: 1px 8px;
      border-radius: 10px;
      background: rgba(230, 232, 238, 0.08);
      color: #ececed;
      white-space: nowrap;
    }
    .ks-chip-sub {
      background: rgba(185, 168, 255, 0.12);
      color: #c9bcff;
    }
    .ks-chip-left {
      background: rgba(224, 168, 76, 0.14);
      color: #e9c27f;
    }
    .ks-row {
      display: flex;
      align-items: center;
      gap: 8px;
      min-width: 0;
      font-size: 12px;
    }
    .ks-row-dot {
      width: 8px;
      height: 8px;
      flex: none;
    }
    .ks-row-shell {
      border-radius: 2px;
      background: #e6e8ee;
      opacity: 0.45;
    }
    .ks-row-shell.ks-row-busy {
      opacity: 1;
      animation: ks-row-blink 0.8s ease-in-out infinite;
    }
    .ks-row-sub {
      border-radius: 50%;
    }
    .ks-row-sub[data-dot='thinking'] {
      background: #b9a8ff;
      animation: ks-row-breath 2s ease-in-out infinite;
    }
    .ks-row-sub[data-dot='working'] {
      background: #e6e8ee;
      animation: ks-row-blink 0.8s ease-in-out infinite;
    }
    .ks-row-sub[data-dot='speaking'] {
      background: #8fe0b0;
    }
    .ks-row-sub[data-dot='asking'] {
      background: #e0a84c;
    }
    .ks-row-kind {
      width: 62px;
      flex: none;
      font: 11px 'JetBrains Mono', ui-monospace, monospace;
      color: #c9cbd1;
    }
    .ks-row-text {
      flex: 1;
      min-width: 0;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      color: #c9cbd1;
    }
    .ks-row-age {
      width: 34px;
      flex: none;
      text-align: right;
      font: 11px 'JetBrains Mono', ui-monospace, monospace;
      color: #8f9199;
    }
    .ks-row-btn {
      padding: 1px 7px;
      font-size: 11px;
    }
    .ks-row-kill {
      color: #f3a3a3;
    }
    @keyframes ks-row-blink {
      0%,
      100% {
        opacity: 1;
      }
      50% {
        opacity: 0.35;
      }
    }
    @keyframes ks-row-breath {
      0%,
      100% {
        opacity: 0.4;
      }
      50% {
        opacity: 1;
      }
    }
    @media (prefers-reduced-motion: reduce) {
      .ks-row-dot {
        animation: none !important;
      }
    }
    .ks-sess {
      display: flex;
      flex: 1;
      min-width: 0;
      gap: 10px;
      align-items: center;
      padding: 8px 12px 8px 10px;
      border: 0;
      border-radius: 10px;
      background: transparent;
      color: var(--ks-text);
      text-align: left;
      cursor: pointer;
    }
    .ks-sess:hover:not(:disabled) {
      background: #222328;
    }
    .ks-sess:disabled {
      cursor: default;
    }
    .ks-extra {
      display: flex;
      align-items: center;
      padding: 8px 12px;
      border-radius: 10px;
      font-size: 12px;
      color: var(--ks-text-secondary);
      background: rgba(255, 255, 255, 0.04);
    }
    .ks-extra-free {
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.12);
    }
    .ks-extra-alert {
      color: #f09595;
      background: var(--ks-red-soft);
    }
    .ks-extra-next {
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(224, 168, 76, 0.4);
      color: #e9c27f;
    }
    .ks-strip {
      display: flex;
      flex: 1;
      gap: 1px;
      height: 10px;
      border-radius: 3px;
      overflow: hidden;
    }
    .ks-strip > span {
      flex: 1;
    }
    .ks-nodata {
      background: repeating-linear-gradient(135deg, rgba(255, 255, 255, 0.18) 0 3px, transparent 3px 6px);
    }
    .ks-badge {
      display: inline-flex;
      padding: 1px 6px;
      border-radius: 4px;
      font-family: 'JetBrains Mono', ui-monospace, monospace;
      font-size: 10.5px;
      color: var(--ks-text-muted);
      background: rgba(255, 255, 255, 0.05);
      white-space: nowrap;
    }
    .ks-badge[data-badge='FULL'] {
      color: #1b1406;
      background: var(--ks-amber);
    }
    .ks-badge[data-badge='DRAINING'] {
      color: #9ec5ff;
      background: transparent;
      box-shadow: inset 0 0 0 1px var(--ks-blue);
    }
    .ks-badge[data-badge='OFFLINE'],
    .ks-badge[data-badge='RECONNECTING'],
    .ks-badge[data-badge='STARTING'] {
      background: repeating-linear-gradient(135deg, rgba(255, 255, 255, 0.08) 0 4px, transparent 4px 8px);
    }
    .ks-badge[data-badge='VERSION'] {
      color: #e9c27f;
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(224, 168, 76, 0.5);
    }
    .ks-badge[data-badge='IDLE'] {
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.12);
    }
    .ks-minfo {
      container: ks-minfo / inline-size;
    }
    .ks-mtitle {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 2px 8px;
      min-width: 0;
    }
    /* A name longer than the column wraps (at its hyphens, else anywhere) to two lines
       instead of being cut off; longer names are clipped after two lines. */
    .ks-mname {
      overflow-wrap: anywhere;
      display: -webkit-box;
      -webkit-box-orient: vertical;
      -webkit-line-clamp: 2;
      line-clamp: 2;
      overflow: hidden;
    }
    .ks-mbadges {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      flex-shrink: 0;
    }
    .ks-transport {
      align-items: center;
      gap: 4px;
      background: transparent;
      color: var(--ks-text-secondary);
    }
    /* Narrow: the transport shrinks to its icon (the tooltip and accessible name keep it). */
    @media (max-width: 640px) {
      .ks-transport-label {
        display: none;
      }
    }
    @container ks-minfo (max-width: 180px) {
      .ks-transport-label {
        display: none;
      }
    }
    .ks-transport[data-transport='ssh'] {
      color: #9ec5ff;
    }
  `,
})
export class FleetModeComponent {
  readonly ui = inject(ModeUiStore);
  readonly control = inject(ControlStore);
  readonly fleet = inject(FleetStore);
  private readonly work = inject(WorkStore);
  private readonly api = inject(ApiClientService);
  private readonly router = inject(Router);
  private readonly host = inject<ElementRef<HTMLElement>>(ElementRef);
  readonly ranges = HEAT_RANGES;
  readonly colors = SPINNER_LABEL_COLORS;
  readonly words = SPINNER_WORDS;

  readonly selectedSid = toSignal(inject(ActivatedRoute).paramMap.pipe(map((p) => p.get('sid') ?? '')), { initialValue: '' });
  readonly range = signal<HeatRange>('12h');
  private readonly rangeTitle = computed(() => HEAT_RANGES.find((r) => r.id === this.range())!.title);
  private readonly now = signal(Date.now());
  /** A faster clock for shell and sub-agent ages (the rows tick while a command runs). */
  readonly clock = signal(Date.now());
  readonly history = signal<HeatHistory | null>(null);
  /** How long machine history is kept, in plain words (tooltip on the range label). */
  readonly retention = computed(() => this.history()?.retention || MACHINE_HISTORY_RETENTION);
  readonly wakeError = signal('');
  readonly wakeCapable = computed(() => this.fleet.workers().some((w) => w.status === 'online' && w.wake_lock?.supported));
  readonly allAwake = computed(() => {
    const able = this.fleet.workers().filter((w) => w.status === 'online' && w.wake_lock?.supported);
    return able.length > 0 && able.every((w) => this.wakeOn(w));
  });

  readonly rows = computed(() =>
    machineRows({
      workers: this.fleet.workers(),
      tasks: this.fleet.tasks(),
      groups: this.work.projectGroups(),
      items: this.work.byId(),
      allItems: this.work.items(),
      heat: this.fleet.heatByWorker(),
      now: this.now(),
    }),
  );

  readonly strips = computed(() => {
    const h = this.history();
    const out: Record<string, { cells: ReturnType<typeof heatStrip>; label: string }> = {};
    for (const w of this.fleet.workers()) {
      const cells = heatStrip(h, w.id);
      out[w.id] = { cells, label: describeStrip(w.name, cells, this.rangeTitle()) };
    }
    return out;
  });

  readonly ticks = computed(() => {
    const h = this.history();
    const end = h ? Date.parse(h.end) : this.now();
    const start = h ? Date.parse(h.start) : end - HEAT_RANGES.find((r) => r.id === this.range())!.ms;
    return stripTicks(start, end, this.range());
  });

  readonly queue = computed(() => queueReasons(this.fleet.tasks(), this.fleet.workers(), (t) => taskTitle(t).text));

  constructor() {
    // Heat history: on range change and every minute.
    effect(() => {
      const r = this.range();
      this.now();
      untracked(() => void this.loadHistory(r));
    });
    const t = setInterval(() => this.now.set(Date.now()), 60_000);
    const c = setInterval(() => this.clock.set(Date.now()), 5_000);
    inject(DestroyRef).onDestroy(() => {
      clearInterval(t);
      clearInterval(c);
    });
    // /fleet/<sid> scrolls that machine into view.
    afterRenderEffect(() => {
      const sid = this.selectedSid();
      if (!sid) return;
      this.host.nativeElement.querySelector(`[data-machine="${CSS.escape(sid)}"]`)?.scrollIntoView({ block: 'nearest' });
    });
  }

  private async loadHistory(r: HeatRange): Promise<void> {
    try {
      const h = await this.api.getHeatHistory(r, HEAT_STRIP_POINTS);
      if (this.range() === r) this.history.set(h);
    } catch {
      // No history (older hub, offline): the strip shows "no data".
      if (this.range() === r) this.history.set(null);
    }
  }

  versionMismatch(w: Worker): string {
    const hub = this.fleet.controllerVersion();
    return isVersionMismatch(hub, w.version) ? `Version mismatch: ${w.name} runs konsertd ${w.version}, hub is ${hub}` : '';
  }

  wakeOn(w: Worker): boolean {
    return w.wake_lock?.desired ?? !!w.wake_lock?.active;
  }

  wakeTip(w: Worker): string {
    const wl = w.wake_lock;
    if (wl?.active) return `${w.name} is kept awake${wl.method ? ` (${wl.method})` : ''}; press to let it sleep`;
    if (this.wakeOn(w)) return wl?.error ? `Couldn't keep ${w.name} awake: ${wl.error}` : `Asking ${w.name} to stay awake…`;
    return `Keep ${w.name} from sleeping while you're away`;
  }

  async setWake(w: Worker, on: boolean): Promise<void> {
    this.wakeError.set('');
    try {
      await this.api.setWorkerWakeLock(w.id, on, WAKE_REASON);
    } catch (e) {
      this.wakeError.set(`Could not update ${w.name}: ${e instanceof Error ? e.message : String(e)}`);
    }
  }

  async setWakeAll(on: boolean): Promise<void> {
    this.wakeError.set('');
    try {
      await this.api.setFleetWakeLock(on, WAKE_REASON);
    } catch (e) {
      this.wakeError.set(`Could not ${on ? 'enable' : 'disable'} Keep awake: ${e instanceof Error ? e.message : String(e)}`);
    }
  }

  machineOf(r: MachineRow) {
    return spinnerMachine(r.w.name, r.heat);
  }

  heatColor(r: MachineRow): string {
    return heatTextColor(r.heat ? r.heat.score : null);
  }

  slots(n: number): number[] {
    return Array.from({ length: Math.min(n, 6) });
  }

  readonly transportLabel = transportLabel;
  readonly transportHint = transportHint;
  readonly transportIcon = transportIcon;

  spoolNote(w: Worker): string | undefined {
    return spoolNote(w, Date.now());
  }

  offlineText(w: Worker): string {
    if (w.launch_state === 'starting') return 'starting konsertd…';
    if (w.launch_state === 'failed') return `could not start: ${w.launch_error || 'see Details'}`;
    return 'offline: not reporting';
  }

  select(w: Worker): void {
    void this.router.navigate(this.selectedSid() === w.sid ? ['/fleet'] : ['/fleet', w.sid]);
  }

  open(s: MachineSession): void {
    if (s.item) this.ui.openInCommand(s.item);
  }

  // --- shells and sub-agents ------------------------------------------------------------

  /** Per-session result of the last cleanup action (keyed by session key). */
  readonly notes = signal<Record<string, { text: string; error: boolean }>>({});
  private readonly pending = signal<Record<string, boolean>>({});

  shellChip(s: MachineSession): string {
    return shellChip(s.detail, s.done);
  }

  subChip(s: MachineSession): string {
    return subagentChip(s.detail);
  }

  rowsOf(s: MachineSession): ShellRow[] {
    return shellRows(s.shellList, this.clock());
  }

  subText(a: SubagentActivity): string {
    return subagentText(a);
  }

  ageOf(since: string): string {
    return shortAge(this.clock() - (Date.parse(since) || this.clock()));
  }

  askOf(s: MachineSession) {
    return askAvailability(s);
  }

  busy(key: string): boolean {
    return !!this.pending()[key];
  }

  /** Ask the session's agent to stop shells (all of them with an empty list). */
  async ask(s: MachineSession, shells: ShellRow[]): Promise<void> {
    const res = await this.shellAction(s, 'ask', shells);
    if (res) {
      const what = shells.length === 1 ? shells[0].text : 'its shells';
      this.note(s.key, res.delivery === 'after_turn' ? `The agent will be asked to stop ${what} when its turn ends.` : `Asked the agent to stop ${what}.`);
    }
  }

  /** Kill shells (all of them with an empty list) after a confirmation. */
  async kill(s: MachineSession, shells: ShellRow[]): Promise<void> {
    const n = shells.length || s.detail?.shells || 0;
    const what = shells.length === 1 ? `${shells[0].text} (pid ${shells[0].pid})` : `all ${n} shells`;
    if (!window.confirm(`Kill ${what} of “${s.title}” and every process it started?`)) return;
    const res = await this.shellAction(s, 'kill', shells);
    if (res) this.note(s.key, shells.length === 1 ? `Killing ${shells[0].text}…` : `Killing ${res.shells} shells…`);
  }

  private async shellAction(s: MachineSession, action: 'kill' | 'ask', shells: ShellRow[]) {
    this.pending.update((p) => ({ ...p, [s.key]: true }));
    try {
      return await this.api.shellAction(
        s.kind,
        s.id,
        action,
        shells.map((x) => ({ pid: x.pid, started_at: x.startedAt })),
      );
    } catch (e) {
      this.note(s.key, e instanceof Error ? e.message : String(e), true);
      return null;
    } finally {
      this.pending.update((p) => ({ ...p, [s.key]: false }));
    }
  }

  private note(key: string, text: string, error = false): void {
    this.notes.update((n) => ({ ...n, [key]: { text, error } }));
  }
}

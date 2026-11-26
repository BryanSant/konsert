import { ChangeDetectionStrategy, Component, DestroyRef, ElementRef, afterRenderEffect, computed, effect, inject, signal, untracked } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { ActivatedRoute, Router } from '@angular/router';
import { map } from 'rxjs';
import { HeatHistory, MACHINE_HISTORY_RETENTION } from '../../core/models/activity.models';
import { Worker } from '../../core/models/syre.models';
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
  offlineText,
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

const WAKE_REASON = 'Keep Awake from the Syre dashboard';

/**
 * 3 Fleet, machines first (signed-off design): each row is a machine (heat avatar, name,
 * spec, state badge, heat word and cause, Drain / Suspend / Details) with the sessions
 * running on it as cards (the same spinner, the agent's mark in the centre since the row is
 * the machine), its load and free slots, and its heat strip over the chosen range (12 h
 * default, 24 h, 3 days, 1 week; from the hub's heat history). Below: the queue and why
 * each task is waiting.
 */
@Component({
  selector: 'sy-fleet-mode',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [IconComponent, TooltipDirective, ActivitySpinnerComponent, HeatAvatarComponent, SpinnerLegendComponent],
  host: { class: 'block h-full overflow-y-auto' },
  template: `
    <div class="px-4 py-3 flex flex-col gap-2.5 max-w-[1500px]">
      <div class="flex flex-wrap items-center gap-2.5 text-[12.5px]">
        <h2 id="sy-fleet-machines" class="m-0 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--sy-text-secondary)]">Machines · {{ rows().length }}</h2>
        <span class="hidden lg:inline text-[var(--sy-text-secondary)]">Each row is a machine, with the project work running on it.</span>
        <span class="flex-1"></span>
        <span class="text-[var(--sy-text-secondary)] cursor-help underline decoration-dotted decoration-[var(--sy-text-muted)] underline-offset-2" id="sy-heat-range-label" [syTooltip]="retention()" data-testid="heat-retention">Heat over the last</span>
        <div class="inline-flex gap-1" role="radiogroup" aria-labelledby="sy-heat-range-label">
          @for (r of ranges; track r.id) {
            <button
              type="button"
              role="radio"
              class="sy-btn sy-btn-sm sy-btn-soft"
              [attr.aria-checked]="range() === r.id"
              [class.!bg-[var(--sy-blue-soft)]]="range() === r.id"
              [class.!text-[#cfe2ff]]="range() === r.id"
              [attr.data-range]="r.id"
              (click)="range.set(r.id)"
            >
              {{ r.label }}
            </button>
          }
        </div>
        <sy-spinner-legend class="ml-1" />
        @if (wakeCapable()) {
          <button
            type="button"
            class="sy-btn sy-btn-sm sy-btn-soft ml-1"
            data-testid="keep-awake-all"
            [attr.aria-pressed]="allAwake()"
            [class.text-[#e6b763]]="allAwake()"
            (click)="setWakeAll(!allAwake())"
            syTooltip="Keep every machine awake (caffeinate on macOS, systemd-inhibit on Linux, the Windows execution state), including machines that connect later"
          >
            <app-icon name="coffee" [size]="12" />
            {{ allAwake() ? 'All awake' : 'Keep all awake' }}
          </button>
        }
        <button type="button" class="sy-btn sy-btn-sm sy-btn-soft ml-1" data-testid="fleet-add-machine" (click)="ui.addMachineOpen.set(true)">
          <app-icon name="plus" [size]="12" />
          Add machine
        </button>
      </div>

      @if (wakeError()) {
        <p role="alert" class="m-0 rounded-[var(--sy-radius-md)] bg-[var(--sy-red-soft)] px-3 py-2 text-[12px] text-[#f3a5a5]">{{ wakeError() }}</p>
      }
      @if (missingSid()) {
        <div class="flex flex-wrap items-center justify-between gap-2 rounded-[var(--sy-radius-md)] bg-[var(--sy-surface-1)] border border-[var(--sy-hairline)] px-3 py-2 text-[12.5px] text-[var(--sy-text-secondary)]" role="status" data-testid="fleet-not-found">
          <span>No machine {{ missingSid() }} here (it may have been removed).</span>
          <button type="button" class="sy-btn sy-btn-sm sy-btn-soft" (click)="clearSelection()">Show all machines</button>
        </div>
      }
      @if (rows().length === 0) {
        <p class="text-[12.5px] text-[var(--sy-text-muted)]" data-testid="fleet-machines">No machines yet. Add this machine or one over SSH to start.</p>
      } @else {
        <div class="flex flex-col gap-2.5" role="list" aria-labelledby="sy-fleet-machines" data-testid="fleet-machines">
          @for (r of rows(); track r.w.id) {
            <section
              role="listitem"
              class="sy-machine"
              [class.sy-machine-selected]="selectedWorker()?.id === r.w.id"
              [class.sy-machine-off]="r.w.status !== 'online'"
              [attr.aria-labelledby]="'sy-m-' + r.w.id"
              [attr.data-machine]="r.w.sid"
            >
              <div class="flex gap-3 items-start min-w-0">
                <sy-heat-avatar [machine]="machineOf(r)" [size]="44" shape="square" />
                <div class="sy-minfo min-w-0 flex-1 flex flex-col gap-[3px]">
                  <!-- The name gets the room: badges wrap under it rather than cut it off. -->
                  <div class="sy-mtitle">
                    <h3 class="sy-mname m-0 min-w-0 max-w-full text-[14px] font-semibold" [id]="'sy-m-' + r.w.id">
                      <button type="button" class="sy-focusable rounded-sm text-left" [attr.aria-current]="selectedWorker()?.id === r.w.id ? 'true' : null" (click)="select(r.w)">{{ r.w.name }}</button>
                    </h3>
                    <span class="sy-mbadges">
                      <span class="sy-badge" [attr.data-badge]="r.badge">{{ r.badge }}</span>
                      <span
                        class="sy-badge sy-transport"
                        role="img"
                        data-testid="machine-transport"
                        [attr.data-transport]="transportLabel(r.w)"
                        [attr.aria-label]="'transport ' + transportLabel(r.w)"
                        [syTooltip]="transportLabel(r.w) + ': ' + transportHint(r.w)"
                        ><app-icon [name]="transportIcon(r.w)" [size]="10" /><span class="sy-transport-label" data-testid="transport-label">{{ transportLabel(r.w) }}</span></span
                      >
                      @if (versionMismatch(r.w); as note) {
                        <span class="sy-badge" data-badge="VERSION" role="img" data-testid="version-mismatch-badge" [attr.aria-label]="note" [syTooltip]="note">VERSION</span>
                      }
                      @for (b of r.directiveBadges; track b.id) {
                        <span class="sy-badge" data-badge="DIRECTIVE" data-testid="directive-badge">{{ b.text }}</span>
                      }
                    </span>
                  </div>
                  <div class="text-[12px] text-[var(--sy-text-secondary)] truncate">{{ r.spec }}</div>
                  <div class="text-[12px]" [style.color]="heatColor(r)" data-testid="machine-heat">{{ r.heatText }}</div>
                  <div class="flex flex-wrap gap-1 mt-1">
                    @if (r.w.status === 'online') {
                      <button type="button" class="sy-btn sy-btn-sm" [disabled]="control.isBusy(r.w.id)" (click)="control.setDrained(r.w.id, !r.w.drained)" [syTooltip]="r.w.drained ? 'Accept new work again' : 'Finish what runs, accept no new work'">
                        {{ r.w.drained ? 'Undrain' : 'Drain' }}
                      </button>
                      @if (r.suspended && !r.running) {
                        <button type="button" class="sy-btn sy-btn-sm" [disabled]="control.isBusy(r.w.id)" (click)="control.workerBulk(r.w.id, 'resume-all')" syTooltip="Resume every suspended session on this machine">Resume</button>
                      } @else {
                        <button type="button" class="sy-btn sy-btn-sm" [disabled]="control.isBusy(r.w.id) || !r.running" (click)="control.workerBulk(r.w.id, 'suspend-all')" syTooltip="Freeze every session on this machine (nothing is lost)">Suspend</button>
                      }
                    }
                    @if (r.w.status === 'online' && r.w.wake_lock?.supported) {
                      <button
                        type="button"
                        class="sy-btn sy-btn-sm"
                        [attr.aria-pressed]="wakeOn(r.w)"
                        [attr.data-testid]="'keep-awake-' + r.w.sid"
                        [class.text-[#e6b763]]="wakeOn(r.w)"
                        (click)="setWake(r.w, !wakeOn(r.w))"
                        [syTooltip]="wakeTip(r.w)"
                      >
                        <app-icon name="coffee" [size]="11" />
                        {{ wakeOn(r.w) ? (r.w.wake_lock?.active ? 'Awake' : 'Waking…') : 'Keep awake' }}
                      </button>
                    }
                    @if (r.w.launch_state === 'failed') {
                      <button type="button" class="sy-btn sy-btn-sm" (click)="fleet.retryStart(r.w.id)" [syTooltip]="'Start syred on ' + r.w.name + ' again'">Retry start</button>
                    }
                    <button type="button" class="sy-btn sy-btn-sm" [attr.data-testid]="'machine-details-' + r.w.sid" (click)="ui.inspectedWorkerId.set(r.w.id)">Details</button>
                  </div>
                </div>
              </div>

              <div class="flex flex-col gap-2 min-w-0">
                <ul class="m-0 p-0 list-none flex flex-wrap items-start gap-2" [attr.aria-label]="'Sessions on ' + r.w.name">
                  @for (s of r.sessions; track s.key) {
                    <li class="sy-sess-li" [class.sy-sess-li-wide]="!!s.detail" [class.sy-sess-done]="s.done" [attr.data-session-card]="s.sid">
                      <button type="button" class="sy-focusable sy-sess" [class.sy-sess-left]="s.done && !!s.detail?.shells" [attr.data-session]="s.sid" [attr.data-state]="s.state" [disabled]="!s.item" (click)="open(s)">
                        <sy-spinner [state]="s.state" [machines]="[machineOf(r)]" [glyph]="true" [agent]="s.agent" [coarse]="s.coarse" [tps]="s.tps" [detail]="s.detail" />
                        <span class="min-w-0 flex-1">
                          <span class="flex items-center gap-1.5 text-[11px] text-[var(--sy-text-secondary)] min-w-0">
                            <span class="w-[7px] h-[7px] rounded-[2px] shrink-0" [style.background]="s.projectColor" aria-hidden="true"></span>
                            <span class="truncate">{{ s.project }} · {{ s.agent }}</span>
                          </span>
                          <span class="block truncate text-[12.5px] text-[var(--sy-text)]">{{ s.title }}</span>
                          <span class="block truncate text-[11.5px]" [style.color]="colors[s.state]" [syTooltip]="s.nowTitle" data-testid="fleet-now"><span class="sy-sr-only">{{ words[s.state] }}: </span>{{ s.now }}</span>
                        </span>
                      </button>
                      @if (s.detail) {
                        <div class="sy-sess-detail" data-testid="session-detail">
                          <div class="flex flex-wrap gap-1.5">
                            @if (shellChip(s); as c) {
                              <span class="sy-chip" [class.sy-chip-left]="s.done" data-testid="shell-chip">{{ c }}</span>
                            }
                            @if (subChip(s); as c) {
                              <span class="sy-chip sy-chip-sub" data-testid="subagent-chip">{{ c }}</span>
                            }
                          </div>
                          @if (s.shellList.length || s.subagents.length) {
                            <ul class="m-0 p-0 list-none grid gap-1" [attr.aria-label]="'Shells and sub-agents of ' + s.title">
                              @for (sh of rowsOf(s); track sh.pid) {
                                <li class="sy-row" data-testid="shell-row" [attr.data-pid]="sh.pid">
                                  <span class="sy-row-dot sy-row-shell" [class.sy-row-busy]="sh.busy" aria-hidden="true"></span>
                                  <span class="sy-row-kind">shell</span>
                                  <span class="sy-row-text" [syTooltip]="sh.title">{{ sh.text }}</span>
                                  <span class="sy-row-age"><span class="sy-sr-only">{{ sh.busy ? 'busy for' : '' }} </span>{{ sh.age }}</span>
                                  <span class="flex gap-1 shrink-0">
                                    <button
                                      type="button"
                                      class="sy-btn sy-btn-sm sy-row-btn"
                                      data-testid="shell-ask"
                                      [disabled]="!askOf(s).ok || busy(s.key)"
                                      [syTooltip]="askOf(s).why"
                                      [attr.aria-label]="'Ask the agent to stop ' + sh.text"
                                      (click)="ask(s, [sh])"
                                    >
                                      Ask to stop
                                    </button>
                                    <button
                                      type="button"
                                      class="sy-btn sy-btn-sm sy-row-btn sy-row-kill"
                                      data-testid="shell-kill"
                                      [disabled]="busy(s.key)"
                                      [syTooltip]="'Kill pid ' + sh.pid + ' and everything it started'"
                                      [attr.aria-label]="'Kill ' + sh.text"
                                      (click)="kill(s, [sh])"
                                    >
                                      Kill
                                    </button>
                                  </span>
                                </li>
                              }
                              @for (a of s.subagents; track a.id) {
                                <li class="sy-row" data-testid="subagent-row" [attr.data-subagent]="a.id">
                                  <span class="sy-row-dot sy-row-sub" [attr.data-dot]="a.state" aria-hidden="true"></span>
                                  <span class="sy-row-kind" [style.color]="colors[a.state]">sub-agent</span>
                                  <span class="sy-row-text" [syTooltip]="subText(a)"><span class="sy-sr-only">{{ words[a.state] }}: </span>{{ subText(a) }}</span>
                                  <span class="sy-row-age">{{ ageOf(a.since) }}</span>
                                </li>
                              }
                            </ul>
                          }
                          @if ((s.detail.shells ?? 0) > 1) {
                            <div class="flex flex-wrap items-center gap-1.5">
                              <button type="button" class="sy-btn sy-btn-sm sy-row-kill" data-testid="shell-kill-all" [disabled]="busy(s.key)" (click)="kill(s, [])">Clean up all ({{ s.detail.shells }})</button>
                              <button type="button" class="sy-btn sy-btn-sm" data-testid="shell-ask-all" [disabled]="!askOf(s).ok || busy(s.key)" [syTooltip]="askOf(s).why" (click)="ask(s, [])">Ask the agent to clean up</button>
                            </div>
                          }
                          @if (notes()[s.key]; as n) {
                            <p class="m-0 text-[11.5px]" [class.text-[#f3a5a5]]="n.error" [class.text-[var(--sy-text-secondary)]]="!n.error" role="status" data-testid="shell-note">{{ n.text }}</p>
                          }
                        </div>
                      }
                    </li>
                  }
                  @if (r.next) {
                    <li class="sy-extra sy-extra-next">next: {{ r.next }} (queued)</li>
                  }
                  @if (r.load) {
                    <li class="sy-extra" [attr.aria-label]="'Machine load: ' + r.load" syTooltip="The whole machine: agents and everything else running on it">machine · {{ r.load }}</li>
                  }
                  @for (f of slots(r.free); track $index) {
                    <li class="sy-extra sy-extra-free">free slot</li>
                  }
                  @if (spoolNote(r.w); as note) {
                    <li class="sy-extra" [class.sy-extra-alert]="r.w.spool?.full" data-testid="machine-spool">{{ note }}</li>
                  }
                  @if (r.w.reconnecting_after_sleep) {
                    <li class="sy-extra" data-testid="machine-reconnecting">reconnecting after sleep…</li>
                  } @else if (r.w.status !== 'online') {
                    <li class="sy-extra">{{ offlineText(r.w) }}</li>
                  } @else if (!r.sessions.length && !r.free && !r.load) {
                    <li class="sy-extra">nothing running</li>
                  }
                </ul>
                <div class="flex items-center gap-2.5">
                  <span class="sy-strip" role="img" [attr.aria-label]="strips()[r.w.id]?.label" data-testid="heat-strip">
                    @for (c of strips()[r.w.id]?.cells; track $index) {
                      <span [class.sy-nodata]="!c.color" [style.background]="c.color"></span>
                    }
                  </span>
                  <span class="font-mono-ide text-[10.5px] text-[var(--sy-text-muted)] w-[110px] shrink-0">heat · last {{ range() }}</span>
                </div>
              </div>
            </section>
          }
          <div class="hidden min-[901px]:grid grid-cols-[minmax(0,290px)_minmax(0,1fr)] gap-x-[18px] px-[14px]" aria-hidden="true">
            <span></span>
            <span class="flex items-center gap-2.5">
              <span class="relative block flex-1 h-4 text-[10.5px] text-[var(--sy-text-muted)] font-mono-ide">
                @for (t of ticks(); track $index) {
                  <span class="absolute whitespace-nowrap" [class.-translate-x-1/2]="!$first && !$last" [class.-translate-x-full]="$last" [style.left.%]="t.left">{{ t.label }}</span>
                }
              </span>
              <span class="w-[110px] shrink-0"></span>
            </span>
          </div>
        </div>
      }

      <section class="rounded-[var(--sy-radius-lg)] bg-[var(--sy-surface-1)] px-[14px] py-3" aria-labelledby="sy-fleet-queue" data-testid="fleet-queue">
        <h2 id="sy-fleet-queue" class="m-0 mb-2 text-[11.5px] font-semibold uppercase tracking-wider text-[var(--sy-text-secondary)]">Queue · why it's waiting</h2>
        @if (queue().length === 0) {
          <p class="m-0 text-[12.5px] text-[var(--sy-text-muted)]">Nothing is waiting for a machine.</p>
        } @else {
          <ul class="m-0 p-0 list-none flex flex-col gap-1.5">
            @for (q of queue(); track q.id) {
              <li class="flex flex-wrap items-center gap-x-3 gap-y-1 text-[12.5px]" [attr.data-queued]="q.sid">
                <span class="font-mono-ide text-[var(--sy-text-muted)]">{{ q.sid }}</span>
                <span class="text-[var(--sy-text)]">{{ q.title }}</span>
                @for (why of q.reasons; track $index) {
                  @if (!$first) {
                    <span class="text-[var(--sy-text-muted)]" aria-hidden="true">·</span>
                  }
                  <span [class.text-[var(--sy-amber)]]="why.tone === 'warn'" [class.text-[var(--sy-text-secondary)]]="why.tone === 'muted'">{{ why.text }}</span>
                }
                <span class="flex-1"></span>
                <button type="button" class="sy-btn sy-btn-sm" (click)="ui.openInCommand(q.sid)">Open</button>
              </li>
            }
          </ul>
        }
      </section>
    </div>
  `,
  styles: `
    :host {
      --sy-spinner-hole: #1c1d21;
    }
    .sy-machine {
      display: grid;
      grid-template-columns: minmax(0, 290px) minmax(0, 1fr);
      gap: 8px 18px;
      padding: 12px 14px;
      border-radius: var(--sy-radius-lg);
      background: var(--sy-surface-1);
    }
    @media (max-width: 900px) {
      .sy-machine {
        grid-template-columns: minmax(0, 1fr);
      }
    }
    .sy-machine-selected {
      box-shadow: inset 0 0 0 1px var(--sy-blue);
    }
    .sy-machine-off {
      opacity: 0.75;
    }
    .sy-sess-li {
      display: flex;
      flex-direction: column;
      flex: 1 1 230px;
      min-width: 230px;
      max-width: 320px;
      border-radius: 10px;
      background: #1c1d21;
    }
    .sy-sess-li-wide {
      flex: 1 1 380px;
      max-width: 560px;
    }
    .sy-sess-done > .sy-sess {
      opacity: 0.7;
    }
    .sy-sess-li:has(.sy-sess-left) {
      box-shadow: inset 0 0 0 1px rgba(224, 168, 76, 0.25);
    }
    .sy-sess-detail {
      display: grid;
      gap: 6px;
      padding: 0 12px 10px 50px;
    }
    .sy-chip {
      font: 11px/1.5 'JetBrains Mono', ui-monospace, monospace;
      padding: 1px 8px;
      border-radius: 10px;
      background: rgba(230, 232, 238, 0.08);
      color: #ececed;
      white-space: nowrap;
    }
    .sy-chip-sub {
      background: rgba(185, 168, 255, 0.12);
      color: #c9bcff;
    }
    .sy-chip-left {
      background: rgba(224, 168, 76, 0.14);
      color: #e9c27f;
    }
    .sy-row {
      display: flex;
      align-items: center;
      gap: 8px;
      min-width: 0;
      font-size: 12px;
    }
    .sy-row-dot {
      width: 8px;
      height: 8px;
      flex: none;
    }
    .sy-row-shell {
      border-radius: 2px;
      background: #e6e8ee;
      opacity: 0.45;
    }
    .sy-row-shell.sy-row-busy {
      opacity: 1;
      animation: sy-row-blink 0.8s ease-in-out infinite;
    }
    .sy-row-sub {
      border-radius: 50%;
    }
    .sy-row-sub[data-dot='thinking'] {
      background: #b9a8ff;
      animation: sy-row-breath 2s ease-in-out infinite;
    }
    .sy-row-sub[data-dot='working'] {
      background: #e6e8ee;
      animation: sy-row-blink 0.8s ease-in-out infinite;
    }
    .sy-row-sub[data-dot='speaking'] {
      background: #8fe0b0;
    }
    .sy-row-sub[data-dot='asking'] {
      background: #e0a84c;
    }
    .sy-row-kind {
      width: 62px;
      flex: none;
      font: 11px 'JetBrains Mono', ui-monospace, monospace;
      color: #c9cbd1;
    }
    .sy-row-text {
      flex: 1;
      min-width: 0;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      color: #c9cbd1;
    }
    .sy-row-age {
      width: 34px;
      flex: none;
      text-align: right;
      font: 11px 'JetBrains Mono', ui-monospace, monospace;
      color: #8f9199;
    }
    .sy-row-btn {
      padding: 1px 7px;
      font-size: 11px;
    }
    .sy-row-kill {
      color: #f3a3a3;
    }
    @keyframes sy-row-blink {
      0%,
      100% {
        opacity: 1;
      }
      50% {
        opacity: 0.35;
      }
    }
    @keyframes sy-row-breath {
      0%,
      100% {
        opacity: 0.4;
      }
      50% {
        opacity: 1;
      }
    }
    @media (prefers-reduced-motion: reduce) {
      .sy-row-dot {
        animation: none !important;
      }
    }
    .sy-sess {
      display: flex;
      flex: 1;
      min-width: 0;
      gap: 10px;
      align-items: center;
      padding: 8px 12px 8px 10px;
      border: 0;
      border-radius: 10px;
      background: transparent;
      color: var(--sy-text);
      text-align: left;
      cursor: pointer;
    }
    .sy-sess:hover:not(:disabled) {
      background: #222328;
    }
    .sy-sess:disabled {
      cursor: default;
    }
    .sy-extra {
      display: flex;
      align-items: center;
      padding: 8px 12px;
      border-radius: 10px;
      font-size: 12px;
      color: var(--sy-text-secondary);
      background: rgba(255, 255, 255, 0.04);
    }
    .sy-extra-free {
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.12);
    }
    .sy-extra-alert {
      color: #f09595;
      background: var(--sy-red-soft);
    }
    .sy-extra-next {
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(224, 168, 76, 0.4);
      color: #e9c27f;
    }
    .sy-strip {
      display: flex;
      flex: 1;
      gap: 1px;
      height: 10px;
      border-radius: 3px;
      overflow: hidden;
    }
    .sy-strip > span {
      flex: 1;
    }
    .sy-nodata {
      background: repeating-linear-gradient(135deg, rgba(255, 255, 255, 0.18) 0 3px, transparent 3px 6px);
    }
    .sy-badge {
      display: inline-flex;
      padding: 1px 6px;
      border-radius: 4px;
      font-family: 'JetBrains Mono', ui-monospace, monospace;
      font-size: 10.5px;
      color: var(--sy-text-muted);
      background: rgba(255, 255, 255, 0.05);
      white-space: nowrap;
    }
    .sy-badge[data-badge='FULL'] {
      color: #1b1406;
      background: var(--sy-amber);
    }
    .sy-badge[data-badge='DRAINING'],
    .sy-badge[data-badge='DIRECTIVE'] {
      color: #9ec5ff;
      background: transparent;
      box-shadow: inset 0 0 0 1px var(--sy-blue);
    }
    .sy-badge[data-badge='OFFLINE'],
    .sy-badge[data-badge='RECONNECTING'],
    .sy-badge[data-badge='STARTING'] {
      background: repeating-linear-gradient(135deg, rgba(255, 255, 255, 0.08) 0 4px, transparent 4px 8px);
    }
    .sy-badge[data-badge='VERSION'] {
      color: #e9c27f;
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(224, 168, 76, 0.5);
    }
    .sy-badge[data-badge='IDLE'] {
      background: transparent;
      box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.12);
    }
    .sy-minfo {
      container: sy-minfo / inline-size;
    }
    .sy-mtitle {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 2px 8px;
      min-width: 0;
    }
    /* A name longer than the column wraps (at its hyphens, else anywhere) to two lines
       instead of being cut off; longer names are clipped after two lines. */
    .sy-mname {
      overflow-wrap: anywhere;
      display: -webkit-box;
      -webkit-box-orient: vertical;
      -webkit-line-clamp: 2;
      line-clamp: 2;
      overflow: hidden;
    }
    .sy-mbadges {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      flex-shrink: 0;
    }
    .sy-transport {
      align-items: center;
      gap: 4px;
      background: transparent;
      color: var(--sy-text-secondary);
    }
    /* Narrow: the transport shrinks to its icon (the tooltip and accessible name keep it). */
    @media (max-width: 640px) {
      .sy-transport-label {
        display: none;
      }
    }
    @container sy-minfo (max-width: 180px) {
      .sy-transport-label {
        display: none;
      }
    }
    .sy-transport[data-transport='ssh'] {
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
  readonly selectedWorker = computed(() => {
    const sid = this.selectedSid();
    return sid ? (this.fleet.workers().find((w) => w.sid === sid || w.id === sid) ?? null) : null;
  });
  readonly missingSid = computed(() => (this.selectedSid() && this.ui.fleetLoaded() && !this.selectedWorker() ? this.selectedSid() : ''));
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
      directives: this.fleet.directives(),
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
    // /fleet/<sid> scrolls that machine into view once the fleet rows have rendered.
    afterRenderEffect(() => {
      const w = this.selectedWorker();
      this.rows();
      if (!w) return;
      this.host.nativeElement.querySelector(`[data-machine="${CSS.escape(w.sid)}"]`)?.scrollIntoView({ block: 'nearest' });
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
    return isVersionMismatch(hub, w.version) ? `Version mismatch: ${w.name} runs syred ${w.version}, hub is ${hub}` : '';
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
    return offlineText(w, this.clock());
  }

  select(w: Worker): void {
    void this.router.navigate(this.selectedWorker()?.id === w.id ? ['/fleet'] : ['/fleet', w.sid]);
  }

  clearSelection(): void {
    void this.router.navigate(['/fleet']);
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

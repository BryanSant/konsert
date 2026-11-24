import { Injectable, computed, inject, signal } from '@angular/core';
import {
  FleetEvent,
  FleetPause,
  Hold,
  OverrideRequest,
  UserRequest,
  Veto,
  VetoReason,
  isTimelineEvent,
  mergeFleetEvents,
  vetoOpen,
} from '../models/control.models';
import { ApiClientService } from '../services/api-client.service';

/** Page size for the "Your actions" timeline. */
export const TIMELINE_PAGE_SIZE = 60;

/**
 * Human control state (PRD §5.8a): fleet Pause everything, active holds, open requests
 * from agents, override requests from the orchestrator, vetoes awaiting the agent's
 * acknowledgement and the "Your actions" timeline. Loaded over REST and kept current by
 * fleet.update frames of kinds "pause", "hold", "request", "override", "veto" and
 * "fleet_event".
 */
@Injectable({ providedIn: 'root' })
export class ControlStore {
  private readonly api = inject(ApiClientService);

  readonly pause = signal<FleetPause>({ paused: false });
  /** Active (unreleased) holds, newest first. */
  readonly holds = signal<Hold[]>([]);
  /** Open requests from agents, newest first. */
  readonly requests = signal<UserRequest[]>([]);
  /** Override requests waiting for the user's decision, newest first. */
  readonly overrides = signal<OverrideRequest[]>([]);
  /** Vetoes the agent has not acknowledged yet (and the user has not dismissed), newest first. */
  readonly vetoes = signal<Veto[]>([]);
  /** Timeline events (user actions and responses), newest first. */
  readonly events = signal<FleetEvent[]>([]);
  readonly eventsCursor = signal<number | undefined>(undefined);
  readonly error = signal('');
  /** Ids of actions in flight, to disable their buttons. */
  readonly busy = signal<Record<string, boolean>>({});

  readonly paused = computed(() => this.pause().paused);
  readonly attentionCount = computed(() => this.holds().length + this.requests().length);

  holdsFor(workerId: string): Hold[] {
    return this.holds().filter((h) => h.worker_id === workerId);
  }

  requestsFor(workerId: string): UserRequest[] {
    return this.requests().filter((r) => r.worker_id === workerId);
  }

  /** The active session hold on a task or chat, if any. */
  sessionHold(id: string): Hold | undefined {
    return this.holds().find(
      (h) => h.scope_kind === 'session' && (h.task_id === id || h.chat_id === id),
    );
  }

  isBusy(key: string): boolean {
    return !!this.busy()[key];
  }

  /** Loads everything; individual failures leave the previous value in place. */
  async load(): Promise<void> {
    await Promise.all([
      this.loadPause(),
      this.loadHolds(),
      this.loadRequests(),
      this.loadOverrides(),
      this.loadVetoes(),
      this.loadEvents(),
    ]);
  }

  async loadPause(): Promise<void> {
    try {
      this.pause.set(normalizePause(await this.api.getFleetPause()));
    } catch {
      // older hub or transient: keep the current state
    }
  }

  async loadHolds(): Promise<void> {
    try {
      const res = await this.api.listHolds({ active: true });
      this.holds.set(sortNewest((res.holds ?? []).filter((h) => !h.released_at)));
    } catch {
      // keep
    }
  }

  async loadRequests(): Promise<void> {
    try {
      const res = await this.api.listRequests({ status: 'open' });
      this.requests.set(sortNewest((res.requests ?? []).filter((r) => r.status === 'open')));
    } catch {
      // keep
    }
  }

  async loadOverrides(): Promise<void> {
    try {
      const res = await this.api.listOverrides({ status: 'pending' });
      this.overrides.set(sortNewest((res.overrides ?? []).filter((o) => o.status === 'pending')));
    } catch {
      // keep
    }
  }

  async loadVetoes(): Promise<void> {
    try {
      const res = await this.api.listVetoes({ open: true });
      this.vetoes.set(sortNewest((res.vetoes ?? []).filter(vetoOpen)));
    } catch {
      // keep
    }
  }

  async loadEvents(): Promise<void> {
    try {
      const res = await this.api.listFleetEvents({ notable: true, limit: TIMELINE_PAGE_SIZE });
      this.events.set(mergeFleetEvents([], (res.events ?? []).filter(isTimelineEvent)));
      this.eventsCursor.set(res.next_cursor || undefined);
    } catch {
      // keep
    }
  }

  async loadOlderEvents(): Promise<void> {
    const cursor = this.eventsCursor();
    if (!cursor) return;
    try {
      const res = await this.api.listFleetEvents({
        notable: true,
        limit: TIMELINE_PAGE_SIZE,
        before_id: cursor,
      });
      this.events.update((cur) =>
        mergeFleetEvents(cur, (res.events ?? []).filter(isTimelineEvent), 2000),
      );
      this.eventsCursor.set(res.next_cursor || undefined);
    } catch (err) {
      this.error.set(errText(err, 'Could not load older events'));
    }
  }

  // --- live updates (fleet.update) ---

  applyPause(p: FleetPause | undefined | null): void {
    if (p) this.pause.set(normalizePause(p));
  }

  applyHold(h: Hold | undefined | null): void {
    if (!h?.id) return;
    this.holds.update((list) => {
      const rest = list.filter((x) => x.id !== h.id);
      return h.released_at ? rest : sortNewest([h, ...rest]);
    });
  }

  applyRequest(r: UserRequest | undefined | null): void {
    if (!r?.id) return;
    this.requests.update((list) => {
      const rest = list.filter((x) => x.id !== r.id);
      return r.status === 'open' ? sortNewest([r, ...rest]) : rest;
    });
  }

  applyOverride(o: OverrideRequest | undefined | null): void {
    if (!o?.id) return;
    this.overrides.update((list) => {
      const rest = list.filter((x) => x.id !== o.id);
      return o.status === 'pending' ? sortNewest([o, ...rest]) : rest;
    });
  }

  applyVeto(v: Veto | undefined | null): void {
    if (!v?.id) return;
    this.vetoes.update((list) => {
      const rest = list.filter((x) => x.id !== v.id);
      return vetoOpen(v) ? sortNewest([v, ...rest]) : rest;
    });
  }

  applyEvent(ev: FleetEvent | undefined | null): void {
    if (!ev?.id || !isTimelineEvent(ev)) return;
    this.events.update((cur) => mergeFleetEvents(cur, [ev]));
  }

  // --- actions ---

  /** Runs an action with its busy flag; errors land in `error` and return false. */
  private async run<T>(
    key: string,
    fallback: string,
    fn: () => Promise<T>,
  ): Promise<T | undefined> {
    this.busy.update((b) => ({ ...b, [key]: true }));
    this.error.set('');
    try {
      return await fn();
    } catch (err) {
      this.error.set(errText(err, fallback));
      return undefined;
    } finally {
      this.busy.update((b) => {
        const next = { ...b };
        delete next[key];
        return next;
      });
    }
  }

  async pauseEverything(): Promise<boolean> {
    const res = await this.run('fleet', 'Could not pause the fleet', () => this.api.pauseFleet());
    if (res) this.pause.set(normalizePause(res));
    return !!res;
  }

  async resumeEverything(): Promise<boolean> {
    const res = await this.run('fleet', 'Could not resume the fleet', () => this.api.resumeFleet());
    if (res) this.pause.set(normalizePause(res));
    return !!res;
  }

  async releaseHold(hold: Hold, note?: string): Promise<boolean> {
    const res = await this.run(hold.id, 'Could not release the hold', () =>
      this.api.releaseHold(hold.id, note),
    );
    if (res)
      this.applyHold({ ...hold, ...res, released_at: res.released_at || new Date().toISOString() });
    return !!res;
  }

  /** Holds a session (nothing automatic touches it until released); returns the hold. */
  async holdSession(kind: 'task' | 'chat', id: string, note?: string): Promise<Hold | undefined> {
    const res = await this.run(id, 'Could not place the hold', () =>
      this.api.createHold(kind === 'task' ? { task_id: id, note } : { chat_id: id, note }),
    );
    if (res) this.applyHold(res);
    return res;
  }

  async resolveRequest(
    req: UserRequest,
    status: 'done' | 'declined',
    note?: string,
  ): Promise<boolean> {
    const res = await this.run(req.id, 'Could not update the request', () =>
      this.api.resolveRequest(req.id, status, note),
    );
    if (res) this.applyRequest({ ...req, ...res, status: res.status || status });
    return !!res;
  }

  /** Approves or denies an override request (with an optional note for the orchestrator). */
  async decideOverride(o: OverrideRequest, decision: 'approve' | 'deny', note?: string): Promise<boolean> {
    const res = await this.run(o.id, 'Could not decide the override request', () =>
      this.api.decideOverride(o.id, decision, note),
    );
    if (res) this.applyOverride({ ...o, ...res, status: res.status || (decision === 'approve' ? 'approved' : 'denied') });
    return !!res;
  }

  /** Dismisses a veto the agent never acknowledged. */
  async dismissVeto(v: Veto): Promise<boolean> {
    const res = await this.run(v.id, 'Could not dismiss the veto', () => this.api.dismissVeto(v.id));
    if (res) this.applyVeto({ ...v, ...res, dismissed_at: res.dismissed_at || new Date().toISOString() });
    return !!res;
  }

  suspendTask(id: string): Promise<boolean> {
    return this.ok(id, 'Could not suspend', () => this.api.suspendTask(id));
  }

  resumeTask(id: string): Promise<boolean> {
    return this.ok(id, 'Could not resume', () => this.api.resumeTask(id));
  }

  cancelTask(id: string): Promise<boolean> {
    return this.ok(id, 'Could not cancel', () => this.api.cancelTask(id));
  }

  vetoTask(id: string, reason: VetoReason, note?: string): Promise<boolean> {
    return this.ok(id, 'Could not veto', () => this.api.vetoTask(id, reason, note));
  }

  suspendChat(id: string): Promise<boolean> {
    return this.ok(id, 'Could not suspend', () => this.api.suspendChat(id));
  }

  resumeChat(id: string): Promise<boolean> {
    return this.ok(id, 'Could not resume', () => this.api.resumeChat(id));
  }

  stopChatTurn(id: string): Promise<boolean> {
    return this.ok(id, 'Could not stop the turn', () => this.api.stopChatTurn(id));
  }

  vetoChat(id: string, reason: VetoReason, note?: string): Promise<boolean> {
    return this.ok(id, 'Could not veto', () => this.api.vetoChat(id, reason, note));
  }

  setDrained(workerId: string, drained: boolean): Promise<boolean> {
    return this.ok(
      workerId,
      drained ? 'Could not drain the machine' : 'Could not undrain the machine',
      () => this.api.setWorkerDrained(workerId, drained),
    );
  }

  workerBulk(
    workerId: string,
    action: 'suspend-all' | 'resume-all' | 'stop-all',
  ): Promise<boolean> {
    return this.ok(workerId, 'Could not apply the action to the machine', () =>
      this.api.workerBulk(workerId, action),
    );
  }

  private async ok(key: string, fallback: string, fn: () => Promise<unknown>): Promise<boolean> {
    const res = await this.run(key, fallback, fn);
    return res !== undefined;
  }
}

function normalizePause(p: FleetPause): FleetPause {
  return {
    paused: !!p.paused,
    paused_at: p.paused_at || undefined,
    paused_by: p.paused_by || undefined,
  };
}

function sortNewest<T extends { created_at: string }>(list: T[]): T[] {
  return [...list].sort((a, b) => (b.created_at || '').localeCompare(a.created_at || ''));
}

function errText(err: unknown, fallback: string): string {
  return err instanceof Error && err.message ? err.message : fallback;
}

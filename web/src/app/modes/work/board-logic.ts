import { Project, RunDigest, Task, Ticket, TicketDigestLine, TicketEdge, TicketRollup, TicketStatus, Worker } from '../../core/models/syre.models';
import { WorkColumn, WorkItem } from '../../core/state/work-logic';

/** Board column. Backlog is hidden until the user asks for it. */
export type BoardColumn = 'backlog' | WorkColumn;

export type SwimlaneBy = 'initiative' | 'epic' | 'project';

export const BOARD_COLUMNS: BoardColumn[] = ['backlog', 'queued', 'running', 'needs_you', 'review', 'done'];

const CLOSED = new Set<TicketStatus>(['done', 'failed', 'canceled']);

export interface BoardBlocker {
  id: string;
  sid: string;
}

/** One card on the Work board: a ticket, or an ad-hoc task/chat with no ticket. */
export interface BoardCard {
  id: string;
  sid: string;
  kind: 'ticket' | 'task' | 'chat';
  title: string;
  column: BoardColumn;
  priority?: string;
  /** Ready, and at least one blocker is not done. */
  blocked: boolean;
  blockers: BoardBlocker[];
  acceptanceDone: number;
  acceptanceTotal: number;
  attempt: number;
  leaseMachine: string;
  leaseAgent: string;
  digest: string;
  projectName: string;
  laneKey: string;
  laneName: string;
  agent: string;
  machine: string;
  now: string;
  control?: WorkItem['control'];
  work?: WorkItem;
}

export interface BoardLane {
  key: string;
  name: string;
  expedite: boolean;
  rollup?: TicketRollup;
  cells: Record<BoardColumn, BoardCard[]>;
}

export interface TicketBoard {
  columns: BoardColumn[];
  lanes: BoardLane[];
}

export interface BoardInputs {
  tickets: readonly Ticket[];
  edges: readonly TicketEdge[];
  digests: Readonly<Record<string, TicketDigestLine>>;
  projects: readonly Project[];
  workers: Readonly<Record<string, { name?: string } | Worker>>;
  tasks: readonly Task[];
  /** Tasks and chats already turned into work items. Ones linked to a ticket are dropped. */
  adHoc: readonly WorkItem[];
  expedited: (id: string) => boolean;
  laneBy: SwimlaneBy;
  showBacklog: boolean;
  showClosed: boolean;
}

export function isContainerKind(kind: string | undefined): boolean {
  return kind === 'initiative' || kind === 'epic';
}

/** Default swimlane: Epic once any epic exists, otherwise Project. */
export function defaultLaneBy(tickets: readonly Ticket[]): SwimlaneBy {
  return tickets.some((t) => t.kind === 'epic') ? 'epic' : 'project';
}

export function columnForStatus(status: TicketStatus): BoardColumn {
  switch (status) {
    case 'backlog':
      return 'backlog';
    case 'ready':
      return 'queued';
    case 'running':
      return 'running';
    case 'needs_you':
      return 'needs_you';
    case 'review':
      return 'review';
    default:
      return 'done';
  }
}

export function digestLineFromJSON(ticketId: string, raw: string | undefined): TicketDigestLine | null {
  if (!raw?.trim()) return null;
  try {
    const d = JSON.parse(raw) as RunDigest;
    if (!d || typeof d !== 'object') return null;
    return {
      ticket_id: ticketId,
      summary: (d.summary || d.status || '').trim(),
      agent: d.agent,
      machine: d.machine,
      attempt: d.attempt,
    };
  } catch {
    return null;
  }
}

/** A short multi-line rendering of a run digest for the detail panel. */
export function compactDigest(d: RunDigest | null | undefined): string {
  if (!d) return '';
  const head = [d.status, d.machine, d.agent, d.model].filter(Boolean).join(' · ');
  const lines = [head, d.summary?.trim(), d.interface?.trim()].filter((l): l is string => !!l);
  if (d.verify?.cmd) lines.push(`verify ${d.verify.cmd}${d.verify.exit != null ? ` (exit ${d.verify.exit})` : ''}`);
  if (d.failure?.category) lines.push(`failed: ${d.failure.category}`);
  const tail = d.failure?.tail?.filter((l) => l.trim()).at(-1);
  if (tail) lines.push(tail.trim());
  if (d.question?.text) lines.push(d.question.text.trim());
  return lines.join('\n');
}

/** Blockers whose status is not done. A missing blocker still blocks. */
export function unfinishedBlockers(ticketId: string, edges: readonly TicketEdge[], byId: Readonly<Record<string, Ticket>>): BoardBlocker[] {
  const out: BoardBlocker[] = [];
  for (const e of edges) {
    if (e.ticket_id !== ticketId) continue;
    const b = byId[e.blocked_by_id];
    if (b?.status === 'done') continue;
    out.push({ id: e.blocked_by_id, sid: b?.sid || e.blocked_by_id });
  }
  return out;
}

/**
 * Rollup blocked count, matching the hub: needs_you, or still open with a blocker
 * that is not done.
 */
export function isRollupBlocked(t: Ticket, edges: readonly TicketEdge[], byId: Readonly<Record<string, Ticket>>): boolean {
  if (t.status === 'needs_you') return true;
  if (CLOSED.has(t.status)) return false;
  return unfinishedBlockers(t.id, edges, byId).length > 0;
}

export function rollupOf(items: readonly Ticket[], edges: readonly TicketEdge[], byId: Readonly<Record<string, Ticket>>, ticketId = ''): TicketRollup {
  let done = 0;
  let running = 0;
  let blocked = 0;
  for (const t of items) {
    if (t.status === 'done') done++;
    if (t.status === 'running') running++;
    if (isRollupBlocked(t, edges, byId)) blocked++;
  }
  return { ticket_id: ticketId, total: items.length, done, running, blocked };
}

/** Non-container descendants of a container, the same set the rollup endpoint counts. */
export function descendantWorkTickets(containerId: string, tickets: readonly Ticket[]): Ticket[] {
  const children = new Map<string, Ticket[]>();
  for (const t of tickets) {
    if (!t.parent_id) continue;
    const list = children.get(t.parent_id) ?? [];
    list.push(t);
    children.set(t.parent_id, list);
  }
  const out: Ticket[] = [];
  const stack = [...(children.get(containerId) ?? [])];
  const seen = new Set<string>();
  while (stack.length) {
    const t = stack.pop()!;
    if (seen.has(t.id)) continue;
    seen.add(t.id);
    for (const child of children.get(t.id) ?? []) stack.push(child);
    if (!isContainerKind(t.kind)) out.push(t);
  }
  return out;
}

function ancestors(ticket: Ticket, byId: Readonly<Record<string, Ticket>>): Ticket[] {
  const chain: Ticket[] = [];
  const seen = new Set<string>([ticket.id]);
  let parent = ticket.parent_id ? byId[ticket.parent_id] : undefined;
  while (parent && !seen.has(parent.id)) {
    seen.add(parent.id);
    chain.push(parent);
    parent = parent.parent_id ? byId[parent.parent_id] : undefined;
  }
  return chain;
}

function laneOf(ticket: Ticket, byId: Readonly<Record<string, Ticket>>, projects: readonly Project[], laneBy: SwimlaneBy): { key: string; name: string; rollupId?: string } {
  if (laneBy === 'project') {
    const p = projects.find((x) => x.id === ticket.project_id);
    return { key: `p:${ticket.project_id}`, name: p?.name || 'Project' };
  }
  const chain = ancestors(ticket, byId);
  if (laneBy === 'epic') {
    const epic = chain.find((x) => x.kind === 'epic');
    if (epic) return { key: `e:${epic.id}`, name: epic.title, rollupId: epic.id };
    return { key: 'no-epic', name: 'No epic' };
  }
  const initiative = chain.find((x) => x.kind === 'initiative');
  if (initiative) return { key: `i:${initiative.id}`, name: initiative.title, rollupId: initiative.id };
  return { key: 'no-initiative', name: 'No initiative' };
}

function emptyCells(): Record<BoardColumn, BoardCard[]> {
  return { backlog: [], queued: [], running: [], needs_you: [], review: [], done: [] };
}

function visible(column: BoardColumn, status: TicketStatus | undefined, showBacklog: boolean, showClosed: boolean): boolean {
  if (column === 'backlog') return showBacklog;
  if (!showClosed && (status === 'failed' || status === 'canceled')) return false;
  return true;
}

function digestFor(ticketId: string, digests: Readonly<Record<string, TicketDigestLine>>, tasks: readonly Task[]): TicketDigestLine | undefined {
  const known = digests[ticketId];
  if (known) return known;
  let best: Task | undefined;
  for (const t of tasks) {
    if (t.ticket_id !== ticketId || !t.digest_json) continue;
    const stamp = t.completed_at || t.created_at;
    const bestStamp = best ? best.completed_at || best.created_at : '';
    if (!best || stamp > bestStamp) best = t;
  }
  return best?.digest_json ? (digestLineFromJSON(ticketId, best.digest_json) ?? undefined) : undefined;
}

interface Placed {
  card: BoardCard;
  ticket?: Ticket;
  rollupId?: string;
  expedite: boolean;
}

/** Columns, swimlanes and per-lane rollups for the Work board. */
export function buildTicketBoard(inp: BoardInputs): TicketBoard {
  const byId: Record<string, Ticket> = {};
  for (const t of inp.tickets) byId[t.id] = t;
  const ticketIds = new Set(inp.tickets.map((t) => t.id));

  const placed: Placed[] = [];
  for (const t of inp.tickets) {
    if (isContainerKind(t.kind)) continue;
    const column = columnForStatus(t.status);
    const lane = laneOf(t, byId, inp.projects, inp.laneBy);
    const blockers = unfinishedBlockers(t.id, inp.edges, byId);
    const digest = digestFor(t.id, inp.digests, inp.tasks);
    const worker = t.lease?.worker_id ? inp.workers[t.lease.worker_id] : undefined;
    const leased = t.lease?.task_id ? inp.tasks.find((task) => task.id === t.lease?.task_id) : undefined;
    const machine = worker?.name || digest?.machine || '';
    const agent = leased?.agent || digest?.agent || '';
    const acceptance = t.acceptance ?? [];
    placed.push({
      ticket: t,
      rollupId: lane.rollupId,
      expedite: t.priority === 'P0' && column !== 'done',
      card: {
        id: t.id,
        sid: t.sid,
        kind: 'ticket',
        title: t.title,
        column,
        priority: t.priority,
        blocked: column === 'queued' && blockers.length > 0,
        blockers,
        acceptanceDone: acceptance.filter((a) => a.done).length,
        acceptanceTotal: acceptance.length,
        attempt: t.attempt ?? 0,
        leaseMachine: column === 'running' ? machine : '',
        leaseAgent: column === 'running' ? agent : '',
        digest: digest?.summary || '',
        projectName: inp.projects.find((p) => p.id === t.project_id)?.name || '',
        laneKey: lane.key,
        laneName: lane.name,
        agent,
        machine,
        now: digest?.summary || '',
      },
    });
  }

  for (const it of inp.adHoc) {
    if (!it.column) continue;
    if (it.task?.ticket_id && ticketIds.has(it.task.ticket_id)) continue;
    const lane =
      inp.laneBy === 'project'
        ? { key: it.projectKey, name: it.projectName }
        : { key: 'ad-hoc', name: 'Ad-hoc' };
    placed.push({
      expedite: inp.expedited(it.id) && it.column !== 'done',
      card: {
        id: it.id,
        sid: it.sid,
        kind: it.kind,
        title: it.title,
        column: it.column,
        blocked: false,
        blockers: [],
        acceptanceDone: 0,
        acceptanceTotal: 0,
        attempt: 0,
        leaseMachine: '',
        leaseAgent: '',
        digest: '',
        projectName: it.projectName,
        laneKey: lane.key,
        laneName: lane.name,
        agent: it.sessionKind === 'terminal' ? 'terminal' : it.agent,
        machine: it.machine,
        now: it.now,
        control: it.control,
        work: it,
      },
    });
  }

  const columns = BOARD_COLUMNS.filter((c) => c !== 'backlog' || inp.showBacklog);
  const rollups = new Map<string, TicketRollup>();
  const byLane = new Map<string, { name: string; rollupId?: string; tickets: Ticket[] }>();
  for (const p of placed) {
    if (!p.ticket) continue;
    let bucket = byLane.get(p.card.laneKey);
    if (!bucket) {
      bucket = { name: p.card.laneName, rollupId: p.rollupId, tickets: [] };
      byLane.set(p.card.laneKey, bucket);
    }
    bucket.tickets.push(p.ticket);
  }
  for (const [key, bucket] of byLane) {
    const items = bucket.rollupId ? descendantWorkTickets(bucket.rollupId, inp.tickets) : bucket.tickets;
    const roll = rollupOf(items, inp.edges, byId, bucket.rollupId ?? '');
    if (roll.total > 0) rollups.set(key, roll);
  }

  const expedite = emptyCells();
  const lanes = new Map<string, BoardLane>();
  const pri: Record<string, number> = { P0: 0, P1: 1, P2: 2, P3: 3 };
  const sorted = [...placed].sort((a, b) => (pri[a.card.priority || 'P2'] ?? 2) - (pri[b.card.priority || 'P2'] ?? 2) || a.card.title.localeCompare(b.card.title));
  for (const p of sorted) {
    const status = p.ticket?.status;
    if (!visible(p.card.column, status, inp.showBacklog, inp.showClosed)) continue;
    if (p.expedite) {
      expedite[p.card.column].push(p.card);
      continue;
    }
    let lane = lanes.get(p.card.laneKey);
    if (!lane) {
      lane = { key: p.card.laneKey, name: p.card.laneName, expedite: false, rollup: rollups.get(p.card.laneKey), cells: emptyCells() };
      lanes.set(p.card.laneKey, lane);
    }
    lane.cells[p.card.column].push(p.card);
  }

  const expTickets = placed.filter((p) => p.expedite && p.ticket).map((p) => p.ticket!);
  const expRoll = rollupOf(expTickets, inp.edges, byId);
  const expLane: BoardLane = {
    key: 'expedite',
    name: 'Expedite',
    expedite: true,
    rollup: expRoll.total > 0 ? expRoll : undefined,
    cells: expedite,
  };
  const rest = [...lanes.values()].sort((a, b) => a.name.localeCompare(b.name));
  return { columns, lanes: [expLane, ...rest] };
}

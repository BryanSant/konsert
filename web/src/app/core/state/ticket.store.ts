import { Injectable, inject, signal } from '@angular/core';
import { Task, Ticket, TicketComment, TicketDigestLine, TicketEdge, TicketRepoBinding } from '../models/syre.models';
import { ApiClientService } from '../services/api-client.service';
import { digestLineFromJSON } from '../../modes/work/board-logic';

/**
 * Tickets, blocked_by edges and the latest digest line for each ticket.
 * Loaded over REST and kept current by fleet.update "ticket" (and edge) frames.
 */
@Injectable({ providedIn: 'root' })
export class TicketStore {
  private readonly api = inject(ApiClientService);

  readonly tickets = signal<Ticket[]>([]);
  readonly edges = signal<TicketEdge[]>([]);
  readonly digests = signal<Record<string, TicketDigestLine>>({});
  readonly loaded = signal(false);
  /** Bumped by every local mutation so an in-flight list does not wipe a newer edit. */
  private rev = 0;

  async load(): Promise<void> {
    const rev = this.rev;
    const res = await this.api.listTickets();
    const serverTickets = res.tickets ?? [];
    const serverEdges = res.edges ?? [];
    const serverDigests: Record<string, TicketDigestLine> = {};
    for (const line of res.digests ?? []) serverDigests[line.ticket_id] = line;
    if (this.rev !== rev) {
      const localTickets = new Map(this.tickets().map((t) => [t.id, t]));
      const merged = serverTickets.map((t) => localTickets.get(t.id) ?? t);
      const seen = new Set(merged.map((t) => t.id));
      for (const t of localTickets.values()) {
        if (!seen.has(t.id)) merged.push(t);
      }
      this.tickets.set(merged);
      const edgeKey = (e: TicketEdge) => `${e.ticket_id}\0${e.blocked_by_id}`;
      const edges = new Map(serverEdges.map((e) => [edgeKey(e), e]));
      for (const e of this.edges()) edges.set(edgeKey(e), e);
      this.edges.set([...edges.values()]);
      this.digests.set({ ...serverDigests, ...this.digests() });
    } else {
      this.tickets.set(serverTickets);
      this.edges.set(serverEdges);
      this.digests.set(serverDigests);
    }
    this.loaded.set(true);
  }

  bySid(sid: string): Ticket | undefined {
    if (!sid) return undefined;
    return this.tickets().find((t) => t.sid === sid || t.id === sid);
  }

  upsert(ticket: Ticket): void {
    this.rev++;
    this.tickets.update((list) => {
      const i = list.findIndex((t) => t.id === ticket.id);
      if (i === -1) return [...list, ticket];
      const next = list.slice();
      next[i] = ticket;
      return next;
    });
  }

  remove(id: string): void {
    this.rev++;
    this.tickets.update((list) => list.filter((t) => t.id !== id && t.sid !== id));
    this.edges.update((list) => list.filter((e) => e.ticket_id !== id && e.blocked_by_id !== id));
  }

  upsertEdge(edge: TicketEdge): void {
    this.rev++;
    this.edges.update((list) => {
      if (list.some((e) => e.ticket_id === edge.ticket_id && e.blocked_by_id === edge.blocked_by_id)) return list;
      return [...list, edge];
    });
  }

  /** Drops a blocked_by edge locally. Live "ticket_edge_deleted" frames use this. */
  dropEdge(ticketId: string, blockerId: string): void {
    this.rev++;
    this.edges.update((list) => list.filter((e) => !(e.ticket_id === ticketId && e.blocked_by_id === blockerId)));
  }

  /** A task's digest is newer than the list snapshot once the run finishes. */
  applyTaskDigest(task: Task): void {
    if (!task.ticket_id || !task.digest_json) return;
    const line = digestLineFromJSON(task.ticket_id, task.digest_json);
    if (!line?.summary && !line?.agent && !line?.machine) return;
    if (!line) return;
    this.rev++;
    this.digests.update((m) => ({ ...m, [task.ticket_id!]: line }));
  }

  async create(req: Partial<Ticket> & { title: string }, opts?: { repos?: TicketRepoBinding[]; blockers?: string[] }): Promise<Ticket> {
    const ticket = await this.api.createTicket(req);
    this.upsert(ticket);
    for (const repo of opts?.repos ?? []) {
      await this.api.putTicketRepo(ticket.id, repo.repo_id, { base_branch: repo.base_branch, target_branch: repo.target_branch });
    }
    for (const blockerId of opts?.blockers ?? []) {
      const edge = await this.api.addTicketEdge(ticket.id, blockerId);
      this.upsertEdge(edge);
    }
    return ticket;
  }

  async update(id: string, patch: Partial<Ticket>): Promise<Ticket> {
    const ticket = await this.api.updateTicket(id, patch);
    this.upsert(ticket);
    return ticket;
  }

  async addEdge(ticketId: string, blockerId: string): Promise<TicketEdge> {
    const edge = await this.api.addTicketEdge(ticketId, blockerId);
    this.upsertEdge(edge);
    return edge;
  }

  async removeEdge(ticketId: string, blockerId: string): Promise<void> {
    await this.api.removeTicketEdge(ticketId, blockerId);
    this.dropEdge(ticketId, blockerId);
  }

  async requeue(id: string): Promise<Ticket> {
    const ticket = await this.api.requeueTicket(id);
    this.upsert(ticket);
    return ticket;
  }

  async cancel(id: string): Promise<Ticket> {
    const ticket = await this.api.cancelTicket(id);
    this.upsert(ticket);
    return ticket;
  }

  async comment(id: string, body: string): Promise<TicketComment> {
    return this.api.createTicketComment(id, body);
  }
}

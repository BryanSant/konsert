import { Injectable, computed, inject, signal } from '@angular/core';
import { AgentAccount, WorkerAgentAccount } from '../models/agent-chat.models';
import { accountHealth } from '../models/agent-usage';
import { ApiClientService } from '../services/api-client.service';

/**
 * Normalizes the accounts payload. Accepted shapes (the hub's exact one is settled at
 * integration):
 *   {accounts: WorkerAgentAccount[]}            flat, each with worker_id
 *   {workers: [{worker_id, accounts: AgentAccount[]}]}
 *   WorkerAgentAccount[]
 */
export function normalizeAccountsPayload(raw: unknown): Record<string, AgentAccount[]> {
  const out: Record<string, AgentAccount[]> = {};
  const push = (workerId: string, a: AgentAccount) => {
    if (!workerId || !a?.id) return;
    (out[workerId] ??= []).push(a);
  };
  if (Array.isArray(raw)) {
    for (const a of raw as WorkerAgentAccount[]) push(a.worker_id, a);
    return out;
  }
  if (!raw || typeof raw !== 'object') return out;
  const obj = raw as { accounts?: WorkerAgentAccount[]; workers?: Array<{ worker_id: string; accounts?: AgentAccount[] }> };
  for (const a of obj.accounts ?? []) push(a.worker_id, a);
  for (const w of obj.workers ?? []) {
    for (const a of w.accounts ?? []) push(w.worker_id, a);
    out[w.worker_id] ??= [];
  }
  return out;
}

/** Display order: default accounts first, then by agent and label. */
function sortAccounts(list: AgentAccount[]): AgentAccount[] {
  return [...list].sort(
    (a, b) => a.agent.localeCompare(b.agent) || Number(b.is_default) - Number(a.is_default) || a.label.localeCompare(b.label),
  );
}

export interface AccountRef {
  workerId: string;
  account: AgentAccount;
}

/** Agent accounts (subscriptions) per worker, from capabilities. */
@Injectable({ providedIn: 'root' })
export class AgentAccountsStore {
  private readonly api = inject(ApiClientService);

  readonly byWorker = signal<Record<string, AgentAccount[]>>({});
  readonly loaded = signal(false);
  readonly refreshing = signal(false);
  readonly error = signal('');

  /** Every account on every worker. */
  readonly all = computed<AccountRef[]>(() =>
    Object.entries(this.byWorker()).flatMap(([workerId, list]) => list.map((account) => ({ workerId, account }))),
  );

  /** Accounts that need the user to sign in again. */
  readonly loggedOut = computed(() => this.all().filter((r) => accountHealth(r.account) === 'logged_out'));

  accountsFor(workerId: string): AgentAccount[] {
    return this.byWorker()[workerId] ?? [];
  }

  find(workerId: string, accountId: string): AgentAccount | undefined {
    return this.accountsFor(workerId).find((a) => a.id === accountId);
  }

  async load(): Promise<void> {
    try {
      const raw = await this.api.listAgentAccounts();
      const map = normalizeAccountsPayload(raw);
      for (const k of Object.keys(map)) map[k] = sortAccounts(map[k]);
      this.byWorker.set(map);
      this.error.set('');
    } catch (err) {
      // An older hub without the endpoint: the composer explains there are no accounts.
      this.error.set(err instanceof Error ? err.message : 'Failed to load agent accounts');
    } finally {
      this.loaded.set(true);
    }
  }

  async refresh(workerId?: string, accountId?: string): Promise<void> {
    this.refreshing.set(true);
    try {
      await this.api.refreshAgentAccounts({ worker_id: workerId, account_id: accountId });
      await this.load();
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : 'Refresh failed');
    } finally {
      this.refreshing.set(false);
    }
  }

  /** fleet.update {kind: agent_accounts, worker_id, accounts}. */
  applyWorkerAccounts(workerId: string, accounts: AgentAccount[]): void {
    if (!workerId) return;
    this.byWorker.update((m) => ({ ...m, [workerId]: sortAccounts(accounts ?? []) }));
    this.loaded.set(true);
  }

  removeWorker(workerId: string): void {
    this.byWorker.update((m) => {
      const { [workerId]: _, ...rest } = m;
      return rest;
    });
  }
}

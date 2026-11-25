/** Pure selection logic for the composer pickers (driven by AgentAccount capabilities). */
import { AgentAccount, AgentApprovalMode, AgentEffort, AgentModel } from '../../../core/models/agent-chat.models';
import { accountHealth } from '../../../core/models/agent-usage';

export interface ComposerSelection {
  workerId: string;
  accountId: string;
  model: string;
  effort: string;
  approvalMode: string;
  cwd: string;
  branch: string;
}

export function emptySelection(): ComposerSelection {
  return { workerId: '', accountId: '', model: '', effort: '', approvalMode: '', cwd: '~', branch: '' };
}

/** The account's default model: is_default, then default_model, then the first. */
export function defaultModel(account: AgentAccount | null | undefined): AgentModel | null {
  const models = account?.models ?? [];
  return (
    models.find((m) => m.is_default) ??
    models.find((m) => m.id === account?.default_model) ??
    models[0] ??
    null
  );
}

export function findModel(account: AgentAccount | null | undefined, id: string): AgentModel | null {
  return (account?.models ?? []).find((m) => m.id === id) ?? null;
}

/** The effort levels the model supports ([] = no effort control: the row is hidden). */
export function effortsFor(model: AgentModel | null | undefined): AgentEffort[] {
  return model?.efforts ?? [];
}

export function defaultEffort(model: AgentModel | null | undefined): string {
  const efforts = effortsFor(model);
  if (efforts.length === 0) return '';
  return efforts.find((e) => e.id === model?.default_effort)?.id ?? efforts[Math.floor((efforts.length - 1) / 2)].id;
}

/** Is the approval mode usable on its worker (no `disabled` reason)? */
export function approvalEnabled(mode: AgentApprovalMode | null | undefined): boolean {
  return !!mode && !mode.disabled;
}

/** The account's default approval mode; never a disabled one. */
export function defaultApproval(account: AgentAccount | null | undefined): string {
  const modes = (account?.approval_modes ?? []).filter(approvalEnabled);
  return modes.find((m) => m.id === account?.default_approval_mode)?.id ?? modes[0]?.id ?? '';
}

/**
 * Makes a selection valid for `account`: keeps model / effort / approval when the account
 * supports them, otherwise falls back to that account's defaults. Used on account switch.
 */
export function normalizeSelection(sel: ComposerSelection, account: AgentAccount | null | undefined): ComposerSelection {
  if (!account) return { ...sel, accountId: '', model: '', effort: '', approvalMode: '' };
  const model = findModel(account, sel.model) ?? defaultModel(account);
  const efforts = effortsFor(model);
  const effort = efforts.some((e) => e.id === sel.effort) ? sel.effort : defaultEffort(model);
  const approvals = account.approval_modes ?? [];
  const approvalMode = approvals.some((m) => m.id === sel.approvalMode && approvalEnabled(m))
    ? sel.approvalMode
    : defaultApproval(account);
  return { ...sel, accountId: account.id, model: model?.id ?? '', effort, approvalMode };
}

/** Selecting a model: keep the effort if the new model has it, else its default. */
export function selectModel(sel: ComposerSelection, account: AgentAccount | null | undefined, modelId: string): ComposerSelection {
  const model = findModel(account, modelId);
  if (!model) return sel;
  const effort = effortsFor(model).some((e) => e.id === sel.effort) ? sel.effort : defaultEffort(model);
  return { ...sel, model: model.id, effort };
}

/** Case-insensitive search over display name, id and description. */
export function filterModels(models: AgentModel[], query: string): AgentModel[] {
  const q = query.trim().toLowerCase();
  if (!q) return models;
  const terms = q.split(/\s+/);
  return models.filter((m) => {
    const hay = `${m.display_name} ${m.id} ${m.description ?? ''}`.toLowerCase();
    return terms.every((t) => hay.includes(t));
  });
}

/** Picks the account a new chat starts with. */
export function pickDefaultAccount(
  accounts: AgentAccount[],
  prefs: { accountId?: string; agent?: string } = {},
): AgentAccount | null {
  const usable = accounts.filter((a) => a.chat_supported !== false);
  const healthy = (a: AgentAccount) => accountHealth(a) !== 'logged_out' && accountHealth(a) !== 'exhausted';
  if (prefs.accountId) {
    const a = usable.find((x) => x.id === prefs.accountId);
    if (a) return a;
  }
  if (prefs.agent) {
    const ofAgent = usable.filter((a) => a.agent === prefs.agent);
    const a = ofAgent.find((x) => x.is_default && healthy(x)) ?? ofAgent.find(healthy) ?? ofAgent[0];
    if (a) return a;
  }
  // No saved preference: prefer the most capable chat agents first (not alphabetical,
  // which picked Antigravity over Claude), then any healthy account.
  const ranked = [...usable].sort((a, b) => agentRank(a.agent) - agentRank(b.agent));
  return ranked.find((a) => a.is_default && healthy(a)) ?? ranked.find(healthy) ?? ranked[0] ?? null;
}

/** Default-choice order of agents when the user hasn't picked one yet. */
export const PREFERRED_AGENTS = ['claude', 'codex', 'grok', 'cursor', 'copilot', 'agy'];

function agentRank(agent: string | undefined): number {
  const i = PREFERRED_AGENTS.indexOf((agent ?? '').toLowerCase());
  return i < 0 ? PREFERRED_AGENTS.length : i;
}

export type ApprovalTone = 'default' | 'amber';

/** Full access (risk 4) is amber, like the reference. */
export function approvalTone(mode: AgentApprovalMode | null | undefined): ApprovalTone {
  return (mode?.risk ?? 0) >= 4 ? 'amber' : 'default';
}

/** Icon for an approval mode (from its hint, else by risk). */
export function approvalIcon(mode: AgentApprovalMode | null | undefined): string {
  if (mode?.icon) return mode.icon;
  switch (mode?.risk) {
    case 0:
      return 'eye';
    case 1:
      return 'hand';
    case 2:
      return 'edit';
    case 3:
      return 'shield-check';
    case 4:
      return 'shield-alert';
    default:
      return 'shield';
  }
}

/** Trigger text "Sonnet 5.5 Med" and tooltip "Sonnet 5.5 · Medium". */
export function modelTriggerText(model: AgentModel | null | undefined, effortId: string): { label: string; tooltip: string } {
  if (!model) return { label: 'Default model', tooltip: 'The agent picks its default model' };
  const effort = effortsFor(model).find((e) => e.id === effortId);
  return {
    label: effort ? `${model.display_name} ${effort.short || effort.label}` : model.display_name,
    tooltip: effort ? `${model.display_name} · ${effort.label}` : model.display_name,
  };
}

/** "claude2" -> agent icon name. */
export function agentIcon(agent: string): string {
  return agent || 'commander';
}

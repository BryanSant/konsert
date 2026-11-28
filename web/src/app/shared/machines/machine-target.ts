/**
 * Where work may go (SYR-FJA2T): all machines, one, a list, a saved group, or every machine
 * with an operator label (now and later). The hub resolves targets with the same rules
 * (store.ResolveTarget); both check machine-target.vectors.json.
 */
export type MachineTarget =
  | { kind: 'all' }
  | { kind: 'machine'; id: string }
  | { kind: 'machines'; ids: string[] }
  | { kind: 'group'; id: string }
  | { kind: 'label'; label: string };

export const ALL_MACHINES: MachineTarget = { kind: 'all' };

/** A saved set of machines. */
export interface MachineGroup {
  id: string;
  name: string;
  worker_ids: string[];
  created_by?: string;
  created_at?: string;
}

/** A Commander chat's machines as the hub reports them. */
export interface ChatScopeView {
  chat_id: string;
  scope: MachineTarget;
  label: string;
  machine_ids: string[];
}

/** The IDs of the machines `t` names right now, in `machineIds` order. */
export function resolveTarget(
  t: { kind: string; id?: string; ids?: string[]; label?: string },
  machineIds: readonly string[],
  labels: Readonly<Record<string, readonly string[]>>,
  groups: Readonly<Record<string, readonly string[]>>,
): string[] {
  const want = (t.label ?? '').toLowerCase();
  const inTarget = (id: string): boolean => {
    switch (t.kind) {
      case '':
      case 'all':
        return true;
      case 'machine':
        return id === t.id;
      case 'machines':
        return (t.ids ?? []).includes(id);
      case 'group':
        return (groups[t.id ?? ''] ?? []).includes(id);
      case 'label':
        return (labels[id] ?? []).some((l) => l.toLowerCase() === want);
      default:
        return false;
    }
  };
  return machineIds.filter(inTarget);
}

export function isAllMachines(t: MachineTarget | null | undefined): boolean {
  return !t || t.kind === 'all';
}

export function sameTarget(a: MachineTarget, b: MachineTarget): boolean {
  if (a.kind !== b.kind) return false;
  switch (a.kind) {
    case 'all':
      return true;
    case 'machine':
    case 'group':
      return a.id === (b as typeof a).id;
    case 'label':
      return a.label.toLowerCase() === (b as typeof a).label.toLowerCase();
    case 'machines': {
      const ids = (b as typeof a).ids;
      return a.ids.length === ids.length && a.ids.every((id) => ids.includes(id));
    }
  }
}

/**
 * The chip text: "All machines", the group or label name, the machine name or "3 machines",
 * plus "N of M online" when some of them are offline.
 */
export function targetChipText(
  t: MachineTarget,
  names: Readonly<Record<string, string>>,
  groups: readonly MachineGroup[],
  resolved: readonly string[],
  online: ReadonlySet<string>,
): string {
  let base: string;
  switch (t.kind) {
    case 'all':
      base = 'All machines';
      break;
    case 'machine':
      base = names[t.id] ?? 'Unknown machine';
      break;
    case 'machines':
      base =
        t.ids.length === 1 ? (names[t.ids[0]] ?? 'Unknown machine') : `${t.ids.length} machines`;
      break;
    case 'group':
      base = groups.find((g) => g.id === t.id)?.name ?? 'Deleted group';
      break;
    case 'label':
      base = t.label;
      break;
  }
  const up = resolved.filter((id) => online.has(id)).length;
  if (resolved.length > 0 && up < resolved.length) base += ` · ${up} of ${resolved.length} online`;
  if (t.kind !== 'all' && resolved.length === 0) base += ' · no machines';
  return base;
}

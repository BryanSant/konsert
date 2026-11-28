import { describe, expect, it } from 'vitest';
import vectors from './machine-target.vectors.json';
import {
  MachineGroup,
  MachineTarget,
  resolveTarget,
  sameTarget,
  targetChipText,
} from './machine-target';

describe('resolveTarget', () => {
  it('matches the shared vectors the hub checks too', () => {
    expect(vectors.cases.length).toBeGreaterThan(0);
    for (const c of vectors.cases) {
      expect(
        resolveTarget(c.target, vectors.machines, vectors.labels, vectors.groups),
        c.name,
      ).toEqual(c.want);
    }
  });
});

describe('targetChipText', () => {
  const names = { 'm-1': 'box-1', 'm-2': 'box-2', 'm-3': 'box-3' };
  const groups: MachineGroup[] = [{ id: 'g-1', name: 'Lab', worker_ids: ['m-1', 'm-2'] }];
  const online = new Set(['m-1', 'm-3']);
  it('names the target and counts offline machines', () => {
    expect(targetChipText({ kind: 'all' }, names, groups, ['m-1', 'm-2', 'm-3'], online)).toBe(
      'All machines · 2 of 3 online',
    );
    expect(targetChipText({ kind: 'all' }, names, groups, ['m-1', 'm-3'], online)).toBe(
      'All machines',
    );
    expect(targetChipText({ kind: 'machine', id: 'm-3' }, names, groups, ['m-3'], online)).toBe(
      'box-3',
    );
    expect(
      targetChipText(
        { kind: 'machines', ids: ['m-1', 'm-3'] },
        names,
        groups,
        ['m-1', 'm-3'],
        online,
      ),
    ).toBe('2 machines');
    expect(
      targetChipText({ kind: 'group', id: 'g-1' }, names, groups, ['m-1', 'm-2'], online),
    ).toBe('Lab · 1 of 2 online');
    expect(targetChipText({ kind: 'label', label: 'GPU' }, names, groups, [], online)).toBe(
      'GPU · no machines',
    );
    expect(targetChipText({ kind: 'group', id: 'g-gone' }, names, groups, [], online)).toBe(
      'Deleted group · no machines',
    );
  });
});

describe('sameTarget', () => {
  it('compares kinds and what they name', () => {
    const t: MachineTarget = { kind: 'machines', ids: ['a', 'b'] };
    expect(sameTarget(t, { kind: 'machines', ids: ['b', 'a'] })).toBe(true);
    expect(sameTarget(t, { kind: 'machines', ids: ['a'] })).toBe(false);
    expect(sameTarget({ kind: 'label', label: 'Work' }, { kind: 'label', label: 'work' })).toBe(
      true,
    );
    expect(sameTarget({ kind: 'all' }, { kind: 'group', id: 'x' })).toBe(false);
  });
});

import { describe, expect, it } from 'vitest';
import { DeliveryPlan, Review, approveActionLabel, deliveryOutcomeLine, prStatusLine } from './review.models';

function review(over: Partial<Review>): Review {
  return { id: 'rv1', sid: 'rv-1', task_id: 't1', decision: 'approved', expedite: false, created_at: '', updated_at: '', ...over };
}

function plan(over: Partial<DeliveryPlan>): DeliveryPlan {
  return {
    mode: 'direct',
    merge_style: 'squash',
    has_remote: false,
    default_branch: 'main',
    can_push: true,
    has_forge: false,
    approve_label: '',
    ...over,
  };
}

describe('approveActionLabel', () => {
  it('uses the server-computed approve_label when present', () => {
    expect(approveActionLabel(plan({ approve_label: 'Approve → merge PR #12 (squash) when checks pass' }))).toBe(
      'Approve → merge PR #12 (squash) when checks pass',
    );
  });

  it('formats fallback labels for every delivery mode', () => {
    expect(approveActionLabel(null, { mode: 'direct', mergeStyle: 'squash', hasRemote: true, defaultBranch: 'main' })).toBe(
      'Approve → squash-merge into main and push',
    );
    expect(approveActionLabel(null, { mode: 'direct', mergeStyle: 'merge', hasRemote: false, defaultBranch: 'main' })).toBe(
      'Approve → merge into main (local only)',
    );
    expect(approveActionLabel(null, { mode: 'pr_merge', mergeStyle: 'squash', hasRemote: true, review: review({ pr_number: 12 }) })).toBe(
      'Approve → merge PR #12 (squash) when checks pass',
    );
    expect(approveActionLabel(null, { mode: 'pr_open', hasRemote: true, defaultBranch: 'main' })).toBe(
      'Approve → push branch and open a PR against main',
    );
    expect(approveActionLabel(null, { mode: 'keep_branch', branch: 'syre/t-1' })).toBe(
      'Approve → mark accepted and keep branch syre/t-1',
    );
  });
});

describe('deliveryOutcomeLine', () => {
  it('prefers the hub delivery_message when present', () => {
    expect(deliveryOutcomeLine(review({ delivery_status: 'delivered', delivery_message: 'Squash-merged into main and pushed.' }))).toBe(
      'Squash-merged into main and pushed.',
    );
  });

  it('formats fallback outcomes by delivery_status', () => {
    expect(deliveryOutcomeLine(review({ delivery_status: 'pr_open', pr_number: 12 }))).toBe('Delivered: PR #12 open.');
    expect(deliveryOutcomeLine(review({ delivery_status: 'auto_merge', pr_number: 12 }))).toContain('Auto-merge enabled on PR #12');
    expect(deliveryOutcomeLine(review({ delivery_status: 'manual' }))).toContain('no forge credential');
    expect(deliveryOutcomeLine(review({ delivery_status: 'conflict' }))).toContain('Merge conflict');
  });
});

describe('prStatusLine', () => {
  it('renders PR number, state, checks and auto-merge', () => {
    expect(prStatusLine({ pr_number: 12, pr_state: 'open', pr_checks: 'pending', pr_auto_merge: true })).toBe(
      'PR #12 · open · checks: pending · auto-merge',
    );
    expect(prStatusLine({ pr_number: 12, pr_state: 'merged', pr_checks: 'passed', pr_auto_merge: true })).toBe(
      'PR #12 · merged · checks: passed',
    );
    expect(prStatusLine(undefined)).toBe('');
  });
});

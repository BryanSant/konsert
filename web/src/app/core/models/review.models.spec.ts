import { describe, expect, it } from 'vitest';
import { Review, githubLine } from './review.models';

function review(over: Partial<Review>): Review {
  return { id: 'rv1', sid: 'rv-1', task_id: 't1', decision: 'approved', expedite: false, created_at: '', updated_at: '', ...over };
}

describe('githubLine', () => {
  it('says plainly that your own PR got a comment instead of a review', () => {
    expect(githubLine(review({ github_status: 'commented' }))).toBe(
      "GitHub doesn't let you approve your own pull request — added a comment instead.",
    );
    expect(githubLine(review({ decision: 'changes_requested', github_status: 'commented' }))).toContain(
      'request changes on your own pull request',
    );
    expect(githubLine(review({ github_status: 'commented', github_message: 'From the hub.' }))).toBe('From the hub.');
  });

  it('links a sent review', () => {
    expect(githubLine(review({ github_status: 'sent', github_url: 'https://github.com/acme/wc/pull/7' }))).toBe(
      'Approved on GitHub: https://github.com/acme/wc/pull/7',
    );
  });
});

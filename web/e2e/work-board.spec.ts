import fs from 'node:fs';
import { expect, Page, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * Work board on tickets: an epic and two tickets, B blocked by A, then A marked done
 * from the detail panel so B is no longer blocked.
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
}

function loadEnv(): E2EEnv {
  return JSON.parse(fs.readFileSync(E2E_STATE_FILE, 'utf8')) as E2EEnv;
}

async function api<T>(page: Page, method: string, path: string, body?: unknown): Promise<T> {
  return page.evaluate(
    async ({ method, path, body }) => {
      const res = await fetch(path, {
        method,
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-Syre-Request': '1' },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${await res.text()}`);
      return res.json();
    },
    { method, path, body },
  ) as Promise<T>;
}

async function signIn(page: Page, env: E2EEnv): Promise<void> {
  await page.goto(`${env.baseURL}/work`);
  await page.getByRole('button', { name: 'Use recovery key' }).click();
  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.locator('#token')).toBeHidden();
  await expect(page.getByTestId('work-board')).toBeVisible();
}

async function createTicket(page: Page, opts: { kind: string; title: string; parent?: string; blocker?: string; repo?: boolean }): Promise<void> {
  await page.getByTestId('new-ticket').click();
  const dialog = page.getByTestId('new-ticket-dialog');
  await expect(dialog).toBeVisible();
  await expect(dialog.getByTestId('nt-project').locator('option').first()).toBeAttached();
  await dialog.getByTestId('nt-kind').selectOption(opts.kind);
  if (opts.parent) {
    const parent = dialog.getByTestId('nt-parent');
    const option = parent.locator('option', { hasText: opts.parent });
    await expect(option).toBeAttached();
    await parent.selectOption((await option.getAttribute('value')) ?? '');
  }
  await dialog.getByTestId('nt-title').fill(opts.title);
  if (opts.repo) {
    const repo = dialog.getByTestId('nt-repos').locator('input').first();
    await expect(repo).toBeVisible();
    await repo.check();
  }
  if (opts.blocker) {
    await dialog.getByTestId('nt-blockers').locator('label', { hasText: opts.blocker }).locator('input').check();
  }
  await dialog.getByTestId('nt-submit').click();
  await expect(dialog).toBeHidden();
}

test('Work board: a ticket blocked by another stays dimmed until that blocker is done', async ({ page }) => {
  const env = loadEnv();
  await signIn(page, env);
  const stamp = Date.now().toString(36);
  const epicTitle = `Checkout ${stamp}`;
  const titleA = `Schema ${stamp}`;
  const titleB = `Store ${stamp}`;

  // Binding a repo before the ticket is ready keeps it off the decomposition
  // pulse, so the shared commander is free for the rest of the suite.
  const projects = await api<{ projects: Array<{ id: string; is_default?: boolean }> }>(page, 'GET', '/api/projects');
  const project = projects.projects.find((p) => p.is_default) ?? projects.projects[0];
  await api(page, 'POST', `/api/projects/${project.id}/repos`, {
    name: `board-${stamp}`,
    remote_url: `https://example.invalid/board-${stamp}.git`,
    default_branch: 'main',
  });

  await createTicket(page, { kind: 'epic', title: epicTitle });
  await createTicket(page, { kind: 'feature', title: titleA, parent: epicTitle, repo: true });
  await createTicket(page, { kind: 'feature', title: titleB, parent: epicTitle, blocker: titleA, repo: true });

  const board = page.getByTestId('work-board');
  const cardA = board.locator('[data-card]').filter({ hasText: titleA });
  const cardB = board.locator('[data-card]').filter({ hasText: titleB });
  await expect(cardA).toBeVisible();
  await expect(cardB).toBeVisible();
  const sidA = await cardA.getAttribute('data-card');
  expect(sidA).toBeTruthy();

  const columnOf = (card: ReturnType<typeof cardA>) => card.evaluate((el) => el.closest('[data-cell]')?.getAttribute('data-cell') ?? '');
  await expect.poll(() => columnOf(cardB)).toBe('queued');
  await expect(cardB).toHaveAttribute('data-blocked', 'true');
  await expect(cardB).toHaveClass(/sy-work-card-blocked/);
  await expect(cardB.getByTestId('blocker-chip')).toHaveText(`blocked by ${sidA}`);

  await cardA.click();
  const panel = page.locator('aside[aria-label="Review"]').getByTestId('ticket-panel');
  await expect(panel).toBeVisible();
  await expect(panel).toHaveAttribute('data-ticket', sidA!);
  await panel.getByTestId('ticket-status').selectOption('done');
  await panel.getByTestId('ticket-move').click();

  await expect.poll(() => columnOf(cardA)).toBe('done');
  await expect(cardB).not.toHaveAttribute('data-blocked');
  await expect(cardB.getByTestId('blocker-chip')).toHaveCount(0);
  await expect.poll(() => columnOf(cardB)).toBe('queued');
});

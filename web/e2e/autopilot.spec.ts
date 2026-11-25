import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { expect, Page, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * Autopilot Play/Pause (SYR-08DMH): creates a project, repo, and ready ticket with a mock
 * agent hint via REST, presses Play in the Work mode header, observes the ticket run to
 * review, and verifies Play turns off automatically (state: finished) when done.
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  workerName: string;
  workerId: string;
  workerSid: string;
  workdir: string;
}

function loadEnv(): E2EEnv {
  return JSON.parse(fs.readFileSync(E2E_STATE_FILE, 'utf8')) as E2EEnv;
}

async function signIn(page: Page, env: E2EEnv): Promise<void> {
  await page.goto(`${env.baseURL}/command`);
  await page.getByRole('button', { name: 'Use recovery key' }).click();
  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.locator('#token')).toBeHidden();
  await expect(page.locator('app-chat-view')).toBeVisible();
}

async function apiJSON<T>(page: Page, method: string, urlPath: string, body?: unknown): Promise<T> {
  return page.evaluate(
    async ({ method, urlPath, body }) => {
      const res = await fetch(urlPath, {
        method,
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-Syre-Request': '1' },
        body: body !== undefined ? JSON.stringify(body) : undefined,
      });
      if (!res.ok) throw new Error(`${method} ${urlPath} failed: ${res.status} ${await res.text()}`);
      return res.json();
    },
    { method, urlPath, body },
  ) as Promise<T>;
}

function initLocalGitRepo(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'syre-autopilot-repo-'));
  execFileSync('git', ['init', '-b', 'main'], { cwd: dir, stdio: 'ignore' });
  execFileSync('git', ['config', 'user.email', 'e2e@syre.local'], { cwd: dir, stdio: 'ignore' });
  execFileSync('git', ['config', 'user.name', 'Syre E2E'], { cwd: dir, stdio: 'ignore' });
  fs.writeFileSync(path.join(dir, 'README.md'), '# Autopilot E2E\n');
  execFileSync('git', ['add', 'README.md'], { cwd: dir, stdio: 'ignore' });
  execFileSync('git', ['commit', '-m', 'init'], { cwd: dir, stdio: 'ignore' });
  return dir;
}

test.describe('Autopilot Play/Pause (mock engine)', () => {
  test('plays a ready ticket to completion and turns Play off automatically', async ({ page, isMobile }) => {
    test.skip(isMobile, 'Work mode header toggle is desktop/tablet layout');
    const env = loadEnv();
    await signIn(page, env);

    const repoDir = initLocalGitRepo();
    try {
      const projName = `AutopilotE2E-${Date.now()}`;
      const proj = await apiJSON<{ id: string; sid: string; name: string }>(page, 'POST', '/api/projects', {
        name: projName,
      });
      const repo = await apiJSON<{ id: string }>(page, 'POST', `/api/projects/${proj.id}/repos`, {
        name: 'app',
        remote_url: repoDir,
        default_branch: 'main',
      });
      const ticket = await apiJSON<{ id: string; sid: string }>(page, 'POST', '/api/tickets', {
        project_id: proj.id,
        kind: 'feature',
        title: 'Autopilot mock job',
        body: 'Complete a short scripted mock run.',
        status: 'ready',
        hints: { agent: 'mock' },
      });
      await apiJSON(page, 'PUT', `/api/tickets/${ticket.id}/repos/${repo.id}`, {
        base_branch: 'main',
        target_branch: 'feat/autopilot-e2e',
      });

      // Open Work mode and select our project in the Autopilot header control.
      await page.getByRole('navigation', { name: 'Modes' }).locator('[data-mode="work"]').click();
      const scope = page.getByTestId('autopilot-scope');
      await expect(scope).toBeVisible();
      await expect(scope.locator(`option[value="${proj.id}"]`)).toHaveCount(1);
      await scope.selectOption(proj.id);

      const toggle = page.getByTestId('autopilot-toggle');
      const stateBadge = page.getByTestId('autopilot-state');
      await expect(stateBadge).toHaveAttribute('data-state', 'paused');
      await expect(toggle).toHaveAttribute('aria-pressed', 'false');
      await expect(page.getByTestId('autopilot-count-dispatchable')).toContainText('1 dispatchable');

      // Press Play -> ticket is dispatched and runs on the mock engine, then Play turns off when done.
      await toggle.click();

      // Ticket reaches review on the hub and a task ran for it.
      await expect
        .poll(
          async () =>
            (await apiJSON<{ ticket: { status: string } }>(page, 'GET', `/api/tickets/${ticket.id}`)).ticket.status,
          { timeout: 20_000 },
        )
        .toBe('review');

      // Live fleet update turns Play off and marks state as finished.
      await expect(stateBadge).toHaveAttribute('data-state', 'finished', { timeout: 10_000 });
      await expect(toggle).toHaveAttribute('aria-pressed', 'false');
      await expect(page.getByTestId('autopilot-count-dispatchable')).toContainText('0 dispatchable');
      await expect(page.getByTestId('autopilot-count-running')).toContainText('0 running');
      await expect(page.getByTestId('autopilot-count-needs-you')).toContainText('0 needs you');

      const finalState = await apiJSON<{ play: boolean; state: string }>(
        page,
        'GET',
        `/api/projects/${proj.id}/autopilot`,
      );
      expect(finalState.play).toBe(false);
      expect(finalState.state).toBe('finished');
    } finally {
      fs.rmSync(repoDir, { recursive: true, force: true });
    }
  });
});

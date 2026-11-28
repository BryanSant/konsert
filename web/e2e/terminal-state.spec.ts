import fs from 'node:fs';
import { expect, Page, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * A user terminal (SYR-4GBB5) is idle at its prompt and working only while a command
 * runs: the real syred's shell integration marks (or its process-table fallback) drive
 * the state shown on the machine's Fleet card.
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  workerId: string;
  workdir: string;
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

test('a terminal is idle at its prompt and working while a command runs', async ({ page }) => {
  test.setTimeout(90_000);
  const env = loadEnv();
  await page.goto(`${env.baseURL}/command`);
  await page.getByRole('button', { name: 'Use recovery key' }).click();
  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.locator('#token')).toBeHidden();

  const res = await api<{ task_id: string }>(page, 'POST', '/api/tasks', {
    worker_selector: { worker_id: env.workerId },
    agent: 'shell',
    session_kind: 'terminal',
    repo_path: env.workdir,
    prompt: '',
    mode: 'interactive',
  });
  const t = await api<{ task_sid: string }>(page, 'GET', `/api/tasks/${res.task_id}`);
  try {
    await page.goto(`${env.baseURL}/fleet`);
    const card = page.locator(`[data-session="${t.task_sid}"]`);
    await expect(card).toHaveAttribute('data-state', 'idle', { timeout: 20_000 });
    await expect(card).toContainText('at the prompt');

    await api(page, 'POST', `/api/tasks/${res.task_id}/steer`, { input_text: 'sleep 4' });
    await expect(card).toHaveAttribute('data-state', 'working', { timeout: 10_000 });
    await expect(card).toContainText('sleep 4');
    await expect(card).toHaveAttribute('data-state', 'idle', { timeout: 15_000 });
  } finally {
    await api(page, 'POST', `/api/tasks/${res.task_id}/steer`, { input_text: 'exit' }).catch(() => undefined);
  }
});

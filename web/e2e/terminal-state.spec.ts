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

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

async function openTerminal(page: Page, env: E2EEnv): Promise<{ id: string; sid: string }> {
  const res = await api<{ task_id: string }>(page, 'POST', '/api/tasks', {
    worker_selector: { worker_id: env.workerId },
    agent: 'shell',
    session_kind: 'terminal',
    repo_path: env.workdir,
    prompt: '',
    mode: 'interactive',
  });
  const t = await api<{ task_sid: string }>(page, 'GET', `/api/tasks/${res.task_id}`);
  // Input sent before the shell draws its prompt is lost: wait until it is idle there.
  await expect
    .poll(async () => (await api<{ activity?: Array<{ task_id?: string; state: string }> }>(page, 'GET', '/api/fleet')).activity?.find((a) => a.task_id === res.task_id)?.state, { timeout: 20_000 })
    .toBe('done');
  return { id: res.task_id, sid: t.task_sid };
}

async function readPid(file: string): Promise<number> {
  await expect.poll(() => (fs.existsSync(file) ? fs.readFileSync(file, 'utf8').trim() : ''), { timeout: 10_000 }).not.toBe('');
  return Number(fs.readFileSync(file, 'utf8').trim());
}

async function taskStatus(page: Page, id: string): Promise<string> {
  return (await api<{ status: string }>(page, 'GET', `/api/tasks/${id}`)).status;
}

test('closing an idle terminal ends its shell; a running command asks first', async ({ page }) => {
  test.skip(process.platform === 'win32', 'Unix process ids');
  test.setTimeout(90_000);
  const env = loadEnv();
  await page.goto(`${env.baseURL}/command`);
  await page.getByRole('button', { name: 'Use recovery key' }).click();
  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.locator('#token')).toBeHidden();

  // Idle at its prompt: closing the tab ends the shell, no question asked.
  const idle = await openTerminal(page, env);
  const shellPidFile = `${env.workdir}/.e2e-shell-${idle.sid}`;
  await api(page, 'POST', `/api/tasks/${idle.id}/steer`, { input_text: `echo $$ > ${shellPidFile}` });
  const shellPid = await readPid(shellPidFile);
  expect(alive(shellPid)).toBe(true);
  await page.goto(`${env.baseURL}/command/${idle.sid}`);
  await page.getByTestId(`close-tab-${idle.sid}`).click();
  await expect(page.getByTestId('close-terminal-command')).toHaveCount(0);
  await expect.poll(() => taskStatus(page, idle.id), { timeout: 15_000 }).toBe('canceled');
  await expect.poll(() => alive(shellPid), { timeout: 10_000 }).toBe(false);

  // A command running: closing asks, naming it; Terminate ends the command and shell.
  const busy = await openTerminal(page, env);
  const jobPidFile = `${env.workdir}/.e2e-job-${busy.sid}`;
  await api(page, 'POST', `/api/tasks/${busy.id}/steer`, { input_text: `sh -c 'echo $$ > ${jobPidFile}; exec sleep 120'` });
  const jobPid = await readPid(jobPidFile);
  await page.goto(`${env.baseURL}/command/${busy.sid}`);
  await expect.poll(async () => (await api<{ activity?: Array<{ task_id?: string; state: string }> }>(page, 'GET', '/api/fleet')).activity?.find((a) => a.task_id === busy.id)?.state, { timeout: 15_000 }).toBe('working');
  await page.getByTestId(`close-tab-${busy.sid}`).click();
  await expect(page.getByTestId('close-terminal-command')).toContainText('sleep 120');
  await page.getByTestId('close-terminal-cancel').click();
  expect(alive(jobPid)).toBe(true);
  await page.getByTestId(`close-tab-${busy.sid}`).click();
  await page.getByTestId('close-terminal-confirm').click();
  await expect.poll(() => taskStatus(page, busy.id), { timeout: 15_000 }).toBe('canceled');
  await expect.poll(() => alive(jobPid), { timeout: 10_000 }).toBe(false);
  fs.rmSync(shellPidFile, { force: true });
  fs.rmSync(jobPidFile, { force: true });
});

import { ChildProcess, spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { expect, Page, test } from '@playwright/test';
import { getFreePort, subscriptionOnlyEnv, waitFor } from './global-setup';

/**
 * First run: a fresh hub with no machines opens Add Machine on its own, with this machine
 * on top. Each test starts its own hub (empty database) so the suite's shared hub, which
 * already has a worker, is unaffected. KONSERT_INSTALL_DRY_RUN keeps the hub from
 * registering a real service: "adding this machine" runs `konsertd install --dry-run` and
 * then the daemon as a child of the hub, which dies with it.
 */

interface Hub {
  baseURL: string;
  proc: ChildProcess;
  tmpDir: string;
}

const ADMIN = 'first-run-admin-token';

async function startHub(): Promise<Hub> {
  const repoRoot = path.resolve(__dirname, '..', '..');
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'konsert-e2e-firstrun-'));
  const port = await getFreePort();
  const baseURL = `http://127.0.0.1:${port}`;
  const proc = spawn(
    path.join(repoRoot, 'bin', 'konsert'),
    ['serve', '--listen', `127.0.0.1:${port}`, '--db', path.join(tmpDir, 'konsert.db'), '--admin-token', ADMIN, '--fleet-token', 'first-run-fleet-token'],
    {
      cwd: repoRoot,
      stdio: ['ignore', 'pipe', 'pipe'],
      env: subscriptionOnlyEnv({
        KONSERT_STATE_DIR: path.join(tmpDir, 'state'),
        KONSERT_INSTALL_DRY_RUN: '1',
        KONSERT_DISABLE_AGENT_DISCOVERY: '1',
      }),
    },
  );
  await waitFor(async () => (await fetch(`${baseURL}/api/health`)).ok, 10_000, 'first-run hub /api/health');
  return { baseURL, proc, tmpDir };
}

async function stopHub(hub: Hub): Promise<void> {
  const exited = new Promise<void>((resolve) => hub.proc.once('exit', () => resolve()));
  hub.proc.kill('SIGTERM');
  await Promise.race([exited, new Promise((r) => setTimeout(r, 8000))]);
  if (hub.proc.exitCode === null) hub.proc.kill('SIGKILL');
  fs.rmSync(hub.tmpDir, { recursive: true, force: true });
}

async function login(page: Page, hub: Hub): Promise<void> {
  await page.goto(hub.baseURL, { waitUntil: 'networkidle' });
  await page.locator('#token').fill(ADMIN);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.getByRole('heading', { name: 'KONSERT' })).toBeVisible();
}

async function api<T>(page: Page, url: string): Promise<T> {
  return page.evaluate(async (u) => {
    const res = await fetch(u, { credentials: 'same-origin' });
    return (await res.json()) as T;
  }, url) as Promise<T>;
}

test.describe('First run', () => {
  let hub: Hub;

  test.beforeEach(async () => {
    hub = await startHub();
  });

  test.afterEach(async () => {
    await stopHub(hub);
  });

  test('opens Add Machine with this machine first; adding it starts konsertd and it comes online', async ({ page }) => {
    await login(page, hub);

    const dialog = page.getByRole('dialog');
    await expect(dialog.getByRole('heading', { name: 'Add your first machine' })).toBeVisible();
    const local = dialog.getByTestId('local-host');
    await expect(local.getByText(`This machine (${os.hostname()})`)).toBeVisible();
    await expect(local.getByTestId('local-host-state')).toHaveText(/Will start konsertd/);

    await local.getByTestId('add-local').click();
    await expect(local.getByText('Online')).toBeVisible({ timeout: 20_000 });

    const fleet = await api<{ workers: Array<{ name: string; status: string; launch?: string }> }>(page, '/api/fleet');
    expect(fleet.workers).toHaveLength(1);
    expect(fleet.workers[0]).toMatchObject({ status: 'online', launch: 'local' });

    // Setup is complete: the dialog does not come back on the next load.
    await page.reload({ waitUntil: 'networkidle' });
    await expect(page.getByRole('heading', { name: 'KONSERT' })).toBeVisible();
    await expect(page.getByRole('heading', { name: /Add (your first|a) machine/ })).toHaveCount(0);
  });

  test('closing it leaves a control-plane-only hub and it does not reopen', async ({ page }) => {
    await login(page, hub);
    const dialog = page.getByRole('dialog');
    await expect(dialog.getByRole('heading', { name: 'Add your first machine' })).toBeVisible();
    await dialog.getByRole('button', { name: 'Esc: close dialog' }).click();
    await expect(page.getByRole('heading', { name: 'Add your first machine' })).toHaveCount(0);

    await expect
      .poll(async () => (await api<{ needs_setup: boolean }>(page, '/api/setup')).needs_setup)
      .toBe(false);
    await page.reload({ waitUntil: 'networkidle' });
    await expect(page.getByRole('heading', { name: 'KONSERT' })).toBeVisible();
    await expect(page.getByRole('heading', { name: /Add (your first|a) machine/ })).toHaveCount(0);
    const fleet = await api<{ workers: unknown[] }>(page, '/api/fleet');
    expect(fleet.workers).toHaveLength(0);
  });
});

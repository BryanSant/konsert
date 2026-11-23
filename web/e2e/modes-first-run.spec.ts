import { ChildProcess, spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { expect, Page, test } from '@playwright/test';
import { getFreePort, subscriptionOnlyEnv, waitFor } from './global-setup';

/**
 * First run in the mode UI: a fresh hub (no machines) opens Command on the welcome, never a
 * blank screen; "Add this machine" uses the hub's auto-start (KONSERT_INSTALL_DRY_RUN keeps
 * it from registering a real service: the daemon runs as the hub's child and dies with it).
 * A phone gets the inbox with an Add a machine card. Each test has its own empty hub.
 */

interface Hub {
  baseURL: string;
  proc: ChildProcess;
  tmpDir: string;
}

const ADMIN = 'modes-first-run-admin';

async function startHub(): Promise<Hub> {
  const repoRoot = path.resolve(__dirname, '..', '..');
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'konsert-e2e-modes-firstrun-'));
  const port = await getFreePort();
  const baseURL = `http://127.0.0.1:${port}`;
  const proc = spawn(
    path.join(repoRoot, 'bin', 'konsert'),
    ['serve', '--listen', `127.0.0.1:${port}`, '--db', path.join(tmpDir, 'konsert.db'), '--admin-token', ADMIN, '--fleet-token', 'modes-first-run-fleet'],
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
  await waitFor(async () => (await fetch(`${baseURL}/api/health`)).ok, 10_000, 'modes first-run hub /api/health');
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
  await expect(page.locator('#token')).toBeHidden();
}

test.describe('Mode UI first run', () => {
  let hub: Hub;

  test.beforeEach(async () => {
    hub = await startHub();
  });

  test.afterEach(async () => {
    await stopHub(hub);
  });

  test('desktop: Command welcomes, adds this machine, then the orchestrator chat takes over', async ({ page, isMobile }) => {
    test.skip(isMobile, 'desktop first run');
    await login(page, hub);
    await expect(page).toHaveURL(/\/command$/);
    const welcome = page.getByTestId('first-run');
    await expect(welcome.getByRole('heading', { name: 'Welcome to Konsert' })).toBeVisible();
    await expect(page.getByTestId('fleet-state')).toHaveText('FLEET: no machines yet');
    // The side panels explain what will appear there; the composer waits for a machine.
    await expect(page.getByTestId('live-work-empty')).toContainText('Every agent session on your machines');
    await expect(page.getByTestId('needs-you-rail')).toContainText('Ask about this');
    await expect(page.getByTestId('first-run-composer')).toBeDisabled();
    await expect(page.getByTestId('pause-everything')).toHaveCount(0);
    // No Add Machine dialog pops up over the welcome.
    await expect(page.getByRole('dialog')).toHaveCount(0);

    await expect(page.getByTestId('first-run-local')).toHaveText(/Will start konsertd/);
    await page.getByTestId('first-run-add-local').click();
    await expect(page.getByTestId('first-run-online')).toContainText(`${os.hostname()} is online`, { timeout: 20_000 });
    await expect(page.getByTestId('first-run-composer')).toBeEnabled();
    await expect(page.getByTestId('pause-everything')).toBeVisible();

    await page.getByTestId('first-run-composer').fill('Summarize what is running');
    await page.getByTestId('first-run-composer').press('Enter');
    const chat = page.locator('app-chat-view');
    await expect(chat).toBeVisible();
    await expect(chat.getByTestId('composer-input')).toHaveValue('Summarize what is running');
    await expect(page.getByTestId('first-run')).toHaveCount(0);
  });

  test('phone: the inbox explains itself and offers Add a machine, never a blank screen', async ({ page, isMobile }) => {
    test.skip(!isMobile, 'phone first run');
    await login(page, hub);
    await expect(page).toHaveURL(/\/inbox$/);
    await expect(page.getByTestId('mobile-fleet-state')).toHaveText('FLEET: no machines');
    await expect(page.getByRole('button', { name: 'Add a machine' })).toBeVisible();
    await expect(page.getByTestId('mobile-pause')).toHaveCount(0);
    await page.getByRole('button', { name: 'Add a machine' }).click();
    await expect(page.getByRole('dialog')).toBeVisible();
  });
});

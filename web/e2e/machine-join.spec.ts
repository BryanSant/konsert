import { ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { expect, Page, test } from '@playwright/test';
import { E2E_STATE_FILE, spawnJoin } from './global-setup';

/**
 * Manual machines join with a one-time code (SYR-RK9YD): Add machine → Manual shows one
 * `syred join` command; a syred started with it appears live; Fleet → Details →
 * Revoke credential disconnects it; Settings → Security rotation lists joined machines.
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  workerName: string;
  workerSid: string;
  syredBin: string;
  repoRoot: string;
  tmpDir: string;
}

function loadEnv(): E2EEnv {
  return JSON.parse(fs.readFileSync(E2E_STATE_FILE, 'utf8')) as E2EEnv;
}

async function login(page: Page, env: E2EEnv): Promise<void> {
  await page.goto(env.baseURL, { waitUntil: 'networkidle' });
  await page.getByRole('button', { name: 'Use recovery key' }).click();
  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.locator('#token')).toBeHidden();
}

const shots = process.env['SYRE_UI_SHOTS'];
if (shots) fs.mkdirSync(shots, { recursive: true });
async function shot(page: Page, name: string): Promise<void> {
  if (shots) await page.screenshot({ path: path.join(shots, name) });
}

test.describe('Manual machine join', () => {
  const procs: ChildProcess[] = [];
  test.afterAll(() => {
    for (const p of procs) p.kill('SIGTERM');
  });

  test('Manual shows a join command; a syred started with it appears; revoke disconnects it', async ({ page }) => {
    const env = loadEnv();
    await login(page, env);
    await page.goto(`${env.baseURL}/fleet`);
    await page.getByTestId('fleet-add-machine').click();
    const modal = page.locator('app-add-machine-modal');
    await modal.getByTestId('add-machine-tab-manual').click();
    const cmd = modal.getByTestId('join-command');
    await expect(cmd).toHaveText(/^syred join http:\/\/\S+ [0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$/);
    await expect(modal.getByTestId('join-expiry')).toHaveText(/expires in 1[45] min/);
    await shot(page, 'add-machine-manual.png');

    // Run the shown command (against the suite's loopback hub URL) with --foreground.
    const code = (await cmd.textContent())!.trim().split(' ').pop()!;
    const name = 'e2e-joined';
    procs.push(
      spawnJoin(env.syredBin, env.repoRoot, env.baseURL, code, path.join(env.tmpDir, 'joined-state'), [
        '--name',
        name,
        '--enable-mock-driver',
      ]),
    );
    const joined = modal.getByTestId('joined-machine');
    await expect(joined).toContainText(name, { timeout: 15_000 });
    await expect(modal.getByTestId('joined-state')).toHaveText(/Online/, { timeout: 15_000 });
    await shot(page, 'add-machine-manual-joined.png');
    const sid = (await joined.locator('.font-mono-ide').textContent())!.trim();
    await page.keyboard.press('Escape');
    await expect(modal).toBeHidden();

    // Fleet → Details → Revoke credential (confirmed) disconnects it.
    await page.getByTestId(`machine-details-${sid}`).click();
    const details = page.locator('app-machine-detail-modal .sy-modal');
    const cred = details.getByTestId('machine-credential');
    await expect(cred).toContainText('joined with a code');
    await cred.getByTestId('revoke-credential').click();
    await shot(page, 'machine-details-revoke-confirm.png');
    await cred.getByTestId('confirm-revoke-credential').click();
    await expect(cred.getByTestId('credential-none')).toBeVisible();
    await expect(details.getByText('offline', { exact: true })).toBeVisible({ timeout: 10_000 });
    await shot(page, 'machine-details-revoked.png');
    await page.keyboard.press('Escape');

    // The machine that joined at suite start is still online.
    await expect(page.getByTestId(`machine-details-${env.workerSid}`)).toBeVisible();

    // Remove the revoked machine so later specs see only the suite's worker.
    for (const p of procs.splice(0)) p.kill('SIGTERM');
    await page.evaluate(async (ref) => {
      const res = await fetch(`/api/workers/${ref}`, { method: 'DELETE', headers: { 'X-Syre-Request': '1' } });
      if (!res.ok) throw new Error(`remove ${ref}: ${res.status}`);
    }, sid);
  });

  test('Settings → Security rotation needs confirmation and lists joined machines', async ({ page }) => {
    const env = loadEnv();
    await login(page, env);
    // Rotation would disconnect the suite's worker; check the dialog, then cancel.
    await page.goto(`${env.baseURL}/fleet`, { waitUntil: 'networkidle' });
    await page.keyboard.press('ControlOrMeta+k');
    const palette = page.getByRole('dialog', { name: 'Search and commands' });
    await palette.getByRole('combobox').fill('Settings');
    await palette.getByRole('option', { name: /^Settings/ }).first().click();
    const sec = page.getByTestId('security-machine-credentials');
    await sec.scrollIntoViewIfNeeded();
    await sec.getByTestId('rotate-machine-credentials').click();
    await expect(sec.getByText(/Disconnect every machine that joined with a code/)).toBeVisible();
    await shot(page, 'settings-security-rotate.png');
    await sec.getByRole('button', { name: 'Cancel' }).click();
    await expect(sec.getByTestId('rotate-machine-credentials')).toBeVisible();
  });
});

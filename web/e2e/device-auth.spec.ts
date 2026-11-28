import { ChildProcess, spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Browser, expect, Page, test } from '@playwright/test';
import { getFreePort, subscriptionOnlyEnv, waitFor } from './global-setup';

/**
 * Zero-copy sign-in with auth on (SYR-W0P8B, SYR-V5JZB, SYR-A29CT): the hub prints a
 * one-time sign-in link at startup and nobody types a token. Each test gets its own hub
 * with no configured credentials, so the recovery key is generated into the tokens file.
 */

interface Hub {
  baseURL: string;
  proc: ChildProcess;
  tmpDir: string;
  /** The "Sign in: …" link printed at startup. */
  signInURL: string;
  logs: () => string;
}

async function startHub(): Promise<Hub> {
  const repoRoot = path.resolve(__dirname, '..', '..');
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'syre-e2e-auth-'));
  const port = await getFreePort();
  const baseURL = `http://127.0.0.1:${port}`;
  const proc = spawn(
    path.join(repoRoot, 'bin', 'syre'),
    ['serve', '--listen', `127.0.0.1:${port}`, '--db', path.join(tmpDir, 'syre.db')],
    {
      cwd: repoRoot,
      stdio: ['ignore', 'pipe', 'pipe'],
      env: subscriptionOnlyEnv({
        SYRE_STATE_DIR: path.join(tmpDir, 'state'),
        SYRE_CONFIG_DIR: path.join(tmpDir, 'config'),
        SYRE_INSTALL_DRY_RUN: '1',
        SYRE_DISABLE_AGENT_DISCOVERY: '1',
      }),
    },
  );
  let out = '';
  proc.stdout?.on('data', (b) => (out += String(b)));
  proc.stderr?.on('data', (b) => (out += String(b)));
  let signInURL = '';
  await waitFor(
    async () => {
      const m = /Sign in: (http:\/\/\S+\/pair\/[0-9A-Z]{4}-[0-9A-Z]{4}) \(10 min/.exec(out);
      if (m) signInURL = m[1];
      return !!m && (await fetch(`${baseURL}/api/health`)).ok;
    },
    10_000,
    'hub sign-in line',
  );
  return { baseURL, proc, tmpDir, signInURL, logs: () => out };
}

async function stopHub(hub: Hub): Promise<void> {
  const exited = new Promise<void>((resolve) => hub.proc.once('exit', () => resolve()));
  hub.proc.kill('SIGTERM');
  await Promise.race([exited, new Promise((r) => setTimeout(r, 8000))]);
  if (hub.proc.exitCode === null) hub.proc.kill('SIGKILL');
  fs.rmSync(hub.tmpDir, { recursive: true, force: true });
}

function recoveryKey(hub: Hub): string {
  const raw = fs.readFileSync(path.join(hub.tmpDir, 'syre-tokens.yaml'), 'utf8');
  return /recovery_key:\s*(\S+)/.exec(raw)?.[1] ?? '';
}

/** Opens a pairing link, names the device and signs in. */
async function pairVia(page: Page, url: string, name: string): Promise<void> {
  await page.goto(url, { waitUntil: 'networkidle' });
  await expect(page.getByRole('heading', { name: 'Sign in to Syre' })).toBeVisible();
  await page.locator('#device-name').fill(name);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByTestId('open-palette')).toBeVisible();
  await expect(page).not.toHaveURL(/\/pair\//);
}

async function openDevices(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByTestId('settings-devices').click();
  await expect(page.getByRole('heading', { name: 'Devices' })).toBeVisible();
}

async function newPage(browser: Browser, ua?: string): Promise<Page> {
  const ctx = await browser.newContext({
    viewport: { width: 1440, height: 900 },
    ...(ua ? { userAgent: ua } : {}),
  });
  return ctx.newPage();
}

const PHONE_UA =
  'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1';

test.describe('Device sign-in (auth on)', () => {
  let hub: Hub;

  test.beforeEach(async ({ isMobile }) => {
    test.skip(isMobile, 'desktop flows; the paired phone is emulated by user agent');
    hub = await startHub();
  });

  test.afterEach(async () => {
    if (hub) await stopHub(hub);
  });

  test('the startup link signs a browser in; no token is printed', async ({ page }) => {
    expect(hub.logs()).not.toContain(recoveryKey(hub));
    expect(hub.logs()).not.toMatch(/shown only|admin token/i);

    // Signed out, the app explains how to sign in instead of asking for a token.
    await page.goto(hub.baseURL, { waitUntil: 'networkidle' });
    await expect(page.getByLabel('Have a code?')).toBeVisible();
    await expect(page.locator('#token')).toHaveCount(0);

    await pairVia(page, hub.signInURL, 'E2E laptop');
    await openDevices(page);
    const row = page.getByTestId('device-list').locator('li').filter({ hasText: 'E2E laptop' });
    await expect(row).toContainText('This device');
    await expect(row).toContainText(/Signed out if unused in (89|90) days/);

    // Single use: the same link no longer works in another browser.
    const other = await newPage(page.context().browser()!);
    await other.goto(hub.signInURL, { waitUntil: 'networkidle' });
    await other.getByRole('button', { name: 'Sign in' }).click();
    await expect(other.getByRole('alert')).toContainText(/invalid, expired or already used/);
    await other.context().close();
  });

  test('a local launcher link (syre open) signs in with no click; opened again it just opens the app', async ({
    page,
  }) => {
    const mint = async (): Promise<string> => {
      const res = await fetch(`${hub.baseURL}/api/pairing-codes`, {
        method: 'POST',
        headers: {
          Authorization: `Bearer ${recoveryKey(hub)}`,
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({ launcher: true }),
      });
      expect(res.status).toBe(201);
      return ((await res.json()) as { url: string }).url;
    };
    const link = await mint();
    expect(link).toMatch(/\/pair\/[0-9A-Z]{4}-[0-9A-Z]{4}#launch$/);
    await page.goto(link);
    await expect(page.getByTestId('open-palette')).toBeVisible();
    await expect(page).not.toHaveURL(/\/pair\//);

    // Launched again while signed in: no second device, just the app.
    await page.goto(await mint());
    await expect(page.getByTestId('open-palette')).toBeVisible();
    await expect(page).not.toHaveURL(/\/pair\//);
    const list = await page.evaluate(
      async () => (await (await fetch('/api/devices')).json()) as { devices: unknown[] },
    );
    expect(list.devices).toHaveLength(1);
  });

  test('add a device from a signed-in browser, then revoke it: it is signed out at once', async ({
    page,
  }) => {
    await pairVia(page, hub.signInURL, 'E2E laptop');
    await openDevices(page);
    await page.getByTestId('add-device').click();
    const panel = page.getByTestId('add-device-panel');
    await expect(panel.getByRole('img', { name: 'QR code for the sign-in link' })).toBeVisible();
    await expect(page.getByTestId('pair-countdown')).toHaveText(/^(10:00|9:5\d)$/);
    const link = (await page.getByTestId('pair-link').textContent())!.trim();
    expect(link).toMatch(/\/pair\/[0-9A-Z]{4}-[0-9A-Z]{4}$/);
    expect(link.endsWith((await page.getByTestId('pair-code-display').textContent())!.trim())).toBe(
      true,
    );

    // The "phone" opens the link (as if it scanned the QR code).
    const phone = await newPage(page.context().browser()!, PHONE_UA);
    await phone.goto(link, { waitUntil: 'networkidle' });
    await expect(phone.locator('#device-name')).toHaveValue('Safari on iPhone');
    await pairVia(phone, link, 'E2E phone');

    // The laptop sees it arrive live.
    await expect(page.getByTestId('paired-notice')).toContainText('E2E phone is signed in');
    // Icon and text sit on one line (the icon used to break the line before the text).
    expect(
      await page.getByTestId('paired-notice').evaluate((el) => {
        const icon = el.querySelector('app-icon')!.getBoundingClientRect();
        const text = el.querySelector('span')!.getBoundingClientRect();
        const mid = icon.top + icon.height / 2;
        return mid > text.top && mid < text.bottom && text.height < 2 * icon.height + 8;
      }),
    ).toBe(true);
    await page.getByRole('button', { name: 'Done' }).click();
    const phoneRow = page.getByTestId('device-list').locator('li').filter({ hasText: 'E2E phone' });
    await expect(phoneRow).toContainText('Phone');
    await expect(phoneRow).toContainText('Safari on iPhone');

    // Revoke: the phone's open session is kicked back to sign-in without a reload.
    await phoneRow.getByTestId('revoke').click();
    await phoneRow.getByTestId('confirm-revoke').click();
    await expect(phoneRow).toHaveCount(0);
    await expect(phone.getByLabel('Have a code?')).toBeVisible({ timeout: 15_000 });
    await expect(phone.getByRole('alert')).toContainText(/signed out/i);
    const status = await phone.evaluate(
      async () => (await fetch('/api/fleet', { credentials: 'same-origin' })).status,
    );
    expect(status).toBe(401);
    await phone.context().close();
  });

  test('the recovery key is a fallback on the login page', async ({ page }) => {
    await page.goto(hub.baseURL, { waitUntil: 'networkidle' });
    await page.getByRole('button', { name: 'Use recovery key' }).click();
    await page.locator('#token').fill('not-the-key');
    await page.getByRole('button', { name: 'Sign In' }).click();
    await expect(page.getByRole('alert')).toContainText(/invalid recovery key/i);
    await page.locator('#token').fill(recoveryKey(hub));
    await page.getByRole('button', { name: 'Sign In' }).click();
    await expect(page.getByTestId('open-palette')).toBeVisible();
    // It signed this browser in as a device of its own.
    await page.getByTestId('open-palette').click();
    await page.getByRole('combobox').fill('devices');
    await page.keyboard.press('Enter');
    await expect(
      page.getByTestId('device-list').locator('li').filter({ hasText: 'This device' }),
    ).toHaveCount(1);
  });

  test('syre login: a signed-in browser approves the CLI', async ({ page }) => {
    await pairVia(page, hub.signInURL, 'E2E laptop');
    const start = await (
      await fetch(`${hub.baseURL}/api/auth/device-code`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'User-Agent': 'syre/e2e' },
        body: JSON.stringify({ name: 'syre CLI on buildbox' }),
      })
    ).json();
    await page.goto(start.verify_url, { waitUntil: 'networkidle' });
    await expect(page.getByTestId('approve-name')).toHaveText('syre CLI on buildbox');
    await page.getByRole('button', { name: 'Approve' }).click();
    await expect(page.getByRole('status')).toContainText('Approved');
    const poll = await (
      await fetch(`${hub.baseURL}/api/auth/device-code/poll`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ poll_token: start.poll_token }),
      })
    ).json();
    expect(poll.status).toBe('approved');
    const fleet = await fetch(`${hub.baseURL}/api/fleet`, {
      headers: { Authorization: `Bearer ${poll.token}` },
    });
    expect(fleet.status).toBe(200);
  });
});

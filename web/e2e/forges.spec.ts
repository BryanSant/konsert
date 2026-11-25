import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { expect, Page, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * Settings → Forges (SYR-CK7S2): reach Forges via ⌘K, detect and add a self-hosted Forgejo
 * instance from a fake HTTP server, paste a token (masked, never echoed back), verify
 * "token you pasted", remove the stored token with confirmation, and verify "not connected".
 * Set SYRE_SHOTS_DIR to capture 1440px and 390px screenshots.
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
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

function startFakeForgejo(): Promise<{ url: string; host: string; close: () => Promise<void> }> {
  return new Promise((resolve, reject) => {
    const server = http.createServer((req, res) => {
      if (req.url === '/api/v1/version') {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ version: '9.0.1' }));
        return;
      }
      res.writeHead(404);
      res.end();
    });
    server.on('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const addr = server.address();
      if (!addr || typeof addr === 'string') {
        server.close(() => reject(new Error('failed to bind fake forgejo server')));
        return;
      }
      const host = `127.0.0.1:${addr.port}`;
      resolve({
        url: `http://${host}`,
        host,
        close: () => new Promise<void>((done) => server.close(() => done())),
      });
    });
  });
}

async function shot(page: Page, name: string): Promise<void> {
  const dir = process.env['SYRE_SHOTS_DIR'];
  if (!dir) return;
  fs.mkdirSync(dir, { recursive: true });
  await page.screenshot({ path: path.join(dir, name) });
}

test.describe('Settings → Forges', () => {
  test('add fake Forgejo host, paste token, remove token, and capture screenshots', async ({ page }) => {
    const env = loadEnv();
    const fakeForge = await startFakeForgejo();
    const secretToken = 'fj_e2e_secret_token_never_rendered_42';

    try {
      await page.setViewportSize({ width: 1440, height: 900 });
      await signIn(page, env);

      // Reach Forges through ⌘K command palette.
      await page.getByTestId('open-palette').click();
      const paletteInput = page.getByRole('combobox', { name: 'Search work, machines, or run a command' });
      await paletteInput.fill('Forges');
      await page.getByRole('option', { name: /Forges/ }).click();

      const section = page.getByTestId('settings-forges');
      await expect(section).toBeVisible();
      await expect(section).toContainText('open and merge PRs/MRs');

      // Detect and add the fake Forgejo server.
      await section.getByTestId('forge-add-input').fill(fakeForge.url);
      await section.getByTestId('forge-add-submit').click();

      const row = section.locator(`[data-forge-host="${fakeForge.host}"]`);
      await expect(row).toBeVisible();
      await expect(row.getByTestId('forge-kind')).toHaveText('Forgejo');
      await expect(row.getByTestId('forge-source')).toHaveText('not connected');

      // Paste token: masked input, never echoed back, source becomes "token you pasted".
      await row.getByTestId('forge-paste-btn').click();
      const tokenInput = row.getByTestId('forge-token-input');
      await expect(tokenInput).toHaveAttribute('type', 'password');
      await tokenInput.fill(secretToken);
      await row.getByTestId('forge-token-save').click();

      await expect(row.getByTestId('forge-source')).toHaveText('token you pasted');
      await expect(row.getByTestId('forge-paste-panel')).toBeHidden();
      expect(await page.content()).not.toContain(secretToken);

      // Capture 1440px and 390px screenshots with the connected forge visible.
      await section.scrollIntoViewIfNeeded();
      await shot(page, 'settings-forges-1440.png');

      await page.setViewportSize({ width: 390, height: 844 });
      await section.scrollIntoViewIfNeeded();
      await shot(page, 'settings-forges-390.png');

      // Restore desktop viewport, remove stored token with confirmation → "not connected".
      await page.setViewportSize({ width: 1440, height: 900 });
      await section.scrollIntoViewIfNeeded();
      await row.getByTestId('forge-remove-btn').click();
      await row.getByTestId('forge-remove-confirm').click();

      await expect(row.getByTestId('forge-source')).toHaveText('not connected');
      await expect(row.getByTestId('forge-remove-btn')).toBeHidden();
    } finally {
      await fakeForge.close();
    }
  });
});

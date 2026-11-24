import fs from 'node:fs';
import path from 'node:path';
import { expect, Page, WebSocketRoute, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * Fleet machine rows with a long machine name: the name gets the room, the transport
 * badge wraps under it and, when the column is narrow, shrinks to an icon whose tooltip
 * and accessible name still say the transport. Checked at 1440 and 390 px. The long
 * name arrives as a fleet.update frame for the suite's real machine. Set
 * KONSERT_SHOTS_DIR (and KONSERT_SHOT_PREFIX) to save screenshots.
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  workerId: string;
}

function loadEnv(): E2EEnv {
  return JSON.parse(fs.readFileSync(E2E_STATE_FILE, 'utf8')) as E2EEnv;
}

async function shot(page: Page, name: string): Promise<void> {
  const dir = process.env['KONSERT_SHOTS_DIR'];
  if (!dir) return;
  fs.mkdirSync(dir, { recursive: true });
  await page.screenshot({ path: path.join(dir, (process.env['KONSERT_SHOT_PREFIX'] ?? '') + name) });
}

const LONG_NAME = 'mac-studio-build-farm-rack-03';

test.describe('Fleet machine row layout', () => {
  test('a long machine name is not truncated by its badges at 1440 and 390 px', async ({ page }) => {
    const env = loadEnv();
    let client: WebSocketRoute | null = null;
    await page.routeWebSocket(/\/ws\/client/, (ws) => {
      const server = ws.connectToServer();
      ws.onMessage((m) => server.send(m));
      server.onMessage((m) => ws.send(m));
      client = ws;
    });
    await page.goto(`${env.baseURL}/fleet`);
    await page.getByRole('button', { name: 'Use recovery key' }).click();
    await page.locator('#token').fill(env.adminToken);
    await page.getByRole('button', { name: 'Sign In' }).click();
    await expect(page.locator('#token')).toBeHidden();

    const snap = await page.evaluate(async () => (await fetch('/api/fleet', { credentials: 'same-origin' })).json());
    const worker = (snap.workers as Array<Record<string, unknown>>).find((w) => w['id'] === env.workerId)!;
    const sid = worker['sid'] as string;
    const row = page.locator(`[data-machine="${sid}"]`);
    await expect(row).toBeVisible();
    (client as WebSocketRoute | null)?.send(
      JSON.stringify({ type: 'fleet.update', payload: { kind: 'worker', worker: { ...worker, name: LONG_NAME, transport: 'ssh', ssh_target: 'bryan@' + LONG_NAME } }, ts: new Date().toISOString() }),
    );
    const name = row.locator('h3');
    await expect(name).toHaveText(LONG_NAME);
    const badge = row.getByTestId('machine-transport');

    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: width > 600 ? 900 : 844 });
      await row.scrollIntoViewIfNeeded();
      await shot(page, `fleet-machine-${width}.png`);
      // The whole name shows.
      const clipped = await name.evaluate((el) => el.scrollWidth > el.clientWidth + 1 || el.scrollHeight > el.clientHeight + 1);
      expect(clipped, `name truncated at ${width}px`).toBe(false);
      // The transport is still there, named for screen readers.
      await expect(badge).toBeVisible();
      await expect(badge).toHaveAttribute('aria-label', /transport ssh/i);
    }
    // Narrow: icon only.
    await expect(badge.getByTestId('transport-label')).toBeHidden();
    await page.setViewportSize({ width: 1440, height: 900 });
  });
});

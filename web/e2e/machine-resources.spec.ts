import fs from 'node:fs';
import { expect, Page, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * Machine details → Resources (SYR-V2GGM): the live agent/other split comes from the real
 * syred's process sampler; the history charts render from GET /api/metrics (served here
 * with a synthetic hour, since real history needs minutes of flushed samples).
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  workerId: string;
  workerSid: string;
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
}

// Requests made by the PWA's service worker bypass page.route.
test.use({ serviceWorkers: 'block' });

test('machine details show the load split and history charts', async ({ page }) => {
  test.setTimeout(90_000); // the first live load sample rides on a heartbeat
  const env = loadEnv();
  const t0 = Date.now() - 60 * 60 * 1000;
  const points = (subject: string, cpu: number, mem: number) =>
    Array.from({ length: 60 }, (_, i) => ({
      subject,
      t: new Date(t0 + i * 60_000).toISOString(),
      n: 6,
      cpu_avg: cpu + (i % 10),
      cpu_max: cpu + 10,
      rss_avg_mb: mem + i,
      rss_max_mb: mem + 100,
      procs_max: 3,
    }));
  await page.route(/\/api\/metrics\?/, (route) =>
    route.fulfill({
      json: {
        range: '12h',
        series: [
          { subject: `agent:${env.workerId}`, period: 60, points: points(`agent:${env.workerId}`, 120, 2000) },
          { subject: `other:${env.workerId}`, period: 60, points: points(`other:${env.workerId}`, 40, 6000) },
        ],
      },
    }),
  );
  await signIn(page, env);
  await page.getByRole('navigation', { name: 'Modes' }).locator('[data-mode="fleet"]').click();
  await page.getByTestId(`machine-details-${env.workerSid}`).click();
  const res = page.locator('app-machine-detail-modal .sy-modal').getByTestId('machine-resources');
  await expect(res).toBeVisible();

  // Live: the real daemon's first load sample arrives with a heartbeat.
  await expect(res.getByTestId('machine-split')).toBeVisible({ timeout: 30_000 });
  await expect(res.getByTestId('machine-split')).toContainText(/Agents/);
  await expect(res.getByTestId('machine-split')).toContainText(/Other/);

  // History: two charts (CPU, memory), each with both series and direct labels.
  for (const i of [0, 1]) {
    const chart = res.getByTestId(`load-chart-${i}`);
    await expect(chart).toBeVisible();
    await expect(chart.locator('path')).toHaveCount(2);
    await expect(chart.locator('text', { hasText: 'Agents' })).toHaveCount(1);
  }
  // Hover shows one tooltip with both series.
  const svg = res.getByTestId('load-chart-0').locator('svg');
  await svg.scrollIntoViewIfNeeded();
  const box = (await svg.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await expect(res.getByTestId('load-tooltip')).toContainText(/agents/);
  await expect(res.getByTestId('load-tooltip')).toContainText(/other/);
  if (process.env['SYRE_E2E_SHOT']) await res.screenshot({ path: process.env['SYRE_E2E_SHOT'] });
  // The table view has every point.
  await res.getByText('Show as a table').click();
  await expect(res.getByTestId('load-table').locator('tbody tr')).toHaveCount(60);
});

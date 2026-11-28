import fs from 'node:fs';
import { expect, Page, WebSocketRoute, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * An observed session's tab streams its transcript (SYR-XHAHC). The e2e daemon runs with
 * agent discovery off, so the observed session is injected over the browser socket, its
 * transcript is served for GET /api/tasks/{id}/transcript, and a live push follows.
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  workerId: string;
  workerName: string;
}

function loadEnv(): E2EEnv {
  return JSON.parse(fs.readFileSync(E2E_STATE_FILE, 'utf8')) as E2EEnv;
}

test.use({ serviceWorkers: 'block' });

test('an observed session tab shows its transcript and follows it live', async ({ page }) => {
  const env = loadEnv();
  let client: WebSocketRoute | null = null;
  await page.routeWebSocket(/\/ws\/client/, (ws) => {
    const server = ws.connectToServer();
    ws.onMessage((m) => server.send(m));
    server.onMessage((m) => ws.send(m));
    client = ws;
  });
  const push = (type: string, payload: unknown) =>
    client?.send(JSON.stringify({ type, payload, ts: new Date().toISOString() }));
  const now = new Date().toISOString();
  await page.route(/\/api\/tasks\/obs-e2e-1\/transcript/, (route) =>
    route.fulfill({
      json: {
        external_id: 'sess-e2e',
        available: true,
        events: [
          {
            chat_id: '',
            seq: 1,
            kind: 'user_message',
            turn_id: 'u1',
            message_id: 'u1',
            text: 'Why does the cache test fail?',
            at: now,
          },
          {
            chat_id: '',
            seq: 2,
            kind: 'message',
            turn_id: 'u1',
            message_id: 'a1',
            text: 'Looking at the eviction policy first.',
            at: now,
          },
        ],
      },
    }),
  );

  await page.goto(`${env.baseURL}/command`);
  await page.getByRole('button', { name: 'Use recovery key' }).click();
  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.locator('#token')).toBeHidden();
  await expect.poll(() => client !== null).toBe(true);

  push('fleet.update', {
    kind: 'task',
    task: {
      id: 'obs-e2e-1',
      sid: 't-obse2e',
      external_id: 'sess-e2e',
      worker_id: env.workerId,
      agent: 'claude',
      status: 'running',
      origin: 'external',
      session_kind: 'chat',
      mode: 'interactive',
      workdir: '/tmp/obs',
      prompt: '',
      title: 'Cache investigation',
      created_at: now,
      started_at: now,
    },
  });
  const line = page.locator('[data-work-item="t-obse2e"] button').first();
  await expect(line).toBeVisible({ timeout: 10_000 });
  await line.click();

  const transcript = page.getByTestId('observed-transcript');
  await expect(transcript).toContainText('Why does the cache test fail?');
  await expect(transcript).toContainText('Looking at the eviction policy first.');
  // Live: the hub relays the daemon's new events.
  push('observed.events', {
    external_id: 'sess-e2e',
    task_id: 'obs-e2e-1',
    events: [
      {
        chat_id: '',
        seq: 3,
        kind: 'message',
        turn_id: 'u1',
        message_id: 'a2',
        text: 'Found it: the TTL is in seconds, not ms.',
        at: now,
      },
    ],
  });
  await expect(transcript).toContainText('Found it: the TTL is in seconds, not ms.');
  // Read-only: no composer in the tab.
  await expect(page.getByTestId('observed-transcript').locator('textarea')).toHaveCount(0);
});

test('the machine list shows managed and observed sessions; an observed one opens read-only', async ({
  page,
}) => {
  const env = loadEnv();
  let client: WebSocketRoute | null = null;
  await page.routeWebSocket(/\/ws\/client/, (ws) => {
    const server = ws.connectToServer();
    ws.onMessage((m) => server.send(m));
    server.onMessage((m) => ws.send(m));
    client = ws;
  });
  const push = (type: string, payload: unknown) =>
    client?.send(JSON.stringify({ type, payload, ts: new Date().toISOString() }));
  const now = new Date().toISOString();
  await page.route(/\/api\/tasks\/obs-e2e-2\/transcript/, (route) =>
    route.fulfill({
      json: {
        external_id: 'sess-e2e-2',
        available: true,
        events: [
          {
            chat_id: '',
            seq: 1,
            kind: 'user_message',
            turn_id: 'u1',
            message_id: 'u1',
            text: 'Started in a terminal outside Syre',
            at: now,
          },
        ],
      },
    }),
  );

  await page.goto(`${env.baseURL}/fleet`);
  await page.getByRole('button', { name: 'Use recovery key' }).click();
  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.locator('#token')).toBeHidden();
  await expect.poll(() => client !== null).toBe(true);

  const task = (id: string, sid: string, title: string, origin: string) => ({
    kind: 'task',
    task: {
      id,
      sid,
      title,
      origin,
      worker_id: env.workerId,
      agent: 'claude',
      status: 'running',
      session_kind: 'chat',
      mode: 'interactive',
      workdir: '/tmp/obs',
      prompt: '',
      created_at: now,
      started_at: now,
      ...(origin === 'external' ? { external_id: 'sess-e2e-2' } : {}),
    },
  });
  push('fleet.update', task('obs-e2e-2', 't-obse2e2', 'Observed refactor', 'external'));
  push('fleet.update', task('man-e2e-2', 't-mane2e2', 'Managed migration', 'direct'));

  const list = page.getByRole('list', { name: `Sessions on ${env.workerName}` });
  await expect(list.locator('[data-session="t-obse2e2"]')).toContainText('Observed refactor', {
    timeout: 10_000,
  });
  await expect(list.locator('[data-session="t-mane2e2"]')).toContainText('Managed migration');

  await list.locator('[data-session="t-obse2e2"]').click();
  const transcript = page.getByTestId('observed-transcript');
  await expect(transcript).toContainText('Started in a terminal outside Syre');
  await expect(transcript.locator('textarea')).toHaveCount(0);
});

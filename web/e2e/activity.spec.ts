import fs from 'node:fs';
import { expect, Page, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * Live-work data end to end (KON-NAYVC, KON-RJ8P7): a mock chat turn with the "activity"
 * keyword walks thinking -> working -> speaking -> done, and konsertd's rate-limited
 * session.activity reaches the browser as fleet.update kind "activity"; the fleet snapshot
 * carries the activity set and every online machine's heat. No UI is asserted: this is the
 * data the Work and Fleet modes render.
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  workerName: string;
  workerId: string;
  workdir: string;
}

interface Activity {
  chat_id?: string;
  task_id?: string;
  state: string;
  tool?: string;
  label?: string;
  summary?: string;
  tokens_per_sec?: number;
  coarse?: boolean;
  source?: string;
}

function loadEnv(): E2EEnv {
  return JSON.parse(fs.readFileSync(E2E_STATE_FILE, 'utf8')) as E2EEnv;
}

async function getJSON<T>(page: Page, path: string): Promise<T> {
  return page.evaluate(async (p) => {
    const res = await fetch(p, { credentials: 'same-origin' });
    if (!res.ok) throw new Error(`${p}: ${res.status}`);
    return res.json();
  }, path) as Promise<T>;
}

test('activity states and machine heat reach the browser', async ({ page }) => {
  const env = loadEnv();
  const frames: Activity[] = [];
  page.on('websocket', (ws) => {
    ws.on('framereceived', (f) => {
      if (typeof f.payload !== 'string' || !f.payload.includes('"activity"')) return;
      try {
        const env = JSON.parse(f.payload) as { type: string; payload?: { kind?: string; activity?: Activity } };
        if (env.type === 'fleet.update' && env.payload?.kind === 'activity' && env.payload.activity) {
          frames.push(env.payload.activity);
        }
      } catch {
        // not JSON
      }
    });
  });

  await page.goto(env.baseURL);
  // Signed out, the login page offers pairing; the recovery key is the fallback.
  await page.getByRole('button', { name: 'Use recovery key' }).click();
  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.locator('#token')).toBeHidden();
  const view = page.locator('app-chat-view');
  await expect(view).toBeVisible();

  // A new chat on the mock2 account (the composer picks the account).
  await view.getByTestId('new-chat').click();
  const chip = view.getByTestId('account-chip');
  await expect(chip).toContainText('Mock');
  await chip.click();
  const menu = view.getByRole('menu', { name: 'Agent account' });
  await menu.locator('[data-account="mock2"]').click();
  await expect(menu).toBeHidden();
  const text = 'show activity please';
  await view.getByTestId('composer-input').fill(text);
  await view.getByTestId('send-button').click();
  await expect(view.getByTestId('user-bubble').filter({ hasText: text })).toBeVisible();

  // The chat is the one whose tool call is the mock's "activity" script.
  await expect
    .poll(() => frames.find((a) => a.state === 'working' && a.label === 'Run: go test ./...')?.chat_id ?? '', { timeout: 20_000 })
    .not.toBe('');
  const chat = { id: frames.find((a) => a.state === 'working' && a.label === 'Run: go test ./...')!.chat_id! };
  const states = () => frames.filter((a) => a.chat_id === chat.id).map((a) => a.state);
  await expect.poll(() => states().includes('done'), { timeout: 20_000 }).toBe(true);
  const seen = states().filter((s, i, all) => i === 0 || all[i - 1] !== s);
  const order = ['thinking', 'working', 'speaking', 'done'];
  let k = 0;
  for (const s of seen) if (s === order[k]) k++;
  expect(k, `states ${seen.join(' > ')}`).toBe(order.length);

  const mine = frames.filter((a) => a.chat_id === chat.id);
  const working = mine.find((a) => a.state === 'working');
  // The raw label stays; the hub adds the readable now line.
  expect(working).toMatchObject({ tool: 'Bash', label: 'Run: go test ./...', summary: 'running tests', source: 'stream' });
  expect(mine.find((a) => a.state === 'speaking')?.tokens_per_sec ?? 0).toBeGreaterThan(0);
  // Rate-limited: a few frames for a ~4 s turn, not one per streamed word.
  expect(mine.length).toBeLessThan(20);

  const snap = await getJSON<{
    activity?: Activity[];
    workers: { id: string; status: string; heat?: { score: number; level: string; cause: string } }[];
  }>(page, '/api/fleet');
  expect(snap.activity?.find((a) => a.chat_id === chat.id)?.state).toBe('done');
  const w = snap.workers.find((x) => x.id === env.workerId);
  expect(w?.status).toBe('online');
  expect(w?.heat?.level).toMatch(/^(cool|warm|hot)$/);
  expect(w?.heat?.score).toBeGreaterThanOrEqual(0);
  expect(['cpu', 'mem', 'gpu', 'slots']).toContain(w?.heat?.cause);
});

import fs from 'node:fs';
import { expect, Locator, Page, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * Human control (KON-H6HJN, PRD §5.8a) against the mock engine: suspend/resume a chat
 * mid-turn, veto with a reason, drain a machine, Pause everything, release a hold.
 * Shares the hub and konsertd started by global-setup (--enable-mock-driver).
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  workerName: string;
  workerId: string;
  workerSid: string;
  workdir: string;
}

function loadEnv(): E2EEnv {
  return JSON.parse(fs.readFileSync(E2E_STATE_FILE, 'utf8')) as E2EEnv;
}

async function signIn(page: Page, env: E2EEnv): Promise<void> {
  await page.goto(env.baseURL);
  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.locator('#token')).toBeHidden();
  await expect(page.locator('app-chat-view')).toBeVisible();
}

async function getJSON<T>(page: Page, path: string): Promise<T> {
  return page.evaluate(async (p) => {
    const res = await fetch(p, { credentials: 'same-origin' });
    if (!res.ok) throw new Error(`${p}: ${res.status}`);
    return res.json();
  }, path) as Promise<T>;
}

async function postJSON<T>(page: Page, path: string, body?: unknown): Promise<T> {
  return page.evaluate(
    async ({ path, body }) => {
      const res = await fetch(path, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-Konsert-Request': '1' },
        body: JSON.stringify(body ?? {}),
      });
      if (!res.ok) throw new Error(`${path} failed: ${res.status} ${await res.text()}`);
      return res.json();
    },
    { path, body },
  ) as Promise<T>;
}

/** Opens a fresh new-chat draft with a mock account selected ("mock2": never exhausted). */
async function newChat(page: Page): Promise<Locator> {
  const chat = page.locator('app-chat-view');
  await chat.getByTestId('new-chat').click();
  const chip = chat.getByTestId('account-chip');
  await expect(chip).toContainText('Mock');
  await chip.click();
  const menu = chat.getByRole('menu', { name: 'Agent account' });
  await menu.locator('[data-account="mock2"]').click();
  await expect(menu).toBeHidden();
  return chat;
}

async function send(chat: Locator, text: string): Promise<void> {
  await chat.getByTestId('composer-input').fill(text);
  await chat.getByTestId('send-button').click();
  await expect(chat.getByTestId('user-bubble').filter({ hasText: text })).toBeVisible();
}

function statusPill(chat: Locator): Locator {
  return chat.locator('ks-status-pill:visible [data-status]').first();
}

/** Text of the last streamed assistant message (the "slow" script appends "tick N"). */
async function transcriptText(chat: Locator): Promise<string> {
  return (await chat.locator('ks-chat-transcript').innerText()) ?? '';
}

async function dispatchMockTask(
  page: Page,
  env: E2EEnv,
): Promise<{ task_id: string; task_sid: string; status: string }> {
  return postJSON(page, '/api/tasks', {
    worker_selector: { worker_id: env.workerSid },
    agent: 'mock',
    repo_path: env.workdir,
    prompt: 'WAIT_INPUT',
    mode: 'interactive',
    origin: 'orchestrator',
  });
}

async function taskStatus(page: Page, id: string): Promise<string> {
  return (await getJSON<{ status: string }>(page, `/api/tasks/${id}`)).status;
}

test.describe('Human control (mock engine)', () => {
  test.beforeEach(async ({ page }) => {
    await signIn(page, loadEnv());
  });

  test('suspends a chat mid-turn and resumes it intact', async ({ page }) => {
    const chat = await newChat(page);
    await send(chat, 'slow suspend test');
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'working');
    await expect(chat.locator('ks-chat-transcript')).toContainText('tick 2');

    await chat.getByTestId('session-suspend').click();
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'suspended');
    // Frozen: no progress while suspended.
    const frozen = await transcriptText(chat);
    await page.waitForTimeout(1200);
    expect(await transcriptText(chat)).toBe(frozen);
    // The composer refuses new turns while suspended.
    await chat.getByTestId('composer-input').fill('another turn');
    await expect(chat.getByTestId('send-button')).toBeDisabled();
    await chat.getByTestId('composer-input').fill('');

    await chat.getByTestId('session-resume').click();
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'working');
    await expect.poll(() => transcriptText(chat), { timeout: 10_000 }).not.toBe(frozen);

    await chat.getByTestId('header-stop').click();
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'cancelled');
  });

  test('vetoes a running chat with a reason the orchestrator can see', async ({ page }) => {
    const chat = await newChat(page);
    await send(chat, 'slow veto test');
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'working');

    await chat.getByTestId('session-veto').click();
    const dialog = page.getByRole('dialog', { name: 'Veto' });
    await expect(dialog).toBeVisible();
    await expect(dialog.getByTestId('veto-submit')).toBeDisabled();
    await dialog.getByTestId('veto-reason-wrong_approach').click();
    await dialog.getByTestId('veto-note').fill('do not rewrite the parser');
    await dialog.getByTestId('veto-submit').click();
    await expect(dialog).toBeHidden();
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'cancelled');

    await expect
      .poll(async () => {
        const res = await getJSON<{
          events: Array<{ kind: string; summary: string; meta_json?: string }>;
        }>(page, '/api/events?limit=50');
        return res.events.some(
          (e) =>
            e.kind === 'veto' &&
            /wrong.approach|do not rewrite/i.test(`${e.summary} ${e.meta_json ?? ''}`),
        );
      })
      .toBe(true);

    // The veto shows in Your actions.
    await page.getByTestId('inspector-control').click();
    await expect(page.getByTestId('your-actions')).toContainText(/veto/i);
  });

  test('drains a machine so it receives no new work, then undrains it', async ({ page }) => {
    const env = loadEnv();
    const footer = page.locator('app-status-footer');
    await footer.getByTestId('footer-hosts').click();
    await footer.getByRole('button', { name: new RegExp(env.workerName, 'i') }).click();
    const modal = page.locator('app-machine-detail-modal .ks-modal');
    await expect(modal).toBeVisible();

    await modal.getByTestId('machine-drain').click();
    await expect(modal.getByTestId('machine-drained')).toBeVisible();
    await expect(modal.getByTestId('machine-drain')).toHaveText(/Undrain/);
    await expect
      .poll(
        async () =>
          (
            await getJSON<{ workers: Array<{ id: string; drained?: boolean }> }>(page, '/api/fleet')
          ).workers.find((w) => w.id === env.workerId)?.drained,
      )
      .toBe(true);

    // A task pinned to the drained machine waits in the queue.
    const t = await dispatchMockTask(page, env);
    await page.waitForTimeout(800);
    expect(await taskStatus(page, t.task_id)).toBe('queued');

    await modal.getByTestId('machine-drain').click();
    await expect(modal.getByTestId('machine-drained')).toBeHidden();
    await expect.poll(() => taskStatus(page, t.task_id), { timeout: 10_000 }).not.toBe('queued');

    await postJSON(page, `/api/tasks/${t.task_id}/cancel`);
    await page.keyboard.press('Escape');
  });

  test('pauses everything with a banner and resumes what the pause suspended', async ({ page }) => {
    const chat = await newChat(page);
    await send(chat, 'slow pause test');
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'working');

    await page.getByTestId('pause-everything').click();
    const banner = page.getByTestId('paused-banner');
    await expect(banner).toBeVisible();
    await expect(banner).toContainText('Everything is paused');
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'suspended');
    expect((await getJSON<{ paused: boolean }>(page, '/api/fleet/pause')).paused).toBe(true);

    // Survives a reload (persisted on the hub).
    await page.reload();
    await expect(page.getByTestId('paused-banner')).toBeVisible();

    await page.getByTestId('resume-everything').click();
    await expect(page.getByTestId('paused-banner')).toBeHidden();
    await expect(page.getByTestId('pause-everything')).toBeVisible();
    expect((await getJSON<{ paused: boolean }>(page, '/api/fleet/pause')).paused).toBe(false);
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'working');
    await chat.getByTestId('header-stop').click();
  });

  test('releases a hold placed by taking control of a terminal task', async ({ page }) => {
    const env = loadEnv();
    const t = await dispatchMockTask(page, env);
    const res = await postJSON<{ hold_id?: string }>(page, `/api/tasks/${t.task_id}/control`, {
      enabled: true,
    });
    expect(res.hold_id).toBeTruthy();
    const { holds } = await getJSON<{
      holds: Array<{ id: string; sid: string; task_id?: string }>;
    }>(page, '/api/holds?active=1');
    const hold = holds.find((h) => h.task_id === t.task_id);
    expect(hold).toBeTruthy();

    await page.getByTestId('inspector-control').click();
    const list = page.getByTestId('holds-list');
    await expect(list.locator(`[data-hold="${hold!.sid}"]`)).toBeVisible();
    await list.getByTestId(`hold-release-${hold!.sid}`).click();
    await expect(list.locator(`[data-hold="${hold!.sid}"]`)).toHaveCount(0);

    const after = await getJSON<{ holds: Array<{ id: string }> }>(page, '/api/holds?active=1');
    expect(after.holds.some((h) => h.id === hold!.id)).toBe(false);
    await postJSON(page, `/api/tasks/${t.task_id}/cancel`);
  });
});

import fs from 'node:fs';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { expect, Page, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * History retention (KON-CSCWP): an old chat whose detailed events were compacted still
 * shows its transcript with a note; the Keep toggle pins a chat; Settings → Storage
 * renders and Compact now runs a pass.
 *
 * Aging a chat needs its rows dated in the past, so the test backdates them directly in
 * the hub's SQLite file (WAL mode allows a second process; the hub's busy timeout covers
 * the brief lock). Set KONSERT_SHOTS_DIR to save screenshots.
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  tmpDir: string;
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

async function api<T>(page: Page, method: string, p: string, body?: unknown): Promise<T> {
  return page.evaluate(
    async ({ method, p, body }) => {
      const res = await fetch(p, {
        method,
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-Konsert-Request': '1' },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      if (!res.ok) throw new Error(`${method} ${p}: ${res.status} ${await res.text()}`);
      return res.json();
    },
    { method, p, body },
  ) as Promise<T>;
}

async function shot(page: Page, name: string): Promise<void> {
  const dir = process.env['KONSERT_SHOTS_DIR'];
  if (!dir) return;
  fs.mkdirSync(dir, { recursive: true });
  await page.screenshot({ path: path.join(dir, name) });
}

/** The hub's fixed-width timestamp layout (nanoseconds, UTC). */
function hubTime(d: Date): string {
  return d.toISOString().replace(/\.(\d{3})Z$/, '.$1000000Z');
}

interface ChatEvents {
  events: Array<{ kind: string }>;
}

async function waitForCompaction(page: Page): Promise<void> {
  await expect
    .poll(async () => (await api<{ compaction: { running: boolean; finished_at?: string } }>(page, 'GET', '/api/storage/compact')).compaction, {
      timeout: 20_000,
    })
    .toMatchObject({ running: false, finished_at: expect.any(String) });
}

test.describe('History retention', () => {
  test.beforeEach(async ({ page }) => {
    await signIn(page, loadEnv());
  });

  test('an old compacted chat shows its transcript and the compacted note; Keep pins it', async ({ page }) => {
    const env = loadEnv();
    const chat = page.locator('app-chat-view');
    await chat.getByTestId('new-chat').click();
    const chip = chat.getByTestId('account-chip');
    await expect(chip).toContainText('Mock');
    await chip.click();
    await chat.getByRole('menu', { name: 'Agent account' }).locator('[data-account="mock2"]').click();
    const prompt = 'Summarise the retention plan';
    await chat.getByTestId('composer-input').fill(prompt);
    await chat.getByTestId('send-button').click();
    await expect(chat.getByTestId('assistant-message').last()).toBeVisible();
    await expect(chat.getByTestId('turn-end').last()).toBeAttached();

    const sessions = await api<Array<{ id: string; title: string }>>(page, 'GET', '/api/chats');
    const list = Array.isArray(sessions) ? sessions : ((sessions as unknown as { sessions: Array<{ id: string }> }).sessions ?? []);
    const id = list[0].id;
    const before = await api<ChatEvents>(page, 'GET', `/api/chats/${id}/events`);
    expect(before.events.map((e) => e.kind)).toContain('tool_call');

    // Age the chat by 60 days, then compact.
    const old = hubTime(new Date(Date.now() - 60 * 24 * 3600 * 1000));
    const db = new DatabaseSync(path.join(env.tmpDir, 'konsert.db'));
    db.exec('PRAGMA busy_timeout = 5000');
    db.prepare('UPDATE chat_events SET created_at = ? WHERE chat_id = ?').run(old, id);
    db.prepare('UPDATE chat_sessions SET compact_next_at = ?, created_at = ? WHERE id = ?').run(old, old, id);
    db.close();

    await api(page, 'POST', '/api/storage/compact');
    await waitForCompaction(page);
    const after = await api<ChatEvents>(page, 'GET', `/api/chats/${id}/events`);
    const kinds = after.events.map((e) => e.kind);
    expect(kinds).not.toContain('tool_call');
    expect(kinds).toEqual(expect.arrayContaining(['user_message', 'message', 'turn_end']));

    // Reopen it: the transcript is there with the note.
    await page.reload();
    await expect(page.locator('app-chat-view')).toBeVisible();
    await chat.getByTestId('chat-switcher').click();
    await page.locator(`[data-chat="${id}"]`).click();
    await expect(chat.getByTestId('transcript-compacted')).toContainText('were compacted on');
    await expect(chat.getByTestId('user-bubble').filter({ hasText: prompt })).toBeVisible();
    await expect(chat.getByTestId('assistant-message').first()).toBeVisible();
    await shot(page, 'compacted-session.png');

    // Keep: pin and unpin.
    const keep = chat.getByTestId('toggle-keep');
    await expect(keep).toHaveAttribute('aria-pressed', 'false');
    await keep.click();
    await expect(keep).toHaveAttribute('aria-pressed', 'true');
    const pinned = await api<{ keep: boolean }>(page, 'GET', `/api/chats/${id}`);
    expect(pinned.keep).toBe(true);
    await keep.click();
    await expect(keep).toHaveAttribute('aria-pressed', 'false');
  });

  test('Settings → Storage shows size, breakdown and retention, and Compact now runs', async ({ page }) => {
    await page.getByRole('button', { name: 'Settings' }).click();
    const storage = page.getByTestId('settings-storage');
    await storage.scrollIntoViewIfNeeded();
    await expect(storage.getByTestId('storage-total')).toContainText(/B$/);
    await expect(storage.getByTestId('storage-breakdown').locator('li')).toHaveCount(7);
    // Agent-chat conversations are counted as conversations, not hidden in the event bulk.
    const conv = storage.locator('[data-kind="conversations"]');
    await expect(conv).toContainText('Conversations');
    const tipId = await conv.getAttribute('aria-describedby');
    await expect(page.locator(`[id="${tipId}"]`)).toHaveText(/Compaction keeps these/);
    await expect(storage.locator('[data-kind="session_detail"]')).toContainText('Session detail');
    const st = await api<{ stats: { breakdown: Array<{ kind: string; rows: number }> } }>(page, 'GET', '/api/storage?fresh=1');
    const convRows = st.stats.breakdown.find((k) => k.kind === 'conversations')?.rows ?? 0;
    expect(convRows).toBeGreaterThan(0);
    await expect(storage.getByTestId('storage-retention-text')).toContainText('30 days');
    const prev = (await api<{ compaction: { started_at?: string } }>(page, 'GET', '/api/storage/compact')).compaction.started_at ?? '';
    await storage.getByTestId('compact-now').click();
    // A new pass ran to the end (the panel polls it and re-measures).
    await expect
      .poll(async () => {
        const c = (await api<{ compaction: { started_at?: string; running: boolean } }>(page, 'GET', '/api/storage/compact')).compaction;
        return c.started_at !== prev && !c.running;
      }, { timeout: 20_000 })
      .toBe(true);
    await expect(storage.getByTestId('compact-now')).toBeEnabled({ timeout: 10_000 });
    await expect(storage.getByTestId('compact-status')).toContainText('Last pass (Compact now)');
    const audit = await api<{ events: Array<{ action: string }> }>(page, 'GET', '/api/audit?action=storage.compact');
    expect(audit.events.length).toBeGreaterThan(0);
    await storage.scrollIntoViewIfNeeded();
    await shot(page, 'settings-storage.png');
  });
});

import fs from 'node:fs';
import path from 'node:path';
import { expect, Page, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * The Machines dialog and a Commander chat's machines (SYR-FJA2T): labels added inline
 * persist on the hub, select mode's Select all / Select none act on the filtered rows, a
 * saved group shows up in the scope picker, the scope a chat starts with is stored on the
 * hub, and Add machine opens inside the same dialog. Set SYRE_SHOTS_DIR to save screenshots.
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  workerId: string;
  workerSid: string;
  workerName: string;
}

function loadEnv(): E2EEnv {
  return JSON.parse(fs.readFileSync(E2E_STATE_FILE, 'utf8')) as E2EEnv;
}

async function shot(page: Page, name: string): Promise<void> {
  const dir = process.env['SYRE_SHOTS_DIR'];
  if (!dir) return;
  fs.mkdirSync(dir, { recursive: true });
  await page.screenshot({ path: path.join(dir, (process.env['SYRE_SHOT_PREFIX'] ?? '') + name) });
}

async function api<T>(page: Page, method: string, urlPath: string, body?: unknown): Promise<T> {
  return page.evaluate(
    async ({ method, urlPath, body }) => {
      const res = await fetch(urlPath, {
        method,
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-Syre-Request': '1' },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      if (!res.ok) throw new Error(`${method} ${urlPath}: ${res.status} ${await res.text()}`);
      return res.json();
    },
    { method, urlPath, body },
  );
}

async function login(page: Page, env: E2EEnv, at: string): Promise<void> {
  await page.goto(`${env.baseURL}${at}`);
  await page.getByRole('button', { name: 'Use recovery key' }).click();
  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.locator('#token')).toBeHidden();
}

test.describe('Machines dialog and Commander scope', () => {
  test.skip(({ isMobile }) => isMobile, 'desktop dialog');

  test.afterEach(async ({ page }) => {
    // Leave the shared hub as it was: no labels, no groups.
    const env = loadEnv();
    const fleet = await api<{ workers: Array<{ id: string; labels?: string[] }>; machine_groups?: Array<{ id: string }> }>(page, 'GET', '/api/fleet').catch(() => null);
    if (!fleet) return;
    for (const w of fleet.workers) for (const l of w.labels ?? []) await api(page, 'DELETE', `/api/workers/${w.id}/labels/${encodeURIComponent(l)}`);
    for (const g of fleet.machine_groups ?? []) await api(page, 'DELETE', `/api/machine-groups/${g.id}`);
    void env;
  });

  test('labels, select all / none, save a group, add a machine: all in one dialog', async ({ page }) => {
    const env = loadEnv();
    await login(page, env, '/fleet');
    await page.getByTestId('fleet-open-machines').click();
    const dialog = page.getByRole('dialog');
    await expect(dialog.getByRole('heading', { name: 'Machines' })).toBeVisible();
    const row = dialog.locator(`[data-machine-row="${env.workerSid}"]`);
    await expect(row).toBeVisible();

    // A label added inline is kept on the hub.
    await row.getByTestId('add-label').click();
    await row.getByTestId('label-input').fill('Gaming hardware');
    await row.getByTestId('label-input').press('Enter');
    await expect(row.getByTestId('machine-label')).toHaveText(['Gaming hardware']);
    await expect
      .poll(async () => (await api<{ workers: Array<{ id: string; labels?: string[] }> }>(page, 'GET', '/api/fleet')).workers.find((w) => w.id === env.workerId)?.labels)
      .toEqual(['Gaming hardware']);
    await shot(page, 'machines-browse.png');

    // The filter matches labels; Select all / none act on the rows it shows.
    await dialog.getByTestId('machines-new-group').click();
    await expect(dialog.getByRole('heading', { name: 'Select machines' })).toBeVisible();
    await dialog.getByTestId('machine-filter').fill('no-such-machine');
    await expect(dialog.getByTestId('machine-table-empty')).toBeVisible();
    await expect(dialog.getByTestId('select-all')).toBeDisabled();
    await dialog.getByTestId('machine-filter').fill('gaming');
    await dialog.getByTestId('select-all').click();
    await expect(dialog.getByTestId('selected-count')).toHaveText('1 selected');
    await expect(dialog.getByTestId(`select-${env.workerSid}`)).toBeChecked();
    await dialog.getByTestId('select-none').click();
    await expect(dialog.getByTestId('selected-count')).toHaveText('0 selected');
    await dialog.getByTestId('select-all').click();

    // Save as group: it is stored on the hub and listed in browse mode.
    await dialog.getByTestId('save-as-group').click();
    await dialog.getByTestId('group-name').fill('Lab');
    await dialog.getByTestId('group-save').click();
    await expect(dialog.locator('[data-group="Lab"]')).toContainText(env.workerName);
    const groups = await api<{ groups: Array<{ name: string; worker_ids: string[] }> }>(page, 'GET', '/api/machine-groups');
    expect(groups.groups).toEqual([expect.objectContaining({ name: 'Lab', worker_ids: [env.workerId] })]);

    // Add machine opens inside the same dialog, with a way back.
    await dialog.getByTestId('machines-add').click();
    await expect(dialog.getByRole('heading', { name: /Add (your first|a) machine/ })).toBeVisible();
    await shot(page, 'machines-add.png');
    await dialog.getByTestId('machines-back').click();
    await expect(dialog.getByRole('heading', { name: 'Machines' })).toBeVisible();
    await dialog.getByRole('button', { name: 'Esc: close dialog' }).click();
    await expect(page.getByRole('dialog')).toHaveCount(0);
  });

  test('a Commander chat starts with the machines picked on its chip; the hub keeps them', async ({ page }) => {
    const env = loadEnv();
    await login(page, env, '/fleet');
    await api(page, 'POST', `/api/workers/${env.workerId}/labels`, { label: 'Work' });
    await api(page, 'POST', '/api/machine-groups', { name: 'Lab', worker_ids: [env.workerId] });
    await page.goto(`${env.baseURL}/command`);
    // Earlier specs may have left a Commander chat open: start a new one.
    const newChat = page.getByRole('tabpanel', { name: 'Commander' }).getByRole('button', { name: 'New chat', exact: true });
    if (await newChat.isVisible()) await newChat.click();

    const chip = page.getByTestId('context-scope');
    await expect(chip).toHaveText(/All machines/);
    await chip.click();
    await expect(page.getByTestId('scope-group-Lab')).toBeVisible();
    await expect(page.getByTestId('scope-label-Work')).toContainText('1 now');
    await shot(page, 'scope-picker.png');
    await page.getByTestId('scope-label-Work').click();
    await expect(chip).toHaveText(/^\s*Work\s*$/);

    // Starting the chat stores its machines on the hub.
    const before = new Set((await api<{ sessions: Array<{ id: string }> }>(page, 'GET', '/api/chats')).sessions.map((c) => c.id));
    const box = page.getByRole('textbox', { name: /message/i }).first();
    await box.fill('hello');
    await box.press('Enter');
    let chatId = '';
    await expect
      .poll(async () => {
        const { sessions } = await api<{ sessions: Array<{ id: string; fleet_tools?: boolean }> }>(page, 'GET', '/api/chats');
        chatId = sessions.find((c) => !before.has(c.id) && c.fleet_tools)?.id ?? '';
        return chatId;
      })
      .not.toBe('');
    await expect.poll(async () => (await api<{ scope: { kind: string; label?: string } }>(page, 'GET', `/api/chats/${chatId}/scope`)).scope).toEqual({ kind: 'label', label: 'Work' });

    // The chip stays editable on the chat: picking the group saves it at once.
    await chip.click();
    await page.getByTestId('scope-group-Lab').click();
    await expect(chip).toHaveText(/Lab/);
    await expect.poll(async () => (await api<{ label: string }>(page, 'GET', `/api/chats/${chatId}/scope`)).label).toBe('group Lab');

    // Select machines… hands a hand-picked set back to the chat.
    await chip.click();
    await page.getByTestId('scope-select').click();
    const dialog = page.getByRole('dialog');
    await expect(dialog.getByRole('heading', { name: 'Select machines' })).toBeVisible();
    await expect(dialog.getByTestId(`select-${env.workerSid}`)).toBeChecked();
    await dialog.getByTestId('select-none').click();
    await expect(dialog.getByTestId('selected-count')).toHaveText('0 selected');
    await dialog.getByTestId(`select-${env.workerSid}`).check();
    await expect(dialog.getByTestId('selected-count')).toHaveText('1 selected');
    await dialog.getByTestId('use-selection').click();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expect(chip).toHaveText(new RegExp(env.workerName));
    await expect
      .poll(async () => (await api<{ scope: { kind: string; id?: string } }>(page, 'GET', `/api/chats/${chatId}/scope`)).scope)
      .toEqual({ kind: 'machine', id: env.workerId });
  });

  test("a ticket's Run on is set in the new-ticket form and changed in its panel (SYR-0M4QQ)", async ({ page }) => {
    const env = loadEnv();
    await login(page, env, '/work');
    await api(page, 'POST', `/api/workers/${env.workerId}/labels`, { label: 'Work' });
    const title = `Run on ${Date.now()}`;
    await page.getByTestId('new-ticket').click();
    const form = page.getByTestId('new-ticket-dialog');
    await expect(form.getByTestId('nt-project').locator('option').first()).toBeAttached();
    await form.getByTestId('nt-title').fill(title);
    const chip = form.getByTestId('nt-run-on').getByTestId('context-scope');
    await expect(chip).toHaveText(/All machines/);
    await chip.click();
    await page.getByTestId('scope-label-Work').click();
    await expect(chip).toHaveText(/Work/);
    await form.getByTestId('nt-submit').click();
    await expect(form).toBeHidden();

    const card = page.getByTestId('work-board').locator('[data-card]').filter({ hasText: title });
    await expect(card).toBeVisible();
    const sid = (await card.getAttribute('data-card'))!;
    await expect.poll(async () => (await api<{ scope: unknown }>(page, 'GET', `/api/tickets/${sid}/scope`)).scope).toEqual({ kind: 'label', label: 'Work' });

    await card.click();
    const panelChip = page.locator('aside[aria-label="Review"]').getByTestId('ticket-run-on').getByTestId('context-scope');
    await expect(panelChip).toHaveText(/Work/);
    await panelChip.click();
    await page.getByTestId('scope-all').click();
    await expect.poll(async () => (await api<{ label: string }>(page, 'GET', `/api/tickets/${sid}/scope`)).label).toBe('All machines');
  });

  test('Settings → Commander → Runs on picks the machine; a Commander chat says where it runs (SYR-CNMDK)', async ({ page }) => {
    const env = loadEnv();
    await login(page, env, '/command');
    await page.getByRole('button', { name: 'Settings' }).click();
    const section = page.getByTestId('settings-commander-host');
    const chip = section.getByTestId('context-machine');
    await expect(chip).toHaveText(/The hub's machine|e2e-box/);
    await chip.click();
    await page.getByTestId(`pick-machine-${env.workerSid}`).click();
    await expect(chip).toHaveText(new RegExp(env.workerName));
    await page.getByRole('button', { name: 'Save Settings' }).click();
    await expect.poll(async () => (await api<{ commander_worker_id: string }>(page, 'GET', '/api/settings')).commander_worker_id).toBe(env.workerId);
    await page.keyboard.press('Escape');

    // A new Commander chat starts there and says so; the line opens the setting.
    const chat = await api<{ id: string; sid: string; worker_id: string }>(page, 'POST', '/api/chats', { title: 'Host check', agent: 'mock', fleet_tools: true });
    expect(chat.worker_id).toBe(env.workerId);
    await page.goto(`${env.baseURL}/command/${chat.sid}`);
    const line = page.getByTestId('commander-host').first();
    await expect(line).toHaveText(new RegExp(`Commander on ${env.workerName}`));
    await line.click();
    await expect(page.getByTestId('settings-commander-host')).toBeInViewport();
    await api(page, 'PUT', '/api/settings', { commander_worker_id: '' });
  });
});

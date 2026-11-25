import fs from 'node:fs';
import { expect, Page, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * The core flow in the mode UI against the suite's hub and mock worker: sign in (a bad
 * token is refused), see the machine, open a live session's terminal (snapshot replay,
 * read-only until you take control), ⌘K, subscription usage, machine details and the
 * audit log. Desktop runs it in Command and Fleet; a phone goes through the inbox.
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

async function loginThroughUI(page: Page, env: E2EEnv): Promise<void> {
  await page.goto(env.baseURL, { waitUntil: 'networkidle' });
  await expect(page.getByRole('heading', { name: 'Syre' })).toBeVisible();

  // An invalid token is rejected first.
  // Signed out, the login page offers pairing; the recovery key is the fallback.
  await page.getByRole('button', { name: 'Use recovery key' }).click();
  await page.locator('#token').fill('wrong-token');
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.getByText(/invalid recovery key/i)).toBeVisible();

  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.locator('#token')).toBeHidden();
}

async function api<T>(page: Page, method: string, path: string, body?: unknown): Promise<T> {
  return page.evaluate(
    async ({ method, path, body }) => {
      const res = await fetch(path, {
        method,
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-Syre-Request': '1' },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${await res.text()}`);
      return res.json();
    },
    { method, path, body },
  ) as Promise<T>;
}

async function taskStatus(page: Page, id: string): Promise<string> {
  return (await api<{ status: string }>(page, 'GET', `/api/tasks/${id}`)).status;
}

test.describe('Syre E2E Suite (Desktop & Mobile PWA)', () => {
  test('signs in, sees the machine, replays a terminal read-only until Take control, ⌘K, usage and machine details', async ({ page, isMobile }) => {
    const env = loadEnv();
    await loginThroughUI(page, env);

    // 1. The machine is listed: Fleet's machine rows on desktop, the read-only fleet on a phone.
    if (isMobile) {
      await expect(page).toHaveURL(/\/inbox$/);
      await page.getByTestId('mobile-fleet-link').click();
      await expect(page.getByTestId('mobile-fleet').locator(`[data-machine="${env.workerName}"]`)).toBeVisible();
      await page.getByRole('link', { name: 'Inbox' }).click();
      await expect(page).toHaveURL(/\/inbox$/);
    } else {
      await expect(page).toHaveURL(/\/command$/);
      await page.getByRole('navigation', { name: 'Modes' }).locator('[data-mode="fleet"]').click();
      // The name may carry an isolation badge ("e2e-box ctr" when the suite runs in a container).
      await expect(page.getByTestId('fleet-machines').locator(`[data-machine="${env.workerSid}"]`).getByRole('heading', { name: env.workerName })).toBeVisible();
      await page.getByRole('navigation', { name: 'Modes' }).locator('[data-mode="command"]').click();
    }

    // 2. A Commander-dispatched interactive mock task (read-only in the UI until you take control).
    const { task_id: taskId, task_sid: taskSid } = await api<{ task_id: string; task_sid: string }>(page, 'POST', '/api/tasks', {
      worker_selector: { worker_id: env.workerId },
      agent: 'mock',
      repo_path: env.workdir,
      prompt: 'WAIT_INPUT',
      mode: 'interactive',
      origin: 'commander',
    });
    expect(taskId).toBeTruthy();
    // Give the mock driver a moment to emit its banner into the ring buffer.
    await page.waitForTimeout(450);

    // Open it: from Live work on desktop; on a phone from the inbox's working list, then its full view.
    if (isMobile) {
      await page.getByTestId('mobile-working').click();
      await page.locator(`[data-work-item="${taskSid}"]`).click();
      await expect(page).toHaveURL(new RegExp(`/inbox/${taskSid}$`));
      await page.getByRole('link', { name: 'Open full view' }).click();
    } else {
      await page.locator(`aside[aria-label="Live work"] [data-work-item="${taskSid}"] button.sy-lw-open`).click();
    }
    await expect(page).toHaveURL(new RegExp(`/command/${taskSid}$`));
    await expect(page.getByTestId('command-tabs').locator(`[data-tab="${taskSid}"]`).getByRole('tab')).toHaveAttribute('aria-selected', 'true');

    // 3. Snapshot replay into the terminal; typing while read-only is refused with a hint.
    const xtermPane = page.locator('sy-session-tab app-xterm-pane');
    await expect(xtermPane).toBeVisible();
    await expect(xtermPane.locator('.xterm-helper-textarea')).toBeAttached();
    await xtermPane.locator('.xterm').click();
    await page.keyboard.type('exit');
    await page.keyboard.press('Enter');
    await expect(xtermPane.getByText(/Read-Only observation mode/i)).toBeVisible();
    expect(['running', 'waiting_input', 'assigned']).toContain(await taskStatus(page, taskId));

    // Take control: typing now reaches the PTY and "exit" completes the mock.
    const take = page.getByTestId('session-strip').getByTestId('take-control');
    await take.click();
    await expect(take).toHaveAttribute('aria-pressed', 'true');
    await xtermPane.locator('.xterm').click();
    await page.keyboard.type('exit');
    await page.keyboard.press('Enter');
    await expect.poll(() => taskStatus(page, taskId), { timeout: 10_000 }).toBe('completed');

    if (isMobile) {
      // 4. Subscription usage on a phone: the read-only fleet's quota list, inside 390px.
      await page.goto(`${env.baseURL}/inbox/fleet`);
      const quota = page.getByTestId('mobile-quota');
      await expect(quota.getByText('Mock', { exact: true })).toBeVisible();
      const box = await quota.boundingBox();
      expect(box?.width ?? 999).toBeLessThanOrEqual(390);
      const overflow = await page.locator('sy-mobile-fleet').evaluate((el) => el.scrollWidth - el.clientWidth);
      expect(overflow).toBeLessThanOrEqual(0);
      return;
    }

    // 4. ⌘K searches machines (blur the terminal first so the key is not eaten by the PTY).
    await page.getByTestId('fleet-state').click();
    await page.keyboard.press('ControlOrMeta+k');
    const palette = page.getByRole('dialog', { name: 'Search and commands' });
    const search = palette.getByRole('combobox');
    await expect(search).toBeVisible();
    await search.fill(env.workerName);
    await expect(palette.getByRole('option', { name: new RegExp(env.workerName) }).first()).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(palette).toBeHidden();

    // 5. Subscription usage in Command's fleet strip.
    const strip = page.getByTestId('fleet-strip');
    await strip.getByTestId('footer-usage').click();
    await expect(page.getByRole('dialog', { name: 'Subscription usage' })).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(page.getByRole('dialog', { name: 'Subscription usage' })).toBeHidden();

    // 6. Machine details from Fleet, with the inline "Are you sure?" removal prompt.
    await page.goto(`${env.baseURL}/fleet`);
    await page.getByTestId(`machine-details-${env.workerSid}`).click();
    const detailModal = page.locator('app-machine-detail-modal .sy-modal');
    await expect(detailModal).toBeVisible();
    await expect(detailModal.getByText('Worker ID')).toBeVisible();
    await detailModal.getByRole('button', { name: 'Remove Machine' }).click();
    await expect(detailModal.getByText(`Are you sure you want to remove "${env.workerName}"?`)).toBeVisible();
    await detailModal.getByRole('button', { name: 'Cancel' }).click();
    await page.keyboard.press('Escape');
    await expect(detailModal).toBeHidden();
  });

  test('the audit log lists control actions, live-appends and filters', async ({ page, isMobile }) => {
    test.skip(isMobile, 'desktop: the audit log opens from ⌘K or the Needs-you rail');
    const env = loadEnv();
    await loginThroughUI(page, env);

    // The API takes ids or short sids: dispatch to the worker by its m- sid and drive the
    // task by its t- sid. The UI shows the sid.
    const { task_id: taskId, task_sid: taskSid } = await api<{ task_id: string; task_sid: string }>(page, 'POST', '/api/tasks', {
      worker_selector: { worker_id: env.workerSid },
      agent: 'mock',
      repo_path: env.workdir,
      prompt: 'WAIT_INPUT',
      mode: 'interactive',
      origin: 'commander',
    });
    expect(taskId).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-/);
    expect(taskSid).toMatch(/^t-[0-9a-z]{5,12}$/);
    await api(page, 'POST', `/api/tasks/${taskSid}/control`, { enabled: true });

    // ⌘K → Audit log opens it as a dialog.
    await page.keyboard.press('ControlOrMeta+k');
    const palette = page.getByRole('dialog', { name: 'Search and commands' });
    await palette.getByRole('combobox').fill('audit');
    await palette.getByRole('option', { name: 'Audit log' }).click();
    const audit = page.getByRole('dialog', { name: 'Audit log' });
    const table = audit.getByRole('table');
    await expect(table.getByRole('columnheader', { name: 'User' })).toBeVisible();
    const takeRow = table.getByRole('row').filter({ hasText: 'Take control' }).first();
    // Signed in with the recovery key, this browser is a device of its own: "<name> (dv-…)".
    await expect(takeRow).toContainText(/\(dv-[0-9a-z]{5,12}\)/);
    await expect(takeRow).toContainText(env.workerName);
    await expect(takeRow).toContainText(taskSid);
    await expect(takeRow).not.toContainText(taskId);

    // A new action appears without reopening (fleet.update "audit").
    await api(page, 'POST', `/api/tasks/${taskId}/cancel`);
    await expect(table.getByRole('row').filter({ hasText: 'Cancel' }).first()).toBeVisible();

    // Filtering by action narrows the table.
    await audit.getByLabel('Action').selectOption('task.take_control');
    await expect(table.getByRole('row').filter({ hasText: 'Cancel' })).toHaveCount(0);
    await expect(table.getByRole('row').filter({ hasText: 'Take control' }).first()).toBeVisible();

    // The Needs-you rail's "Full audit log" opens the same dialog.
    await page.keyboard.press('Escape');
    await expect(audit).toBeHidden();
    await page.getByTestId('open-audit').click();
    await expect(audit.getByRole('table')).toBeVisible();
  });
});

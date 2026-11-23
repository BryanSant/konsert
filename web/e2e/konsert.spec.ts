import fs from 'node:fs';
import { expect, Page, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  workerName: string;
  workerId: string;
  workdir: string;
}

function loadEnv(): E2EEnv {
  return JSON.parse(fs.readFileSync(E2E_STATE_FILE, 'utf8')) as E2EEnv;
}

async function loginThroughUI(page: Page, env: E2EEnv): Promise<void> {
  await page.goto(env.baseURL, { waitUntil: 'networkidle' });
  await expect(page.getByRole('heading', { name: 'Konsert' })).toBeVisible();

  // Verify invalid token is rejected first
  await page.locator('#token').fill('wrong-token');
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.getByText(/invalid token/i)).toBeVisible();

  // Sign in with real admin token
  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.getByRole('heading', { name: 'KONSERT' })).toBeVisible();
}

test.describe('Konsert E2E Suite (Desktop & Mobile PWA)', () => {
  test('authenticates, verifies fleet sidebar, xterm snapshot replay, Read-Only vs Take Control, Cmd+K, Cmd+J, and footer popovers', async ({
    page,
  }, testInfo) => {
    const env = loadEnv();
    const isMobile = testInfo.project.name === 'mobile';

    await loginThroughUI(page, env);

    // 1. Verify Workspaces (Space > Project) & Machines sidebar
    if (isMobile) {
      // Open mobile slide-over fleet drawer
      await page.getByRole('banner').getByRole('button').first().click();
    }

    const sidebar = page.locator('app-fleet-sidebar:visible');
    await expect(sidebar.getByRole('button', { name: 'Workspaces' })).toBeVisible();
    await expect(sidebar.getByRole('button', { name: 'Machines' })).toBeVisible();

    // Switch to Machines view and verify connected worker e2e-box appears
    await sidebar.getByRole('button', { name: 'Machines' }).click();
    // The name may carry an isolation badge ("e2e-box ctr" when the suite runs in a container).
    await expect(sidebar.getByRole('button', { name: new RegExp(`^${env.workerName}( \\w+)?$`) })).toBeVisible();

    if (isMobile) {
      // Close mobile drawer before dispatching task
      await page.locator('.ks-backdrop').first().click({ force: true });
    }

    // 2. Dispatch an Orchestrated interactive mock task (defaults to Read-Only in UI!)
    const taskId = await page.evaluate(
      async ({ workerId, workdir }) => {
        const res = await fetch('/api/tasks', {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            worker_selector: { worker_id: workerId },
            agent: 'mock',
            repo_path: workdir,
            prompt: 'WAIT_INPUT',
            mode: 'interactive',
            origin: 'orchestrator',
          }),
        });
        if (!res.ok) throw new Error(`dispatch failed: ${res.status}`);
        const body = (await res.json()) as { task_id: string };
        return body.task_id;
      },
      { workerId: env.workerId, workdir: env.workdir }
    );
    expect(taskId).toBeTruthy();

    // Give mock driver a brief moment to emit initial banner into ring buffer
    await page.waitForTimeout(450);

    // Open the mock task from the Agents roster in the visible sidebar
    if (isMobile) {
      await page.getByRole('banner').getByRole('button').first().click();
    }
    const agentEntry = page
      .locator('app-fleet-sidebar:visible')
      .getByRole('button', { name: /workspace.*e2e-box/i })
      .first();
    await expect(agentEntry).toBeVisible();
    await agentEntry.click();

    // 3. Verify xterm snapshot replay and Read-Only guard blocking stdin vs Take Control forwarding stdin
    const xtermPane = page.locator('app-xterm-pane:visible').first();
    await expect(xtermPane).toBeVisible();
    await expect(xtermPane.getByRole('button', { name: 'Read-Only' })).toBeVisible();

    // Wait for xterm viewport to mount and snapshot frames to arrive
    const xtermHelper = xtermPane.locator('.xterm-helper-textarea');
    await expect(xtermHelper).toBeAttached();

    // Focus terminal and send keystrokes while in Read-Only mode -> must trigger Read-Only toast and NOT complete task
    await xtermPane.locator('.xterm').click();
    await page.keyboard.type('exit');
    await page.keyboard.press('Enter');
    await expect(xtermPane.getByText(/Read-Only observation mode/i)).toBeVisible();

    // Confirm task is still running/waiting_input (Read-Only blocked stdin!)
    const statusWhileReadOnly = await page.evaluate(async (id) => {
      const r = await fetch(`/api/tasks/${id}`, { credentials: 'same-origin' });
      const b = (await r.json()) as { status: string };
      return b.status;
    }, taskId);
    expect(['running', 'waiting_input', 'assigned']).toContain(statusWhileReadOnly);

    // Click Take Control -> switches to Interactive mode -> typing "exit\r" completes the mock PTY!
    await xtermPane.getByRole('button', { name: 'Take Control' }).click();
    await expect(xtermPane.getByRole('button', { name: 'Interactive' })).toBeVisible();

    await xtermPane.locator('.xterm').click();
    await page.keyboard.type('exit');
    await page.keyboard.press('Enter');

    await expect
      .poll(
        async () =>
          page.evaluate(async (id) => {
            const r = await fetch(`/api/tasks/${id}`, { credentials: 'same-origin' });
            const b = (await r.json()) as { status: string };
            return b.status;
          }, taskId),
        { timeout: 10_000 }
      )
      .toBe('completed');

    // 4. Desktop-only shortcuts & popovers: Cmd/Ctrl+K Spotlight, Cmd/Ctrl+J Scratchpad, Footer Popovers
    if (!isMobile) {
      // Blur xterm helper textarea first so Ctrl+K is not consumed as PTY kill-line
      await page.getByRole('banner').click({ position: { x: 10, y: 10 } });
      await page.keyboard.press('Control+k');
      const searchInput = page.getByPlaceholder(/Search spaces, projects, repos/i);
      await expect(searchInput).toBeVisible();
      await searchInput.fill(env.workerName);
      await expect(page.getByText('Machines', { exact: true }).last()).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(searchInput).toBeHidden();

      // Floating Scratchpad (Ctrl+J)
      await page.keyboard.press('Control+j');
      await expect(page.getByText('Scratch Workspace', { exact: true })).toBeVisible();
      await page.keyboard.press('Control+j');

      // Footer popovers (Usage, Resources, Ports, Hosts)
      const footer = page.locator('app-status-footer');
      await footer.getByTestId('footer-usage').click();
      await expect(footer.getByText('Subscription usage', { exact: true })).toBeVisible();
      await page.keyboard.press('Escape');

      await footer.getByTestId('footer-resources').click();
      await expect(footer.getByText('Resource Manager')).toBeVisible();

      await footer.getByTestId('footer-ports').click();
      await expect(footer.getByText('Listening Fleet Ports')).toBeVisible();

      await footer.getByTestId('footer-hosts').click();
      await expect(footer.getByText('Fleet Hosts')).toBeVisible();
      await footer.getByRole('button', { name: new RegExp(env.workerName, 'i') }).click();

      // Machine Details dialog + inline "Are you sure?" removal prompt
      const detailModal = page.locator('app-machine-detail-modal .ks-modal');
      await expect(detailModal).toBeVisible();
      await expect(detailModal.getByText('Worker ID')).toBeVisible();
      await detailModal.getByRole('button', { name: 'Remove Machine' }).click();
      await expect(detailModal.getByText(`Are you sure you want to remove "${env.workerName}"?`)).toBeVisible();
      await detailModal.getByRole('button', { name: 'Cancel' }).click();
      await page.keyboard.press('Escape');
      await expect(detailModal).toBeHidden();
    } else {
      // Mobile-specific check: footer usage popover renders cleanly within 390px viewport
      const footer = page.locator('app-status-footer');
      await footer.getByTestId('footer-usage').click();
      const popover = footer.getByRole('dialog', { name: 'Subscription usage' });
      await expect(popover).toBeVisible();
      const box = await popover.boundingBox();
      expect(box).not.toBeNull();
      expect((box?.width ?? 999)).toBeLessThanOrEqual(390);
    }
  });

  test('audit log inspector lists control actions, live-appends and filters', async ({ page }, testInfo) => {
    test.skip(testInfo.project.name === 'mobile', 'desktop inspector rail');
    const env = loadEnv();
    await loginThroughUI(page, env);

    const api = (path: string, body?: unknown) =>
      page.evaluate(
        async ({ path, body }) => {
          const res = await fetch(path, {
            method: 'POST',
            credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json', 'X-Konsert-Request': '1' },
            body: JSON.stringify(body ?? {}),
          });
          if (!res.ok) throw new Error(`${path} failed: ${res.status}`);
          return res.json();
        },
        { path, body },
      );

    const { task_id: taskId } = (await api('/api/tasks', {
      worker_selector: { worker_id: env.workerId },
      agent: 'mock',
      repo_path: env.workdir,
      prompt: 'WAIT_INPUT',
      mode: 'interactive',
      origin: 'orchestrator',
    })) as { task_id: string };
    await api(`/api/tasks/${taskId}/control`, { enabled: true });

    await page.getByRole('button', { name: 'Audit Log' }).click();
    const inspector = page.locator('#ks-right-pane');
    const table = inspector.getByRole('table');
    await expect(table.getByRole('columnheader', { name: 'User' })).toBeVisible();
    const takeRow = table.getByRole('row').filter({ hasText: 'Take control' }).first();
    await expect(takeRow).toContainText('operator');
    await expect(takeRow).toContainText(env.workerName);

    // A new action appears without reloading the panel (fleet.update "audit").
    await api(`/api/tasks/${taskId}/cancel`);
    await expect(table.getByRole('row').filter({ hasText: 'Cancel' }).first()).toBeVisible();

    // Filtering by action narrows the table.
    await inspector.getByLabel('Action').selectOption('task.take_control');
    await expect(table.getByRole('row').filter({ hasText: 'Cancel' })).toHaveCount(0);
    await expect(table.getByRole('row').filter({ hasText: 'Take control' }).first()).toBeVisible();
  });
});

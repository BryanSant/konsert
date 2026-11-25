import fs from 'node:fs';
import { expect, Page, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  workerId: string;
  workerName: string;
  workerSid: string;
  workdir: string;
}

function loadEnv(): E2EEnv {
  return JSON.parse(fs.readFileSync(E2E_STATE_FILE, 'utf8')) as E2EEnv;
}

async function signIn(page: Page, env: E2EEnv, path = '/'): Promise<void> {
  await page.goto(`${env.baseURL}${path}`);
  await page.getByRole('button', { name: 'Use recovery key' }).click();
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

async function dispatchWaiting(page: Page, env: E2EEnv, title: string): Promise<{ id: string; sid: string }> {
  const res = await api<{ task_id: string }>(page, 'POST', '/api/tasks', {
    worker_selector: { worker_id: env.workerId },
    agent: 'mock',
    repo_path: env.workdir,
    prompt: `${title}\nWAIT_INPUT`,
    mode: 'interactive',
    origin: 'commander',
  });
  let sid = '';
  await expect
    .poll(async () => {
      const t = await api<{ task_sid: string; status: string }>(page, 'GET', `/api/tasks/${res.task_id}`);
      sid = t.task_sid;
      return t.status;
    })
    .toBe('waiting_input');
  return { id: res.task_id, sid };
}

async function createAgentChat(page: Page, env: E2EEnv, title: string): Promise<{ id: string; sid: string }> {
  const res = await api<{ id: string; sid: string }>(page, 'POST', '/api/chat/sessions', {
    title,
    backend: 'agent',
    worker_id: env.workerId,
    agent: 'mock',
    cwd: env.workdir,
  });
  return { id: res.id, sid: res.sid };
}

test.describe('PWA deep links by sid (desktop)', () => {
  test.skip(({ isMobile }) => isMobile, 'desktop deep links');

  test('opens task, chat, and machine via /command/<sid>, /<sid>, reload, and unknown sid shows not-found', async ({ browser, page }) => {
    const env = loadEnv();
    await signIn(page, env);

    const task = await dispatchWaiting(page, env, 'Deep link task');
    const chat = await createAgentChat(page, env, 'Deep link chat');

    try {
      // 1. Fresh context (empty localStorage, signed out): /command/<taskSid> opens the tab after sign-in and survives reload.
      const ctx1 = await browser.newContext({ viewport: { width: 1440, height: 900 }, permissions: ['clipboard-read', 'clipboard-write'] });
      try {
        const p1 = await ctx1.newPage();
        await signIn(p1, env, `/command/${task.sid}`);
        await expect(p1).toHaveURL(new RegExp(`/command/${task.sid}$`));
        const tab = p1.getByTestId('command-tabs').locator(`[data-tab="${task.sid}"] [role="tab"]`);
        await expect(tab).toBeVisible();
        await expect(tab).toHaveAttribute('aria-selected', 'true');
        await expect(p1.getByTestId('session-strip')).toContainText(task.sid);

        // Copy link action on the session tab copies origin + '/' + sid.
        await p1.getByTestId('copy-session-link').click();
        const copiedSessionUrl = await p1.evaluate(() => navigator.clipboard.readText());
        expect(copiedSessionUrl).toBe(`${env.baseURL}/${task.sid}`);

        // Reload preserves the open tab.
        await p1.reload();
        await expect(p1).toHaveURL(new RegExp(`/command/${task.sid}$`));
        await expect(p1.getByTestId('command-tabs').locator(`[data-tab="${task.sid}"] [role="tab"]`)).toHaveAttribute('aria-selected', 'true');
      } finally {
        await ctx1.close();
      }

      // 2. Fresh context: mode-agnostic /<taskSid> redirects to /command/<taskSid> and opens the tab.
      const ctx2 = await browser.newContext({ viewport: { width: 1440, height: 900 } });
      try {
        const p2 = await ctx2.newPage();
        await signIn(p2, env, `/${task.sid}`);
        await expect(p2).toHaveURL(new RegExp(`/command/${task.sid}$`));
        await expect(p2.getByTestId('command-tabs').locator(`[data-tab="${task.sid}"] [role="tab"]`)).toHaveAttribute('aria-selected', 'true');
      } finally {
        await ctx2.close();
      }

      // 3. Fresh context: mode-agnostic /<chatSid> redirects to /command/<chatSid> and opens the chat tab.
      const ctx3 = await browser.newContext({ viewport: { width: 1440, height: 900 } });
      try {
        const p3 = await ctx3.newPage();
        await signIn(p3, env, `/${chat.sid}`);
        await expect(p3).toHaveURL(new RegExp(`/command/${chat.sid}$`));
        await expect(p3.getByTestId('command-tabs').locator(`[data-tab="${chat.sid}"] [role="tab"]`)).toHaveAttribute('aria-selected', 'true');
      } finally {
        await ctx3.close();
      }

      // 4. Fresh context: mode-agnostic /<workerSid> redirects to /fleet/<workerSid>, highlights the machine, and Copy link in machine detail works.
      const ctx4 = await browser.newContext({ viewport: { width: 1440, height: 900 }, permissions: ['clipboard-read', 'clipboard-write'] });
      try {
        const p4 = await ctx4.newPage();
        await signIn(p4, env, `/${env.workerSid}`);
        await expect(p4).toHaveURL(new RegExp(`/fleet/${env.workerSid}$`));
        const machineRow = p4.locator(`[data-machine="${env.workerSid}"]`);
        await expect(machineRow).toHaveClass(/sy-machine-selected/);

        await p4.getByTestId(`machine-details-${env.workerSid}`).click();
        await p4.getByTestId('copy-machine-link').click();
        const copiedMachineUrl = await p4.evaluate(() => navigator.clipboard.readText());
        expect(copiedMachineUrl).toBe(`${env.baseURL}/${env.workerSid}`);
      } finally {
        await ctx4.close();
      }

      // 5. Unknown sids show clear not-found states instead of silently dropping.
      await page.goto(`${env.baseURL}/command/t-nope99`);
      await expect(page.getByTestId('command-not-found')).toContainText('t-nope99');

      await page.goto(`${env.baseURL}/fleet/m-nope99`);
      await expect(page.getByTestId('fleet-not-found')).toContainText('m-nope99');

      await page.goto(`${env.baseURL}/work/t-nope99`);
      await expect(page.getByTestId('work-not-found')).toContainText('t-nope99');
    } finally {
      await api(page, 'POST', `/api/tasks/${task.id}/cancel`).catch(() => undefined);
      await api(page, 'DELETE', `/api/chat/sessions/${chat.id}`).catch(() => undefined);
    }
  });
});

test.describe('PWA deep links by sid (mobile)', () => {
  test.skip(({ isMobile }) => !isMobile, 'mobile deep links');

  test('redirects /<taskSid> to /inbox/<taskSid> on phone width in a fresh context', async ({ browser, page }) => {
    const env = loadEnv();
    await signIn(page, env);

    const task = await dispatchWaiting(page, env, 'Mobile deep link task');

    try {
      const ctx = await browser.newContext({
        viewport: { width: 390, height: 844 },
        isMobile: true,
        hasTouch: true,
      });
      try {
        const p = await ctx.newPage();
        await signIn(p, env, `/${task.sid}`);
        await expect(p).toHaveURL(new RegExp(`/inbox/${task.sid}$`));
        await expect(p.getByRole('heading', { level: 1 })).toContainText('Mobile deep link task');
        await expect(p.getByText(task.sid).first()).toBeVisible();
      } finally {
        await ctx.close();
      }
    } finally {
      await api(page, 'POST', `/api/tasks/${task.id}/cancel`).catch(() => undefined);
    }
  });
});

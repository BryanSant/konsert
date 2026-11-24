import fs from 'node:fs';
import path from 'node:path';
import { expect, Page, WebSocketRoute, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * Shells left running by finished sessions (KON-DCEZV), against the suite's hub and
 * konsertd: Settings → Shells left running saves the global mode, idle period and a
 * per-project mode; a "shells left running" Needs-you item offers Kill all, which really
 * terminates the shell a finished mock task left behind (BG_SHELL).
 *
 * The hub raises the item after the idle period (30 min by default, at least 1 min), so
 * the test pushes the hub's fleet.update frame for the real session into the page's socket;
 * the hub's timing (remind, ask, stop after idle, Keep, holds) is covered by the Go tests
 * with an injected clock. Set KONSERT_SHOTS_DIR to save screenshots.
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  workerId: string;
  workdir: string;
}

function loadEnv(): E2EEnv {
  return JSON.parse(fs.readFileSync(E2E_STATE_FILE, 'utf8')) as E2EEnv;
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

/** Proxies the browser's hub socket so the test can push frames to the page. */
async function proxySocket(page: Page): Promise<{ push: (type: string, payload: unknown) => void }> {
  let client: WebSocketRoute | null = null;
  await page.routeWebSocket(/\/ws\/client/, (ws) => {
    const server = ws.connectToServer();
    ws.onMessage((m) => server.send(m));
    server.onMessage((m) => ws.send(m));
    client = ws;
  });
  return { push: (type, payload) => client?.send(JSON.stringify({ type, payload, ts: new Date().toISOString() })) };
}

async function signIn(page: Page, env: E2EEnv): Promise<void> {
  await page.goto(`${env.baseURL}/command`);
  await page.getByRole('button', { name: 'Use recovery key' }).click();
  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.locator('#token')).toBeHidden();
  await expect(page.locator('app-chat-view')).toBeVisible();
}

interface ShellPolicy {
  mode: string;
  idle_minutes: number;
  projects: Record<string, string>;
}

interface Activity {
  task_id?: string;
  shells?: number;
  shell_list?: Array<{ pid: number; command: string; started_at: string; since: string }>;
}

test.describe('Shells left running', () => {
  test('Settings saves the policy: global mode, idle period and a project override', async ({ page }) => {
    const env = loadEnv();
    await signIn(page, env);
    const before = await api<ShellPolicy>(page, 'GET', '/api/settings/shells');
    expect(before.mode).toBe('remind');
    expect(before.idle_minutes).toBe(30);
    const projects = await api<Array<{ id: string; sid: string }> | { projects: Array<{ id: string; sid: string }> }>(page, 'GET', '/api/projects');
    const list = Array.isArray(projects) ? projects : (projects.projects ?? []);

    await page.getByRole('button', { name: 'Settings' }).click();
    const sec = page.getByTestId('settings-shells');
    await sec.scrollIntoViewIfNeeded();
    await expect(sec.getByTestId('shells-hint')).toContainText('After 30 min idle, one Needs-you item per session');
    await sec.getByTestId('shells-mode').selectOption('stop');
    await sec.getByTestId('shells-idle').fill('45');
    await expect(sec.getByTestId('shells-hint')).toContainText('idle for 45 min');
    if (list.length) {
      await sec.getByTestId('shells-projects').locator('summary').click();
      await sec.getByTestId(`shells-project-${list[0].sid}`).selectOption('ask');
    }
    await sec.getByTestId('shells-save').click();
    await expect(sec.getByRole('status')).toHaveText('Saved');
    await shot(page, 'settings-shells.png');
    const saved = await api<ShellPolicy>(page, 'GET', '/api/settings/shells');
    expect(saved.mode).toBe('stop');
    expect(saved.idle_minutes).toBe(45);
    if (list.length) expect(saved.projects[list[0].id]).toBe('ask');

    // Back to the defaults for the rest of the suite.
    await api(page, 'PUT', '/api/settings/shells', { mode: 'remind', idle_minutes: 30, projects: {} });
  });

  test('a shells-left-running item kills the shell a finished task left behind', async ({ page }) => {
    const env = loadEnv();
    const sock = await proxySocket(page);
    await signIn(page, env);
    const res = await api<{ task_id: string }>(page, 'POST', '/api/tasks', {
      worker_selector: { worker_id: env.workerId },
      agent: 'mock',
      repo_path: env.workdir,
      prompt: 'Leave a dev server running BG_SHELL=300 SLEEP=3',
      mode: 'oneshot',
      origin: 'orchestrator',
    });
    const id = res.task_id;
    await expect
      .poll(async () => ['completed', 'idle'].includes((await api<{ status: string }>(page, 'GET', `/api/tasks/${id}`)).status), { timeout: 15_000 })
      .toBe(true);
    let act: Activity | undefined;
    await expect
      .poll(
        async () => {
          act = (await api<{ activity?: Activity[] }>(page, 'GET', '/api/fleet')).activity?.find((a) => a.task_id === id);
          return act?.shells ?? 0;
        },
        { timeout: 15_000 },
      )
      .toBe(1);
    // Nothing raised yet: the session has not been idle for 30 minutes.
    expect((await api<{ reminders: unknown[] }>(page, 'GET', '/api/shells/reminders')).reminders).toEqual([]);

    // The hub's item for this session, as it sends it after the idle period.
    sock.push('fleet.update', {
      kind: 'shell_reminder',
      shell_reminder: {
        id: `task:${id}`,
        task_id: id,
        worker_id: env.workerId,
        title: '1 shell left running by Leave a dev server running',
        session_title: 'Leave a dev server running',
        shells: 1,
        shell_list: act?.shell_list ?? [],
        idle_since: new Date(Date.now() - 31 * 60_000).toISOString(),
        can_ask: false,
        mode: 'remind',
        created_at: new Date().toISOString(),
      },
    });
    const item = page.getByTestId('needs-you-rail').locator(`[data-shells="task-${id}"]`);
    await expect(item).toContainText('1 shell left running by Leave a dev server running');
    await item.click();
    const card = page.getByTestId('needs-you-shells');
    await expect(card).toContainText('sleep 300');
    // The task ended, so its agent can't be asked: Kill all and Keep running are offered.
    await expect(card.getByTestId('shells-ask')).toHaveCount(0);
    await expect(card.getByTestId('shells-keep')).toBeVisible();
    await shot(page, 'needs-you-shells.png');
    const kill = page.waitForRequest((r) => r.url().endsWith(`/api/tasks/${id}/shells/kill`) && r.method() === 'POST');
    await card.getByTestId('shells-kill').click();
    await kill;
    await expect(page.getByTestId('needs-you-rail').locator(`[data-shells="task-${id}"]`)).toHaveCount(0);
    // konsertd really terminated it.
    await expect
      .poll(async () => (await api<{ activity?: Activity[] }>(page, 'GET', '/api/fleet')).activity?.find((a) => a.task_id === id)?.shells ?? 0, {
        timeout: 15_000,
      })
      .toBe(0);
    const audit = await api<{ events: Array<{ action: string; task_id?: string }> }>(page, 'GET', '/api/audit?action=session.shell_kill');
    expect(audit.events.some((e) => e.task_id === id)).toBe(true);
  });
});

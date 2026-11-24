import fs from 'node:fs';
import { expect, Page, WebSocketRoute, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * Shells and sub-agents on Fleet session cards (KON-ZAY87), against the suite's hub and
 * konsertd: a mock task starts a real background shell (BG_SHELL), konsertd counts it from
 * the process tree and the card's chip and spinner badge rise; "Ask to stop" and "Kill"
 * call the cleanup API (Kill really terminates it, so the count falls). Sub-agents are
 * driven by fleet.update frames injected into the browser's socket.
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

async function api<T>(page: Page, method: string, path: string, body?: unknown): Promise<T> {
  return page.evaluate(
    async ({ method, path, body }) => {
      const res = await fetch(path, {
        method,
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-Konsert-Request': '1' },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${await res.text()}`);
      return res.json();
    },
    { method, path, body },
  ) as Promise<T>;
}

/** Proxies the browser's hub socket so a test can push extra frames to the page. */
async function proxySocket(
  page: Page,
): Promise<{ push: (type: string, payload: unknown) => void }> {
  let client: WebSocketRoute | null = null;
  await page.routeWebSocket(/\/ws\/client/, (ws) => {
    const server = ws.connectToServer();
    ws.onMessage((m) => server.send(m));
    server.onMessage((m) => ws.send(m));
    client = ws;
  });
  return {
    push: (type, payload) =>
      client?.send(JSON.stringify({ type, payload, ts: new Date().toISOString() })),
  };
}

async function signIn(page: Page, env: E2EEnv): Promise<void> {
  await page.goto(env.baseURL);
  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.locator('#token')).toBeHidden();
}

async function dispatch(
  page: Page,
  env: E2EEnv,
  prompt: string,
  interactive: boolean,
): Promise<{ id: string; sid: string }> {
  const res = await api<{ task_id: string; task_sid: string }>(page, 'POST', '/api/tasks', {
    worker_selector: { worker_id: env.workerId },
    agent: 'mock',
    repo_path: env.workdir,
    prompt,
    mode: interactive ? 'interactive' : 'oneshot',
    origin: 'orchestrator',
  });
  const t = await api<{ task_sid: string }>(page, 'GET', `/api/tasks/${res.task_id}`);
  return { id: res.task_id, sid: t.task_sid };
}

test.describe('Shells and sub-agents (desktop)', () => {
  test.skip(({ isMobile }) => isMobile, 'desktop Fleet');

  test('a background shell shows on its Fleet card; ask and kill clean it up', async ({ page }) => {
    const env = loadEnv();
    const sock = await proxySocket(page);
    await signIn(page, env);
    const task = await dispatch(page, env, 'Serve the docs BG_SHELL=300\nWAIT_INPUT', true);
    await page.goto(`${env.baseURL}/fleet`);
    const card = page.locator(`[data-session-card="${task.sid}"]`);

    // konsertd counts the shell from the process tree (~1 s poll + debounce).
    await expect(card.getByTestId('shell-chip')).toHaveText('❯ 1 shell · 1 busy', {
      timeout: 15_000,
    });
    await expect(card.getByTestId('spinner-shells')).toHaveText('❯1');
    const row = card.getByTestId('shell-row');
    await expect(row).toHaveCount(1);
    await expect(row).toContainText('sleep 300; true');
    const pid = Number(await row.getAttribute('data-pid'));
    expect(pid).toBeGreaterThan(0);

    // Sub-agents from fleet.update frames: chip, rows and orbiting dots.
    const snap = await api<{ activity?: Array<Record<string, unknown>> }>(
      page,
      'GET',
      '/api/fleet',
    );
    const cur = snap.activity?.find((a) => a['task_id'] === task.id) ?? {};
    const now = new Date().toISOString();
    sock.push('fleet.update', {
      kind: 'activity',
      activity: {
        ...cur,
        task_id: task.id,
        worker_id: env.workerId,
        state: 'working',
        tool: 'Task',
        waiting: true,
        subagents: [
          {
            id: 'A',
            title: 'S3 client tests',
            state: 'working',
            label: 'Run: go test ./internal/cache/...',
            since: now,
          },
          { id: 'B', title: 'Docs for cache flags', state: 'speaking', since: now },
        ],
        since: now,
        at: new Date().toISOString(),
      },
    });
    await expect(card.getByTestId('subagent-chip')).toHaveText('◉ 2 sub-agents');
    await expect(card.getByTestId('subagent-row')).toHaveCount(2);
    await expect(card.getByTestId('subagent-row').first()).toContainText(
      'S3 client tests · Run: go test ./internal/cache/...',
    );
    await expect(card.locator('.ks-sp-dot')).toHaveCount(2);
    await expect(card.locator('[data-state]').first()).toHaveAttribute('data-state', 'waiting');
    // A sub-agent asks: the whole ring turns amber.
    sock.push('fleet.update', {
      kind: 'activity',
      activity: {
        ...cur,
        task_id: task.id,
        worker_id: env.workerId,
        state: 'asking',
        label: 'Sub-agent “Docs for cache flags” asks: Allow Write: docs/flags.md?',
        subagents: [
          {
            id: 'B',
            title: 'Docs for cache flags',
            state: 'asking',
            label: 'Allow Write: docs/flags.md?',
            since: now,
          },
        ],
        since: now,
        at: new Date().toISOString(),
      },
    });
    await expect(card.locator('[data-state]').first()).toHaveAttribute('data-state', 'asking');
    await expect(card.getByTestId('fleet-now')).toContainText(
      'Sub-agent “Docs for cache flags” asks',
    );

    // Ask the agent: the API gets the shell's pid and start time.
    const askReq = page.waitForRequest(
      (r) => r.url().endsWith(`/api/tasks/${task.id}/shells/ask`) && r.method() === 'POST',
    );
    await card.getByTestId('shell-ask').click();
    const ask = await askReq;
    expect(ask.postDataJSON().shells[0].pid).toBe(pid);
    await expect(card.getByTestId('shell-note')).toContainText('Asked the agent to stop');

    // Kill, after confirming: the shell really goes, and so do the chip and badge.
    page.once('dialog', (d) => {
      expect(d.message()).toContain(`pid ${pid}`);
      void d.accept();
    });
    const killReq = page.waitForRequest(
      (r) => r.url().endsWith(`/api/tasks/${task.id}/shells/kill`) && r.method() === 'POST',
    );
    await card.getByTestId('shell-kill').click();
    expect((await killReq).postDataJSON().shells[0].pid).toBe(pid);
    await expect(card.getByTestId('shell-chip')).toHaveCount(0, { timeout: 15_000 });
    await expect(card.getByTestId('spinner-shells')).toHaveCount(0);

    await api(page, 'POST', `/api/tasks/${task.id}/steer`, { input_text: 'exit' }).catch(
      () => undefined,
    );
  });

  test('a shell left running after the session ended keeps the card and can be killed', async ({
    page,
  }) => {
    const env = loadEnv();
    await signIn(page, env);
    const task = await dispatch(page, env, 'Leave a server running BG_SHELL=300 SLEEP=3', false);
    await expect
      .poll(
        async () =>
          ['completed', 'idle'].includes(
            (await api<{ status: string }>(page, 'GET', `/api/tasks/${task.id}`)).status,
          ),
        { timeout: 15_000 },
      )
      .toBe(true);
    await page.goto(`${env.baseURL}/fleet`);
    const card = page.locator(`[data-session-card="${task.sid}"]`);
    await expect(card.getByTestId('shell-chip')).toHaveText('❯ 1 shell left running', {
      timeout: 15_000,
    });
    // Its agent is gone: asking isn't available, killing is.
    await expect(card.getByTestId('shell-ask')).toBeDisabled();
    page.once('dialog', (d) => void d.accept());
    await card.getByTestId('shell-kill').click();
    await expect(card.getByTestId('shell-chip')).toHaveCount(0, { timeout: 15_000 });
  });
});

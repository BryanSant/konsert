import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { expect, Locator, Page, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * The mode UI (KON-88NN9): users land in Command on desktop and in the needs-you inbox on a
 * phone. Command has one Needs-you area (the right rail); Work is the board with the review
 * panel; Fleet has the machines and the timeline range picker; Wall tiles stream and stay put.
 * Runs against the suite's hub with the mock worker.
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  workerId: string;
  workerName: string;
  workdir: string;
}

const QUESTION = 'Accept the shortened exit criteria on KON-2Q0CH?';

function loadEnv(): E2EEnv {
  return JSON.parse(fs.readFileSync(E2E_STATE_FILE, 'utf8')) as E2EEnv;
}

async function signIn(page: Page, env: E2EEnv, path = '/'): Promise<void> {
  await page.goto(`${env.baseURL}${path}`);
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
        headers: { 'Content-Type': 'application/json', 'X-Konsert-Request': '1' },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${await res.text()}`);
      return res.json();
    },
    { method, path, body },
  ) as Promise<T>;
}

/** Dispatches a mock task that waits for input (so it stays live) and returns its sid. */
async function dispatchWaiting(page: Page, env: E2EEnv, title: string): Promise<{ id: string; sid: string }> {
  const res = await api<{ task_id: string }>(page, 'POST', '/api/tasks', {
    worker_selector: { worker_id: env.workerId },
    agent: 'mock',
    repo_path: env.workdir,
    prompt: `${title}\nWAIT_INPUT`,
    mode: 'interactive',
    origin: 'orchestrator',
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

async function cancel(page: Page, id: string): Promise<void> {
  await api(page, 'POST', `/api/tasks/${id}/cancel`).catch(() => undefined);
}

/** Starts a mock agent chat in the orchestrator tab and has it ask the canned question. */
async function askFromOrchestrator(page: Page): Promise<string> {
  const chat = page.locator('app-chat-view');
  await chat.getByTestId('new-chat').click();
  await chat.getByTestId('account-chip').click();
  await page.getByRole('menu', { name: 'Agent account' }).locator('[data-account="mock2"]').click();
  await chat.getByTestId('composer-input').fill('question');
  await chat.getByTestId('send-button').click();
  let sid = '';
  await expect
    .poll(async () => {
      const res = await api<{ questions: Array<{ sid: string; prompt: string; status: string; created_at: string }> }>(page, 'GET', '/api/questions');
      const q = res.questions.filter((x) => x.prompt === QUESTION && x.status === 'open').sort((a, b) => b.created_at.localeCompare(a.created_at))[0];
      sid = q?.sid ?? '';
      return sid;
    })
    .toMatch(/^q-/);
  return sid;
}

function rail(page: Page): Locator {
  return page.getByTestId('needs-you-rail');
}

test.describe('Mode UI (desktop)', () => {
  test.skip(({ isMobile }) => isMobile, 'desktop modes');

  test('lands in Command with the mode header; keys 1–4 switch modes and the URL follows', async ({ page }) => {
    const env = loadEnv();
    await signIn(page, env);
    await expect(page).toHaveURL(/\/command$/);
    const nav = page.getByRole('navigation', { name: 'Modes' });
    await expect(nav.getByRole('link')).toHaveText([/1\s*Command/, /2\s*Work/, /3\s*Fleet/, /4\s*Wall/]);
    await expect(nav.locator('[data-mode="command"]')).toHaveAttribute('aria-current', 'page');
    await expect(page.getByTestId('fleet-state')).toHaveText('FLEET: RUNNING');
    await expect(page.getByTestId('pause-everything')).toBeVisible();

    // The orchestrator tab is pinned: no close button.
    const tabs = page.getByTestId('command-tabs');
    await expect(tabs.getByRole('tab', { name: 'Orchestrator' })).toHaveAttribute('aria-selected', 'true');
    await expect(tabs.locator('[data-tab="orchestrator"] [aria-label^="Close"]')).toHaveCount(0);

    await page.locator('body').click({ position: { x: 700, y: 400 } });
    await page.locator('body').press('Escape');
    await page.getByTestId('mode-header').click({ position: { x: 600, y: 10 } });
    for (const [key, mode] of [
      ['2', 'work'],
      ['3', 'fleet'],
      ['4', 'wall'],
      ['1', 'command'],
    ]) {
      await page.keyboard.press(key);
      await expect(page).toHaveURL(new RegExp(`/${mode}$`));
      await expect(nav.locator(`[data-mode="${mode}"]`)).toHaveAttribute('aria-current', 'page');
    }

    // ⌘K palette jumps between modes too.
    await page.getByTestId('open-palette').click();
    const palette = page.getByRole('dialog', { name: 'Search and commands' });
    await palette.getByRole('combobox').fill('fleet');
    await palette.getByRole('combobox').press('Enter');
    await expect(page).toHaveURL(/\/fleet$/);
  });

  test('Command: a session opens as a closable tab with GOAL/NOW and controls; holds land in the rail', async ({ page }) => {
    const env = loadEnv();
    await signIn(page, env);
    const task = await dispatchWaiting(page, env, 'Tidy the release notes');
    try {
      const live = page.locator('aside[aria-label="Live work"]');
      const row = live.locator(`[data-work-item="${task.sid}"]`);
      await expect(row).toContainText('Tidy the release notes');
      await row.click();
      await expect(page).toHaveURL(new RegExp(`/command/${task.sid}$`));

      const tab = page.getByTestId('command-tabs').locator(`[data-tab="${task.sid}"]`);
      await expect(tab.getByRole('tab')).toHaveAttribute('aria-selected', 'true');
      const strip = page.getByTestId('session-strip');
      await expect(strip).toContainText('GOAL');
      await expect(strip).toContainText('Tidy the release notes');
      await expect(strip).toContainText('Waiting for your input');
      await expect(strip.locator('[data-control="ASKING"]')).toBeVisible();

      // Take control and hand back.
      const take = strip.getByTestId('take-control');
      await take.click();
      await expect(take).toHaveAttribute('aria-pressed', 'true');
      await expect(strip.locator('[data-control="YOU"]')).toBeVisible();
      await take.click();
      await expect(take).toHaveAttribute('aria-pressed', 'false');

      // Hold: the hub places a session hold; it shows in the rail, released from the strip.
      await strip.getByTestId('session-hold-btn').click();
      await expect(rail(page).locator('[data-hold]').filter({ hasText: task.sid })).toBeVisible();
      await expect(strip.locator('[data-control="HELD"], [data-control="ASKING"]').first()).toBeVisible();
      await strip.getByTestId('session-release-hold').click();
      await expect(rail(page).locator('[data-hold]').filter({ hasText: task.sid })).toHaveCount(0);

      // Deep link survives a reload, then the tab closes back to the orchestrator.
      await page.reload();
      await expect(page.getByTestId('session-strip')).toContainText('Tidy the release notes');
      await page.getByTestId(`close-tab-${task.sid}`).click();
      await expect(page).toHaveURL(/\/command$/);
      await expect(page.getByTestId('command-tabs').locator(`[data-tab="${task.sid}"]`)).toHaveCount(0);
    } finally {
      await cancel(page, task.id);
    }
  });

  test('Command: questions are answered only in the rail; the chat shows a pointer, live work shows none', async ({ page }) => {
    const env = loadEnv();
    await signIn(page, env);
    const sid = await askFromOrchestrator(page);

    const card = rail(page).locator(`[data-question="${sid}"]`);
    await expect(card).toContainText(QUESTION);
    await expect(card.getByTestId('rail-ask')).toBeVisible();
    // One Needs-you area: the chat points to the rail, the left pane carries nothing.
    const chat = page.locator('app-chat-view');
    await expect(chat.locator(`[data-testid="question-pointer"][data-question="${sid}"]`)).toBeVisible();
    await expect(chat.getByTestId('question-card')).toHaveCount(0);
    await expect(page.locator('aside[aria-label="Live work"] [data-question], aside[aria-label="Live work"] [data-rail-item]')).toHaveCount(0);
    await expect(page.getByTestId('needs-you-count')).toBeVisible();

    // "Answer in Needs you" focuses the rail card; answering there unblocks the agent.
    await chat.locator(`[data-testid="question-pointer"][data-question="${sid}"]`).getByRole('button').click();
    await expect(card).toBeFocused();
    await card.getByTestId('rail-option-1').click();
    await expect(card).toHaveCount(0);
    await expect(chat.getByTestId('assistant-message').filter({ hasText: 'The user chose: "No, keep the strict exit criteria"' })).toBeVisible();
  });

  test('Work: board with lanes and columns, review panel, board/list toggle', async ({ page }) => {
    const env = loadEnv();
    await signIn(page, env, '/work');
    const task = await dispatchWaiting(page, env, 'Write the migration guide\n- cover the new config keys\n- link the changelog');
    try {
      const board = page.getByTestId('work-board');
      await expect(board.locator('[data-column]')).toHaveText([/Queued/, /Running/, /Needs you/, /Review/, /Done today/]);
      await expect(board.locator('[data-lane="Expedite"]')).toBeVisible();
      const card = board.locator(`[data-card="${task.sid}"]`);
      await expect(card).toContainText('Write the migration guide');
      await card.click();
      await expect(page).toHaveURL(new RegExp(`/work/${task.sid}$`));
      const panel = page.locator('aside[aria-label="Review"]').getByTestId('review-panel');
      await expect(panel).toContainText('Write the migration guide');
      await expect(panel).toContainText('cover the new config keys');
      await expect(panel).toContainText('Receipts vs acceptance');

      // Expedite moves the card into the Expedite lane.
      await panel.getByTestId('toggle-expedite').click();
      await expect(board.locator('[data-lane="Expedite"]').locator(`[data-card="${task.sid}"]`)).toBeVisible();
      await panel.getByTestId('toggle-expedite').click();

      await page.getByTestId('view-list').click();
      await expect(page.getByTestId('work-list').locator(`[data-row="${task.sid}"]`)).toBeVisible();
      await page.keyboard.press('ControlOrMeta+b');
      await expect(page.getByTestId('work-board')).toBeVisible();
    } finally {
      await cancel(page, task.id);
    }
  });

  test('Work: a finished change waits in Review; Approve moves it to Done today', async ({ page }) => {
    const env = loadEnv();
    await signIn(page, env, '/work');
    // A git checkout for the agent to change (the mock touches a file and leaves it uncommitted).
    const repo = fs.mkdtempSync(path.join(os.tmpdir(), 'konsert-e2e-review-'));
    try {
      const git = (...args: string[]) => execFileSync('git', ['-C', repo, ...args], { stdio: 'ignore' });
      git('init', '-q');
      fs.writeFileSync(path.join(repo, 'README.md'), '# demo\n');
      git('add', 'README.md');
      git('-c', 'user.email=e2e@users.noreply.github.com', '-c', 'user.name=e2e', 'commit', '-qm', 'init');
      const res = await api<{ task_id: string }>(page, 'POST', '/api/tasks', {
        worker_selector: { worker_id: env.workerId },
        agent: 'mock',
        repo_path: repo,
        prompt: 'Add the changelog entry\n- mention the new modes\nTOUCH_FILE=CHANGELOG.md',
        mode: 'oneshot',
        origin: 'orchestrator',
      });
      let sid = '';
      await expect
        .poll(async () => {
          const t = await api<{ task_sid: string; status: string }>(page, 'GET', `/api/tasks/${res.task_id}`);
          sid = t.task_sid;
          return t.status;
        })
        .toBe('completed');
      const board = page.getByTestId('work-board');
      const card = board.locator(`[data-card="${sid}"]`);
      const column = () => card.evaluate((el) => el.closest('[role="cell"]')?.getAttribute('data-cell') ?? '');
      await expect.poll(column).toBe('review');
      await card.click();
      const panel = page.locator('aside[aria-label="Review"]').getByTestId('review-panel');
      await expect(panel).toContainText('mention the new modes');
      await expect(panel.getByTestId('review-receipts')).toContainText('CHANGELOG.md');
      await expect(rail(page)).toHaveCount(0);
      await panel.getByTestId('review-approve').click();
      await expect.poll(column).toBe('done');
      await expect(panel.getByTestId('review-message')).toContainText('Approved');
    } finally {
      fs.rmSync(repo, { recursive: true, force: true });
    }
  });

  test('Fleet: machines and the timeline with its range picker (12h default)', async ({ page }) => {
    const env = loadEnv();
    await signIn(page, env, '/fleet');
    const machines = page.getByTestId('fleet-machines');
    await expect(machines.locator('tr', { hasText: env.workerName })).toBeVisible();
    const timeline = page.getByTestId('fleet-timeline');
    await expect(timeline.getByRole('heading')).toHaveText('Last 12 hours');
    await expect(timeline.getByRole('radio', { name: '12h' })).toHaveAttribute('aria-checked', 'true');
    for (const [id, title] of [
      ['24h', '24 hours'],
      ['3d', '3 days'],
      ['1w', 'week'],
    ]) {
      await timeline.getByRole('radio', { name: id }).click();
      await expect(timeline.getByRole('radio', { name: id })).toHaveAttribute('aria-checked', 'true');
      await expect(timeline.getByRole('heading')).toHaveText(`Last ${title}`);
    }
    await expect(timeline.locator(`[data-timeline="${env.workerName}"]`)).toBeVisible();
  });

  test('Wall: tiles stream live sessions, layouts and pins; tiles stay where they are', async ({ page }) => {
    const env = loadEnv();
    await signIn(page, env);
    const a = await dispatchWaiting(page, env, 'Wall tile A');
    const b = await dispatchWaiting(page, env, 'Wall tile B');
    try {
      await page.goto(`${env.baseURL}/wall`);
      const grid = page.getByTestId('wall-grid');
      await expect(grid.locator(`[data-tile="${a.sid}"]`)).toBeVisible();
      await expect(grid.locator(`[data-tile="${b.sid}"]`)).toBeVisible();
      // The terminal streams into the tile.
      await expect(grid.locator(`[data-tile="${a.sid}"] .xterm-rows`)).toContainText('Ready for interactive input');

      await page.getByTestId(`pin-${b.sid}`).click();
      await page.getByTestId(`pin-${a.sid}`).click();
      await expect(page.getByTestId('wall-summary')).toContainText('pinned 2');
      const order = async () => grid.locator('[data-tile]').evaluateAll((els) => els.map((e) => e.getAttribute('data-tile')));
      const before = await order();
      expect(before.indexOf(b.sid)).toBeLessThan(before.indexOf(a.sid));

      // Tiles don't move when a session changes state.
      await api(page, 'POST', '/api/holds', { task_id: a.id });
      await expect(grid.locator(`[data-tile="${a.sid}"] [data-control="HELD"]`)).toBeVisible();
      expect(await order()).toEqual(before);

      // Moving is up to the user.
      await grid.getByRole('button', { name: `Move ${a.sid} earlier` }).click();
      const after = await order();
      expect(after.indexOf(a.sid)).toBeLessThan(after.indexOf(b.sid));

      // Layouts: 1×2 shows two tiles per page and pages the rest.
      await page.locator('[data-layout="1x2"]').click();
      await expect(page.locator('[data-layout="1x2"]')).toHaveAttribute('aria-pressed', 'true');
      await expect(grid.locator('[data-tile]')).toHaveCount(2);
      await page.locator('[data-layout="3x3"]').click();
      await expect(page.getByTestId('wall-summary')).toContainText('9 per page');

      // Take control from a tile.
      const take = page.getByTestId(`take-control-${b.sid}`);
      await take.click();
      await expect(take).toHaveAttribute('aria-pressed', 'true');
      await take.click();

      await grid.getByTestId(`remove-${a.sid}`).click();
      await grid.getByTestId(`remove-${b.sid}`).click();
      await expect(page.getByTestId('wall-summary')).toContainText('pinned 0');
    } finally {
      await cancel(page, a.id);
      await cancel(page, b.id);
    }
  });

  test('Pause all is quiet and asks first (Cancel is the default); Resume needs no confirmation', async ({ page }) => {
    const env = loadEnv();
    await signIn(page, env, '/work');
    await page.getByTestId('pause-everything').click();
    const dialog = page.getByRole('dialog', { name: 'Pause everything?' });
    await expect(dialog).toBeVisible();
    await expect(dialog.getByTestId('pause-cancel')).toBeFocused();
    await dialog.getByTestId('pause-cancel').click();
    await expect(page.getByTestId('fleet-state')).toHaveText('FLEET: RUNNING');

    await page.getByTestId('pause-everything').click();
    await page.getByTestId('pause-confirm').click();
    await expect(page.getByTestId('fleet-state')).toHaveText(/FLEET: PAUSED/);
    await page.getByTestId('resume-everything').click();
    await expect(page.getByTestId('fleet-state')).toHaveText('FLEET: RUNNING');
  });
});

test.describe('Mode UI (phone)', () => {
  test.skip(({ isMobile }) => !isMobile, 'phone inbox');

  test('lands in the needs-you inbox; session detail has big controls; fleet is read-only', async ({ page }) => {
    const env = loadEnv();
    await signIn(page, env);
    await expect(page).toHaveURL(/\/inbox$/);
    await expect(page.getByTestId('mobile-banner')).toBeVisible();
    const task = await dispatchWaiting(page, env, 'Phone check task');
    try {
      await page.getByTestId('mobile-working').click();
      await page.locator(`[data-work-item="${task.sid}"]`).click();
      await expect(page).toHaveURL(new RegExp(`/inbox/${task.sid}$`));
      await expect(page.getByRole('heading', { level: 1 })).toContainText('Phone check task');
      const hold = page.getByTestId('mobile-hold');
      const box = await hold.boundingBox();
      expect(box!.height).toBeGreaterThanOrEqual(44);
      await hold.click();
      await expect(page.getByTestId('mobile-release')).toBeVisible();
      await expect(page.getByTestId('mobile-receipts')).toContainText('held task');
      await page.getByTestId('mobile-release').click();
      await expect(page.getByTestId('mobile-hold')).toBeVisible();

      await page.getByTestId('mobile-veto').click();
      const sheet = page.getByRole('dialog', { name: 'Veto: why?' });
      await sheet.getByRole('radio', { name: 'Not now' }).click();
      await sheet.getByTestId('mobile-veto-confirm').click();
      await expect(sheet).toBeHidden();

      await page.getByTestId('mobile-back').click();
      await expect(page).toHaveURL(/\/inbox$/);
      await page.getByTestId('mobile-fleet-link').click();
      await expect(page.getByTestId('mobile-fleet').locator(`[data-machine="${env.workerName}"]`)).toBeVisible();
    } finally {
      await cancel(page, task.id);
    }
  });

  test('the emergency pause is a quiet link behind a confirm sheet with Cancel first', async ({ page }) => {
    const env = loadEnv();
    await signIn(page, env);
    const link = page.getByTestId('mobile-pause');
    await expect(link).toHaveText(/Emergency: pause everything/);
    expect((await link.boundingBox())!.height).toBeGreaterThanOrEqual(44);
    await link.click();
    const sheet = page.getByRole('dialog', { name: 'Pause everything?' });
    await expect(sheet.getByTestId('mobile-pause-cancel')).toBeFocused();
    await sheet.getByTestId('mobile-pause-cancel').click();
    await expect(sheet).toBeHidden();
    await expect(page.getByTestId('mobile-fleet-state')).toHaveText('FLEET: RUNNING');
  });

  test('a question opens the phone questions flow from the inbox', async ({ page }) => {
    const env = loadEnv();
    await signIn(page, env, '/command');
    const sid = await askFromOrchestrator(page);
    await page.goto(`${env.baseURL}/inbox`);
    await page.locator(`[data-testid="mobile-needs-you"] [data-question="${sid}"]`).click();
    const panel = page.getByTestId('questions-panel');
    await expect(panel).toBeVisible();
    const card = panel.getByTestId('questions-detail').getByTestId('question-card');
    await expect(card).toHaveAttribute('data-question', sid);
    await card.getByTestId('question-option-option_1').click();
    await expect(card.getByTestId('question-receipt')).toContainText('Sent “Yes”');
    await panel.getByTestId('questions-close').click();
    await expect(page.locator(`[data-testid="mobile-needs-you"] [data-question="${sid}"]`)).toHaveCount(0);
  });
});

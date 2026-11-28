import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { expect, Locator, Page, test } from '@playwright/test';
import { E2E_STATE_FILE, resumeFleetIfPaused } from './global-setup';

/**
 * The mode UI (SYR-88NN9): users land in Command on desktop and in the needs-you inbox on a
 * phone. Command has one Needs-you area (the right rail); Work is the board with the review
 * panel; Fleet has the machine rows with their heat strips and the queue; Wall tiles stream and stay put.
 * Runs against the suite's hub with the mock worker.
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  workerId: string;
  workerName: string;
  workerSid: string;
  workdir: string;
}

const QUESTION = 'Accept the shortened exit criteria on SYR-2Q0CH?';

function loadEnv(): E2EEnv {
  return JSON.parse(fs.readFileSync(E2E_STATE_FILE, 'utf8')) as E2EEnv;
}

async function signIn(page: Page, env: E2EEnv, path = '/'): Promise<void> {
  await page.goto(`${env.baseURL}${path}`);
  // Signed out, the login page offers pairing; the recovery key is the fallback.
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

/** Dispatches a mock task that waits for input (so it stays live) and returns its sid. */
async function dispatchWaiting(
  page: Page,
  env: E2EEnv,
  title: string,
): Promise<{ id: string; sid: string }> {
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
      const t = await api<{ task_sid: string; status: string }>(
        page,
        'GET',
        `/api/tasks/${res.task_id}`,
      );
      sid = t.task_sid;
      return t.status;
    })
    .toBe('waiting_input');
  return { id: res.task_id, sid };
}

async function cancel(page: Page, id: string): Promise<void> {
  await api(page, 'POST', `/api/tasks/${id}/cancel`).catch(() => undefined);
}

/** Starts a mock agent chat in the commander tab and has it ask the canned question. */
async function askFromCommander(page: Page): Promise<string> {
  const chat = page.locator('app-chat-view');
  await chat.getByTestId('new-chat').click();
  await chat.getByTestId('account-chip').click();
  await page.getByRole('menu', { name: 'Agent account' }).locator('[data-account="mock2"]').click();
  await chat.getByTestId('composer-input').fill('question');
  await chat.getByTestId('send-button').click();
  let sid = '';
  await expect
    .poll(async () => {
      const res = await api<{
        questions: Array<{ sid: string; prompt: string; status: string; created_at: string }>;
      }>(page, 'GET', '/api/questions');
      const q = res.questions
        .filter((x) => x.prompt === QUESTION && x.status === 'open')
        .sort((a, b) => b.created_at.localeCompare(a.created_at))[0];
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
  test.afterEach(async () => {
    const env = loadEnv();
    await resumeFleetIfPaused(env.baseURL, env.adminToken);
  });
  test.skip(({ isMobile }) => isMobile, 'desktop modes');

  test('lands in Command with the mode header; keys 1–4 switch modes and the URL follows', async ({
    page,
  }) => {
    const env = loadEnv();
    await signIn(page, env);
    await expect(page).toHaveURL(/\/command$/);
    const nav = page.getByRole('navigation', { name: 'Modes' });
    await expect(nav.getByRole('link')).toHaveText([
      /1\s*Command/,
      /2\s*Work/,
      /3\s*Fleet/,
      /4\s*Wall/,
    ]);
    await expect(nav.locator('[data-mode="command"]')).toHaveAttribute('aria-current', 'page');
    await expect(page.getByTestId('fleet-state')).toHaveText('FLEET: RUNNING');
    await expect(page.getByTestId('pause-everything')).toBeVisible();

    // The commander tab is pinned: no close button.
    const tabs = page.getByTestId('command-tabs');
    await expect(tabs.getByRole('tab', { name: 'Commander' })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    await expect(tabs.locator('[data-tab="commander"] [aria-label^="Close"]')).toHaveCount(0);

    await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
    for (const [key, mode] of [
      ['2', 'work'],
      ['3', 'fleet'],
      ['4', 'wall'],
      ['1', 'command'],
    ]) {
      await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
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

  test('Command: a session opens as a closable tab with GOAL/NOW and controls; holds land in the rail', async ({
    page,
  }) => {
    const env = loadEnv();
    await signIn(page, env);
    const task = await dispatchWaiting(page, env, 'Tidy the release notes');
    try {
      // Live work is the project story: the line sits in its project's section with the
      // spinner; a task waiting for input is asking and offers "→ Needs you".
      const live = page.locator('aside[aria-label="Live work"]');
      const row = live.locator(`[data-project] [data-work-item="${task.sid}"]`);
      await expect(row).toContainText('Tidy the release notes');
      await expect(row).toHaveAttribute('data-state', 'asking');
      await expect(row.locator('sy-spinner[data-spinner="asking"]')).toBeVisible();
      await expect(row.getByTestId('line-needs-you')).toBeVisible();
      await row.locator('button.sy-lw-open').click();
      await expect(page).toHaveURL(new RegExp(`/command/${task.sid}$`));

      const tab = page.getByTestId('command-tabs').locator(`[data-tab="${task.sid}"]`);
      await expect(tab.getByRole('tab')).toHaveAttribute('aria-selected', 'true');
      const strip = page.getByTestId('session-strip');
      await expect(strip).toContainText('GOAL');
      await expect(strip).toContainText('Tidy the release notes');
      await expect(strip).toContainText(/Waiting for (your )?input/);
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
      await expect(
        strip.locator('[data-control="HELD"], [data-control="ASKING"]').first(),
      ).toBeVisible();
      await strip.getByTestId('session-release-hold').click();
      await expect(rail(page).locator('[data-hold]').filter({ hasText: task.sid })).toHaveCount(0);

      // Deep link survives a reload, then the tab closes back to the commander.
      await page.reload();
      await expect(page.getByTestId('session-strip')).toContainText('Tidy the release notes');
      await page.getByTestId(`close-tab-${task.sid}`).click();
      await expect(page).toHaveURL(/\/command$/);
      await expect(
        page.getByTestId('command-tabs').locator(`[data-tab="${task.sid}"]`),
      ).toHaveCount(0);
    } finally {
      await cancel(page, task.id);
    }
  });

  test('Needs you: every item opens the one dialog; it walks the queue and answering moves on', async ({
    page,
  }) => {
    const env = loadEnv();
    await signIn(page, env);
    const sid = await askFromCommander(page);

    // The rail lists it as the shared compact item; the left pane carries none.
    const item = rail(page).locator(`sy-needs-you-item [data-question="${sid}"]`);
    await expect(item).toContainText(QUESTION);
    await expect(page.locator('aside[aria-label="Live work"] [data-needs-you]')).toHaveCount(0);

    // The rail item opens the dialog on that question, with its position in the queue.
    await item.click();
    const dialog = page.getByTestId('questions-panel');
    await expect(dialog).toBeVisible();
    const card = dialog.getByTestId('questions-detail').getByTestId('question-card');
    await expect(card).toHaveAttribute('data-question', sid);
    await expect(dialog.getByTestId('needs-you-position')).toHaveText(/\d+ of \d+/);
    await expect(dialog.locator(`[data-queue-item="${sid}"]`)).toHaveAttribute(
      'aria-current',
      'step',
    );
    // Esc closes and focus goes back to the item we came from.
    await page.keyboard.press('Escape');
    await expect(dialog).toBeHidden();
    await expect(item).toBeFocused();

    // The card in the asking chat stays; its Ask about this opens the same dialog with the side conversation.
    const chat = page.locator('app-chat-view');
    await chat
      .getByTestId('question-card')
      .filter({ hasText: QUESTION })
      .getByTestId('question-ask')
      .click();
    await expect(dialog.getByTestId('side-conversation')).toBeVisible();
    await expect(
      dialog.getByTestId('questions-detail').getByTestId('question-card'),
    ).toHaveAttribute('data-question', sid);
    await page.keyboard.press('Escape'); // closes the side conversation first
    await expect(dialog.getByTestId('side-conversation')).toHaveCount(0);

    // The header count opens it too; answering moves on (or says All caught up and closes).
    await page.getByTestId('questions-close').click();
    await page.getByTestId('needs-you-count').click();
    await dialog.locator(`[data-queue-item="${sid}"]`).click();
    await dialog.getByTestId('questions-detail').getByTestId('question-option-option_2').click();
    await expect(
      dialog.getByTestId('questions-detail').locator(`[data-question="${sid}"]`),
    ).toHaveCount(0, { timeout: 5000 });
    await expect(
      chat
        .getByTestId('assistant-message')
        .filter({ hasText: 'The user chose: "No, keep the strict exit criteria"' }),
    ).toBeVisible();
    if (await dialog.isVisible()) await page.keyboard.press('Escape');
  });

  test('Work: board with lanes and columns, review panel, board/list toggle', async ({ page }) => {
    const env = loadEnv();
    await signIn(page, env, '/work');
    const task = await dispatchWaiting(
      page,
      env,
      'Write the migration guide\n- cover the new config keys\n- link the changelog',
    );
    try {
      const board = page.getByTestId('work-board');
      await expect(board.locator('[data-column]')).toHaveText([
        /Queued/,
        /Running/,
        /Needs you/,
        /Review/,
        /Done/,
      ]);
      await expect(board.locator('[data-lane="Expedite"]')).toBeVisible();
      const card = board.locator(`[data-card="${task.sid}"]`);
      await expect(card).toContainText('Write the migration guide');
      await card.click();
      await expect(page).toHaveURL(new RegExp(`/work/${task.sid}$`));
      const panel = page.locator('aside[aria-label="Review"]').getByTestId('review-panel');
      await expect(panel).toContainText('Write the migration guide');
      await expect(panel).toContainText('cover the new config keys');
      await expect(panel).toContainText('Receipts vs acceptance');
      // Acceptance comes from the hub: the prompt's bullets (no ka ticket linked).
      await expect(panel.getByTestId('acceptance-source')).toHaveText(
        'Asked for (from the prompt)',
      );
      await expect(panel.getByTestId('acceptance').locator('li')).toHaveCount(2);

      // Expedite moves the card into the Expedite lane, and it is kept on the hub: it
      // survives a reload (and any other client sees it).
      await panel.getByTestId('toggle-expedite').click();
      await expect(
        board.locator('[data-lane="Expedite"]').locator(`[data-card="${task.sid}"]`),
      ).toBeVisible();
      const reviews = await api<{
        reviews: Array<{ task_id?: string; expedite: boolean; sid: string }>;
      }>(page, 'GET', '/api/reviews');
      expect(reviews.reviews.find((r) => r.task_id === task.id)).toMatchObject({
        expedite: true,
        sid: expect.stringMatching(/^rv-/),
      });
      await page.reload();
      await expect(
        board.locator('[data-lane="Expedite"]').locator(`[data-card="${task.sid}"]`),
      ).toBeVisible();
      await expect(panel.getByTestId('toggle-expedite')).toHaveText(/Remove from Expedite/);
      await expect(panel.getByTestId('toggle-expedite')).toHaveAttribute('aria-pressed', 'true');
      await panel.getByTestId('toggle-expedite').click();
      await expect(
        board.locator('[data-lane="Expedite"]').locator(`[data-card="${task.sid}"]`),
      ).toHaveCount(0);
      expect(
        await page.evaluate(() =>
          Object.keys(localStorage).filter((k) => k.startsWith('syre.review')),
        ),
      ).toEqual([]);

      await page.getByTestId('view-list').click();
      await expect(page.getByTestId('work-list').locator(`[data-row="${task.sid}"]`)).toBeVisible();
      await page.keyboard.press('ControlOrMeta+b');
      await expect(page.getByTestId('work-board')).toBeVisible();
    } finally {
      await cancel(page, task.id);
    }
  });

  test('Work: a finished change waits in Review; Approve moves it to Done today', async ({
    page,
  }) => {
    const env = loadEnv();
    await signIn(page, env, '/work');
    // A git checkout for the agent to change (the mock touches a file and leaves it uncommitted).
    const repo = fs.mkdtempSync(path.join('/var/tmp', 'syre-e2e-review-'));
    try {
      const git = (...args: string[]) =>
        execFileSync('git', ['-C', repo, ...args], { stdio: 'ignore' });
      git('init', '-q', '-b', 'main');
      git('config', 'user.email', 'e2e@example.invalid');
      git('config', 'user.name', 'e2e');
      fs.writeFileSync(path.join(repo, 'README.md'), '# demo\n');
      git('add', 'README.md');
      git('commit', '-qm', 'init');
      const res = await api<{ task_id: string }>(page, 'POST', '/api/tasks', {
        worker_selector: { worker_id: env.workerId },
        agent: 'mock',
        repo_path: repo,
        use_worktree: true,
        branch: 'syre/e2e-review',
        prompt: 'Add the changelog entry\n- mention the new modes\nTOUCH_FILE=CHANGELOG.md',
        mode: 'oneshot',
        origin: 'commander',
      });
      let sid = '';
      await expect
        .poll(async () => {
          const t = await api<{ task_sid: string; status: string }>(
            page,
            'GET',
            `/api/tasks/${res.task_id}`,
          );
          sid = t.task_sid;
          return t.status;
        })
        .toBe('completed');
      const board = page.getByTestId('work-board');
      const card = board.locator(`[data-card="${sid}"]`);
      const column = () =>
        card.evaluate((el) => el.closest('[role="cell"]')?.getAttribute('data-cell') ?? '');
      await expect.poll(column).toBe('review');
      // A review is a needs-you item: the card opens the shared dialog with the review body.
      await card.click();
      const dialog = page.getByTestId('questions-panel');
      const panel = dialog.getByTestId('needs-you-review').getByTestId('review-panel');
      await expect(panel).toContainText('mention the new modes');
      await expect(panel.getByTestId('review-receipts')).toContainText('CHANGELOG.md');
      // No remote: defaults to direct local-only merge.
      await expect(panel.getByTestId('review-delivery-action')).toContainText('local only');
      await panel.getByTestId('review-approve').click();
      await expect(panel.getByTestId('review-message')).toContainText('Approved');
      await expect(panel.getByTestId('review-message')).toContainText('local only');
      await expect.poll(column).toBe('done');
      if (await dialog.isVisible()) await page.keyboard.press('Escape');

      // The decision and delivery outcome are kept on the hub across a reload.
      await page.reload();
      await expect.poll(column).toBe('done');
      await card.click();
      const aside = page.locator('aside[aria-label="Review"]').getByTestId('review-panel');
      await expect(aside.getByTestId('review-decision')).toContainText('Approved');
      await expect(aside.getByTestId('review-decision')).toContainText('local only');
      await expect(aside.getByTestId('review-approve')).toHaveCount(0);
    } finally {
      fs.rmSync(repo, { recursive: true, force: true });
    }
  });

  test('Work: Approve in direct mode pushes to a local bare remote; pr_merge without forge shows compare URL text', async ({
    page,
  }) => {
    const env = loadEnv();
    await signIn(page, env, '/work');
    const base = fs.mkdtempSync(path.join('/var/tmp', 'syre-e2e-delivery-'));
    const bare = path.join(base, 'origin.git');
    const repoDirect = path.join(base, 'repo-direct');
    const repoPR = path.join(base, 'repo-pr');
    let repoDirectId = '';
    let repoPRId = '';
    try {
      fs.mkdirSync(bare, { recursive: true });
      fs.mkdirSync(repoDirect, { recursive: true });
      fs.mkdirSync(repoPR, { recursive: true });
      const gitIn = (dir: string, ...args: string[]) =>
        execFileSync('git', ['-C', dir, ...args], { encoding: 'utf8' }).trim();

      gitIn(bare, 'init', '--bare', '-b', 'main');
      gitIn(repoDirect, 'init', '-b', 'main');
      gitIn(repoDirect, 'config', 'user.email', 'e2e@example.invalid');
      gitIn(repoDirect, 'config', 'user.name', 'e2e');
      fs.writeFileSync(path.join(repoDirect, 'README.md'), '# direct\n');
      gitIn(repoDirect, 'add', 'README.md');
      gitIn(repoDirect, 'commit', '-qm', 'init');
      gitIn(repoDirect, 'remote', 'add', 'origin', bare);
      gitIn(repoDirect, 'push', '-u', 'origin', 'main');

      const createdDirect = await api<{ id: string }>(page, 'POST', '/api/projects/p-00000/repos', {
        name: 'e2e-direct',
        remote_url: bare,
        default_branch: 'main',
        delivery_mode: 'direct',
        merge_style: 'squash',
      });
      repoDirectId = createdDirect.id;

      const directTask = await api<{ task_id: string }>(page, 'POST', '/api/tasks', {
        worker_selector: { worker_id: env.workerId },
        repo_id: repoDirectId,
        agent: 'mock',
        repo_path: repoDirect,
        use_worktree: true,
        branch: 'syre/e2e-direct',
        prompt: 'Direct delivery test\nTOUCH_FILE=DIRECT.md',
        mode: 'oneshot',
        origin: 'commander',
      });
      let directSid = '';
      await expect
        .poll(async () => {
          const t = await api<{ task_sid: string; status: string }>(
            page,
            'GET',
            `/api/tasks/${directTask.task_id}`,
          );
          directSid = t.task_sid;
          return t.status;
        })
        .toBe('completed');

      const board = page.getByTestId('work-board');
      const directCard = board.locator(`[data-card="${directSid}"]`);
      await expect(directCard).toBeVisible();
      await directCard.click();
      const dialog = page.getByTestId('questions-panel');
      const panel = dialog.getByTestId('needs-you-review').getByTestId('review-panel');
      await expect(panel.getByTestId('review-delivery-action')).toContainText(
        'Approve → squash-merge into main and push',
      );
      await panel.getByTestId('review-approve').click();
      await expect(panel.getByTestId('review-message')).toContainText(
        'pushed to origin/main; your local main is behind — pull when ready',
      );
      if (await dialog.isVisible()) await page.keyboard.press('Escape');

      // Confirm the file actually landed on the bare remote's main branch,
      // while the user's primary checkout on main was left untouched.
      expect(gitIn(bare, 'ls-tree', '-r', '--name-only', 'main')).toContain('DIRECT.md');
      expect(fs.existsSync(path.join(repoDirect, 'DIRECT.md'))).toBe(false);

      // Now verify the panel text in pr_merge mode with no forge credential.
      gitIn(repoPR, 'init', '-b', 'main');
      gitIn(repoPR, 'config', 'user.email', 'e2e@example.invalid');
      gitIn(repoPR, 'config', 'user.name', 'e2e');
      fs.writeFileSync(path.join(repoPR, 'README.md'), '# pr\n');
      gitIn(repoPR, 'add', 'README.md');
      gitIn(repoPR, 'commit', '-qm', 'init');
      gitIn(repoPR, 'remote', 'add', 'origin', bare);

      const createdPR = await api<{ id: string }>(page, 'POST', '/api/projects/p-00000/repos', {
        name: 'e2e-pr-merge',
        remote_url: `${bare}#pr`,
        default_branch: 'main',
        delivery_mode: 'pr_merge',
        merge_style: 'squash',
      });
      repoPRId = createdPR.id;

      const prTask = await api<{ task_id: string }>(page, 'POST', '/api/tasks', {
        worker_selector: { worker_id: env.workerId },
        repo_id: repoPRId,
        agent: 'mock',
        repo_path: repoPR,
        use_worktree: true,
        branch: 'syre/e2e-pr',
        prompt: 'PR merge panel test\nTOUCH_FILE=PRMERGE.md',
        mode: 'oneshot',
        origin: 'commander',
      });
      let prSid = '';
      await expect
        .poll(async () => {
          const t = await api<{ task_sid: string; status: string }>(
            page,
            'GET',
            `/api/tasks/${prTask.task_id}`,
          );
          prSid = t.task_sid;
          return t.status;
        })
        .toBe('completed');

      const prCard = board.locator(`[data-card="${prSid}"]`);
      await expect(prCard).toBeVisible();
      await prCard.click();
      const prPanel = dialog.getByTestId('needs-you-review').getByTestId('review-panel');
      await expect(prPanel.getByTestId('review-delivery-action')).toContainText(
        'Approve → push branch and show compare URL (no forge credential)',
      );
      if (await dialog.isVisible()) await page.keyboard.press('Escape');
    } finally {
      if (repoDirectId)
        await api(page, 'DELETE', `/api/projects/p-00000/repos/${repoDirectId}`).catch(
          () => undefined,
        );
      if (repoPRId)
        await api(page, 'DELETE', `/api/projects/p-00000/repos/${repoPRId}`).catch(() => undefined);
      fs.rmSync(base, { recursive: true, force: true });
    }
  });

  test('Fleet: machine rows with sessions, heat strips over the range picker (12h default), the queue', async ({
    page,
  }) => {
    const env = loadEnv();
    await signIn(page, env, '/fleet');
    const task = await dispatchWaiting(page, env, 'Fleet row session');
    try {
      const row = page.getByTestId('fleet-machines').locator(`[data-machine="${env.workerSid}"]`);
      await expect(row.getByRole('heading', { name: env.workerName })).toBeVisible();
      await expect(row.locator('.sy-badge').first()).toHaveText(/^(OK|IDLE|FULL)$/);
      await expect(row.getByTestId('machine-heat')).not.toBeEmpty();
      for (const name of ['Drain', 'Suspend', 'Details']) {
        await expect(row.getByRole('button', { name, exact: true })).toBeVisible();
      }
      // The session runs on its machine's row as a card with the spinner.
      const card = row.locator(`[data-session="${task.sid}"]`);
      await expect(card).toContainText('Fleet row session');
      await expect(card.locator('sy-spinner[data-spinner]')).toBeVisible();

      // The heat strip follows the range picker.
      const strip = row.getByTestId('heat-strip');
      await expect(strip).toHaveAttribute('aria-label', /last 12 hours/);
      const ranges = page.getByRole('radiogroup', { name: 'Heat over the last' });
      await expect(ranges.locator('[data-range="12h"]')).toHaveAttribute('aria-checked', 'true');
      for (const [id, title] of [
        ['24h', '24 hours'],
        ['3d', '3 days'],
        ['1w', 'week'],
      ]) {
        await ranges.locator(`[data-range="${id}"]`).click();
        await expect(ranges.locator(`[data-range="${id}"]`)).toHaveAttribute(
          'aria-checked',
          'true',
        );
        await expect(ranges.locator('[aria-checked="true"]')).toHaveCount(1);
        await expect(strip).toHaveAttribute('aria-label', new RegExp(`last ${title}`));
        // 24h reads hourly roll-ups: one cell per hour.
        if (id === '24h') await expect(strip.locator('span')).toHaveCount(24);
      }
      // Retention is stated in plain words on the range label.
      await page.getByTestId('heat-retention').hover();
      await expect(page.getByRole('tooltip')).toContainText(
        'every sample for 12 hours, hourly summaries for 30 days',
      );

      // Details asks the machine for its listening ports (not in heartbeats any more); the
      // hub itself listens on this machine, so there is at least one.
      await row.getByTestId(`machine-details-${env.workerSid}`).click();
      await expect(page.getByTestId('machine-ports')).toBeVisible();
      await expect(page.getByTestId('machine-ports')).toContainText('live');
      await page.keyboard.press('Escape');

      await expect(page.getByTestId('fleet-queue')).toContainText('Queue');
      // The session card opens the session in Command.
      await card.click();
      await expect(page).toHaveURL(new RegExp(`/command/${task.sid}$`));
    } finally {
      await cancel(page, task.id);
    }
  });

  test('Wall: tiles stream live sessions, layouts and pins; tiles stay where they are', async ({
    page,
  }) => {
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
      await expect(grid.locator(`[data-tile="${a.sid}"] .xterm`)).toBeVisible();

      await page.getByTestId(`pin-${b.sid}`).click();
      await page.getByTestId(`pin-${a.sid}`).click();
      await expect(page.getByTestId('wall-summary')).toContainText('pinned 2');
      const order = async () =>
        grid
          .locator('[data-tile]')
          .evaluateAll((els) => els.map((e) => e.getAttribute('data-tile')));
      const before = await order();
      expect(before.indexOf(b.sid)).toBeLessThan(before.indexOf(a.sid));

      // Tiles don't move when a session changes state.
      await api(page, 'POST', '/api/holds', { task_id: a.id });
      await expect(grid.locator(`[data-tile="${a.sid}"] [data-control="HELD"]`)).toBeVisible();
      expect(await order()).toEqual(before);

      // Moving is up to the user.
      await grid.getByRole('button', { name: `Move ${a.sid} earlier` }).click();
      await expect
        .poll(async () => {
          const after = await order();
          return after.indexOf(a.sid) < after.indexOf(b.sid);
        })
        .toBe(true);

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

  test('Pause all is quiet and asks first (Cancel is the default); Resume needs no confirmation', async ({
    page,
  }) => {
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
  test.afterEach(async () => {
    const env = loadEnv();
    await resumeFleetIfPaused(env.baseURL, env.adminToken);
  });
  test.skip(({ isMobile }) => !isMobile, 'phone inbox');

  test('lands in the needs-you inbox; session detail has big controls; fleet is read-only', async ({
    page,
  }) => {
    const env = loadEnv();
    await signIn(page, env);
    await expect(page).toHaveURL(/\/inbox$/);
    await expect(page.getByTestId('mobile-banner')).toBeVisible();
    const task = await dispatchWaiting(page, env, 'Phone check task');
    try {
      await page.getByTestId('mobile-working').click();
      const line = page.locator(`[data-work-item="${task.sid}"]`);
      await expect(line.locator('sy-spinner[data-spinner]')).toBeVisible();
      await line.click();
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
      await expect(
        page.getByTestId('mobile-fleet').locator(`[data-machine="${env.workerName}"]`),
      ).toBeVisible();
    } finally {
      await cancel(page, task.id);
    }
  });

  test('the emergency pause is a quiet link behind a confirm sheet with Cancel first', async ({
    page,
  }) => {
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

  test('nothing floats over the terminal quick keys after Take control (SYR-WKJM6)', async ({
    page,
  }) => {
    const env = loadEnv();
    await signIn(page, env);
    const task = await dispatchWaiting(page, env, 'Phone quick keys task');
    try {
      await page.goto(`${env.baseURL}/command/${task.sid}`);
      await page.getByTestId('take-control').click();
      for (const name of ['Ctrl+C', 'Esc', 'Tab', 'Enter']) {
        const key = page.getByRole('button', { name, exact: true });
        await expect(key).toBeInViewport({ ratio: 1 });
        // The key itself must be what a tap at its centre hits.
        const topmost = await key.evaluate((el) => {
          const r = el.getBoundingClientRect();
          const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
          return hit === el || el.contains(hit);
        });
        expect(topmost, `${name} key is covered`).toBe(true);
      }
    } finally {
      await cancel(page, task.id);
    }
  });

  test('a needs-you item opens the dialog as a full-screen sheet; answering moves on', async ({
    page,
  }) => {
    const env = loadEnv();
    await signIn(page, env, '/command');
    const sid = await askFromCommander(page);
    await page.goto(`${env.baseURL}/inbox`);
    const item = page.locator(`[data-testid="mobile-needs-you"] [data-question="${sid}"]`);
    expect((await item.boundingBox())!.height).toBeGreaterThanOrEqual(44);
    await item.click();
    const panel = page.getByTestId('questions-panel');
    await expect(panel).toBeInViewport();
    // A full-screen sheet once its entrance animation settles.
    await expect.poll(async () => (await panel.boundingBox())!.width).toBeGreaterThanOrEqual(389);
    await expect.poll(async () => (await panel.boundingBox())!.height).toBeGreaterThanOrEqual(840);
    const card = panel.getByTestId('questions-detail').getByTestId('question-card');
    await expect(card).toHaveAttribute('data-question', sid);
    await card.getByTestId('question-option-option_1').click();
    await expect(
      panel.getByTestId('questions-detail').locator(`[data-question="${sid}"]`),
    ).toHaveCount(0, { timeout: 5000 });
    if (await panel.isVisible())
      await panel
        .getByTestId('questions-close')
        .click()
        .catch(() => undefined);
    await expect(
      page.locator(`[data-testid="mobile-needs-you"] [data-question="${sid}"]`),
    ).toHaveCount(0);
  });
});

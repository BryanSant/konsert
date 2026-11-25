import fs from 'node:fs';
import { expect, Locator, Page, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * Subscription-only acceptance flow (SYR-8Y5PS): chat with a logged-in agent CLI account and
 * no API key anywhere. syred runs with --enable-mock-driver and agent discovery disabled,
 * so the only accounts are the mock engine's "mock" and "mock2"; prompt keywords drive the
 * scripted behaviour (see internal/worker/chat/mock.go).
 *
 * All tests share one hub and one syred. The "limit" keyword marks the mock account's
 * weekly window exhausted for later probes, so every test picks its account explicitly and
 * the usage-limit test copes with "mock" already being exhausted (CI retries).
 */

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

async function signIn(page: Page, env: E2EEnv): Promise<void> {
  await page.goto(`${env.baseURL}/command`);
  // Signed out, the login page offers pairing; the recovery key is the fallback.
  await page.getByRole('button', { name: 'Use recovery key' }).click();
  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  // (The login card's "Syre" heading also matches a case-insensitive "SYRE".)
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

/** Opens a fresh new-chat draft with the given mock account selected. */
async function newChat(page: Page, accountId: 'mock' | 'mock2' = 'mock'): Promise<Locator> {
  const chat = page.locator('app-chat-view');
  await chat.getByTestId('new-chat').click();
  const chip = chat.getByTestId('account-chip');
  // Capabilities arrive over the socket: wait until the mock accounts are known.
  await expect(chip).toContainText('Mock');
  await chip.click();
  const menu = chat.getByRole('menu', { name: 'Agent account' });
  await menu.locator(`[data-account="${accountId}"]`).click();
  await expect(menu).toBeHidden();
  await expect(chip).toContainText(accountId === 'mock' ? /Mock(?! 2)/ : /Mock 2/);
  return chat;
}

/** The visible status pill (header on desktop, under the header on mobile). */
function statusPill(chat: Locator): Locator {
  return chat.locator('sy-status-pill:visible [data-status]').first();
}

/**
 * Records every status the pill shows from now on, so fast transitions (a short mock turn is
 * over in well under a second) are asserted from the DOM history instead of by racing polls.
 */
async function recordStatuses(page: Page): Promise<() => Promise<string[]>> {
  await page.evaluate(() => {
    const w = window as unknown as { __ksStatuses: string[]; __ksObs?: MutationObserver };
    w.__ksObs?.disconnect();
    w.__ksStatuses = [];
    const root = document.querySelector('app-chat-view')!;
    const note = (el: Element) => {
      const s = el.getAttribute('data-status');
      if (s && w.__ksStatuses[w.__ksStatuses.length - 1] !== s) w.__ksStatuses.push(s);
    };
    w.__ksObs = new MutationObserver((muts) => {
      for (const m of muts) {
        if (m.type === 'attributes') note(m.target as Element);
        m.addedNodes.forEach((n) => {
          if (n instanceof Element) {
            if (n.hasAttribute('data-status')) note(n);
            n.querySelectorAll('[data-status]').forEach(note);
          }
        });
      }
    });
    w.__ksObs.observe(root, {
      subtree: true,
      childList: true,
      attributes: true,
      attributeFilter: ['data-status'],
    });
  });
  return () =>
    page.evaluate(() => (window as unknown as { __ksStatuses: string[] }).__ksStatuses.slice());
}

async function send(chat: Locator, text: string): Promise<void> {
  const input = chat.getByTestId('composer-input');
  await input.fill(text);
  await chat.getByTestId('send-button').click();
  await expect(chat.getByTestId('user-bubble').filter({ hasText: text })).toBeVisible();
}

test.describe('Subscription-only agent chat (mock engine, no API key)', () => {
  test.beforeEach(async ({ page }) => {
    const env = loadEnv();
    await signIn(page, env);
  });

  test('no API key is configured and the composer offers the mock account, models, efforts and approvals', async ({
    page,
  }) => {
    const settings = await getJSON<{ llm_api_key_set: boolean }>(page, '/api/settings');
    expect(settings.llm_api_key_set).toBe(false);

    const chat = await newChat(page, 'mock');

    // (a) model popover: mock models with descriptions; effort row follows the model.
    const trigger = chat.getByTestId('model-trigger');
    await expect(trigger).toHaveText('Mock Smart High');
    await trigger.click();
    const picker = chat.getByRole('dialog', { name: 'Model and effort' });
    const models = picker.getByRole('listbox', { name: 'Models' });
    await expect(models.getByRole('option')).toHaveCount(3);
    await expect(models.locator('[data-model="mock-smart"]')).toContainText(
      'Deterministic scripted replies',
    );
    await expect(models.locator('[data-model="mock-fast"]')).toContainText(
      'Same script, fewer efforts',
    );
    await expect(models.locator('[data-model="mock-mini"]')).toContainText('No effort levels');
    const efforts = picker.getByTestId('effort-row').getByRole('radio');
    await expect(efforts).toHaveText(['Low', 'Med', 'High', 'Max']);
    await expect(picker.locator('[data-effort="high"]')).toHaveAttribute('aria-checked', 'true');

    await picker.getByRole('radio', { name: 'Max' }).click();
    await expect(trigger).toHaveText('Mock Smart Max');

    await models.locator('[data-model="mock-fast"]').click();
    await expect(efforts).toHaveText(['Low', 'Med']);
    await expect(trigger).toHaveText('Mock Fast Med');

    await models.locator('[data-model="mock-mini"]').click();
    // No efforts: the popover closes on pick and the trigger has no effort suffix.
    await expect(picker).toBeHidden();
    await expect(trigger).toHaveText('Mock Mini');
    await trigger.click();
    await expect(picker.getByTestId('effort-row')).toHaveCount(0);
    // Escape inside the popover closes it (press it from the search box, not before focus
    // has moved into the freshly opened panel).
    await picker.getByTestId('model-search').press('Escape');
    await expect(picker).toBeHidden();

    // (b) approval popover: the account's own modes; choosing one updates the chip.
    const approval = chat.getByTestId('approval-chip');
    await expect(approval).toHaveAttribute('aria-label', 'Approval mode: Ask');
    await approval.click();
    const modes = chat.getByRole('menu', { name: 'Approval mode' });
    await expect(modes.getByRole('menuitemradio')).toHaveCount(5);
    for (const [id, label] of [
      ['plan', 'Plan'],
      ['default', 'Ask'],
      ['accept-edits', 'Accept edits'],
      ['auto', 'Auto'],
      ['full-access', 'Full access'],
    ]) {
      await expect(modes.locator(`[data-mode="${id}"]`)).toContainText(label);
    }
    await expect(modes.locator('[data-mode="plan"]')).toContainText('Read-only: proposes changes');
    await modes.locator('[data-mode="accept-edits"]').click();
    await expect(modes).toBeHidden();
    await expect(approval).toHaveAttribute('aria-label', 'Approval mode: Accept edits');
  });

  test('a normal prompt streams a reply and a tool card; status goes working to done', async ({
    page,
  }) => {
    const chat = await newChat(page, 'mock2');
    const statuses = await recordStatuses(page);
    // A long prompt makes the streamed echo take a while (one delta per word).
    const prompt =
      'Hello from the subscription e2e suite, please reply with a friendly greeting and keep this sentence going for a few more words so the stream lasts';
    await send(chat, prompt);

    const reply = chat
      .getByTestId('assistant-message')
      .filter({ hasText: 'Mock reply from mock-smart' });
    await expect(reply).toContainText('so the stream lasts');
    await expect(
      chat.getByTestId('assistant-message').filter({ hasText: 'Let me look at the files.' }),
    ).toBeVisible();
    const tool = chat.locator('sy-tool-call-card').filter({ hasText: 'Read README.md' });
    await expect(tool.locator('[data-tool-status]')).toHaveAttribute(
      'data-tool-status',
      'completed',
    );
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'done');

    const seen = await statuses();
    expect(seen).toContain('working');
    expect(seen.lastIndexOf('done')).toBeGreaterThan(seen.indexOf('working'));
  });

  test('permission: the card asks, Allow completes the turn', async ({ page }) => {
    const chat = await newChat(page, 'mock2');
    await send(chat, 'permission to clean up');

    const card = chat.locator('[data-elicitation-kind="permission"]');
    await expect(card).toBeVisible();
    await expect(card.getByRole('heading', { name: 'Allow Bash: rm -rf build/?' })).toBeVisible();
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'needs_you');
    await card.getByRole('button', { name: 'Allow once' }).click();

    await expect(
      chat.getByTestId('assistant-message').filter({ hasText: 'Done: build/ was removed.' }),
    ).toBeVisible();
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'done');
    await expect(chat.locator('[data-elicitation-state="resolved"]')).toBeVisible();
  });

  test('ask_user: the question card takes a typed answer and the turn continues', async ({
    page,
  }) => {
    const chat = await newChat(page, 'mock2');
    await send(chat, 'ask me which color to use');

    const card = chat.locator('[data-elicitation-kind="question"]');
    await expect(
      card.getByRole('heading', { name: 'Which color should the button be?' }),
    ).toBeVisible();
    await expect(card.getByRole('button', { name: 'Blue' })).toBeVisible();
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'needs_you');
    // Free text sits behind "Other…" on the question card (the same card as the queue).
    await card.getByTestId('question-other').click();
    await card.getByTestId('question-other-input').fill('Purple');
    await card.getByRole('button', { name: 'Send' }).click();

    await expect(
      chat.getByTestId('assistant-message').filter({ hasText: 'You answered: Purple' }),
    ).toBeVisible();
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'done');
  });

  test('Stop cancels a long turn', async ({ page }) => {
    const chat = await newChat(page, 'mock2');
    await send(chat, 'slow');
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'working');
    await expect(chat.getByTestId('assistant-message').filter({ hasText: 'tick 1' })).toBeVisible();

    await chat.getByTestId('stop-button').click();
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'cancelled');
    await expect(chat.getByTestId('send-button')).toBeVisible();
  });

  test('fail auth: the sign-in card shows the relogin command', async ({ page }) => {
    const env = loadEnv();
    const chat = await newChat(page, 'mock2');
    await send(chat, 'fail auth');

    const card = chat.locator('[data-error-kind="auth"]');
    await expect(card.getByTestId('error-title')).toHaveText('Mock 2 needs you to sign in again');
    // The command carries the account's profile env so it signs in the right account.
    await expect(card.getByTestId('relogin-command')).toHaveText(
      'SYRE_MOCK_ACCOUNT=mock2 mock login',
    );
    await expect(
      card.getByRole('button', { name: `Open terminal on ${env.workerName}` }),
    ).toBeEnabled();
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'failed');
  });

  test('crash: the exit card shows the code and expandable stderr', async ({ page }) => {
    const chat = await newChat(page, 'mock2');
    await send(chat, 'crash');

    const card = chat.locator('[data-error-kind="exit"]');
    await expect(card.getByTestId('error-title')).toHaveText('mock exited with code 3');
    await expect(card.getByTestId('error-stderr')).toHaveCount(0);
    const toggle = card.getByRole('button', { name: 'Show error output' });
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await toggle.click();
    await expect(card.getByTestId('error-stderr')).toContainText('fatal: simulated crash');
    await expect(card.getByRole('button', { name: 'Hide error output' })).toHaveAttribute(
      'aria-expanded',
      'true',
    );
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'failed');
  });

  test('usage lists the mock accounts (fleet strip on desktop, the fleet quota on a phone)', async ({ page, isMobile }) => {
    if (isMobile) {
      // A phone has no fleet strip: the read-only fleet lists each subscription's usage. (The
      // mobile project runs after the desktop usage-limit tests have exhausted both.)
      await page.getByTestId('back-to-inbox').click();
      await page.getByTestId('mobile-fleet-link').click();
      const quota = page.getByTestId('mobile-quota');
      await expect(quota.getByText('Mock', { exact: true })).toBeVisible();
      await expect(quota.getByText('Mock 2', { exact: true })).toBeVisible();
      const box = await quota.boundingBox();
      expect(box?.width ?? 999).toBeLessThanOrEqual(page.viewportSize()!.width);
      return;
    }
    const strip = page.getByTestId('fleet-strip');
    await expect(strip.getByTestId('footer-usage')).toContainText('Mock');
    await strip.getByTestId('footer-usage').click();
    const popover = page.getByRole('dialog', { name: 'Subscription usage' });
    await expect(popover).toBeVisible();
    await expect(popover.locator('[data-account-row="mock"]')).toContainText('Mock');
    const mock2 = popover.locator('[data-account-row="mock2"]');
    await expect(mock2).toContainText('Mock 2');
    // mock2's 5-hour window is at 87%: shown as a warning with its percentage.
    await expect(mock2).toHaveAttribute('data-health', 'warning');
    await expect(mock2.getByTestId('usage-state')).toContainText('87% of 5-hour');
    const box = await popover.boundingBox();
    expect(box?.width ?? 999).toBeLessThanOrEqual(page.viewportSize()!.width);
  });

  // Runs late on purpose: it leaves "mock" exhausted for the rest of the syred's life.
  test('usage limit: limit card, exhausted account, switch to mock2 and continue', async ({
    page,
  }, testInfo) => {
    test.skip(
      testInfo.project.name !== 'desktop',
      'needs a healthy mock and mock2; the desktop run used them up',
    );
    const chat = await newChat(page, 'mock');
    await send(chat, 'limit');

    // The limit card: window name plus reset time and countdown.
    const card = chat.locator('[data-error-kind="usage_limit"]');
    await expect(card.getByTestId('error-title')).toContainText(/Out of 5-hour usage until \S/);
    await expect(card.getByTestId('error-reset')).toContainText(/Resets in .+ · \S/);
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'failed');

    // syred folds the rate-limit event into the account's capabilities and the hub
    // reports the account exhausted.
    await expect
      .poll(async () => {
        const { accounts } = await getJSON<{
          accounts: Array<{ id: string; exhausted?: boolean }>;
        }>(page, '/api/agents/accounts');
        return accounts.find((a) => a.id === 'mock')?.exhausted;
      })
      .toBe(true);

    // The hub pushes the account change live (fleet.update agent_accounts with worker_id).
    const chip = chat.getByTestId('account-chip');
    await expect(chip).toContainText('Out');
    const strip = page.getByTestId('fleet-strip');
    await expect(strip.locator('[data-usage-account="mock"]')).toContainText('out');
    await strip.getByTestId('footer-usage').click();
    const usage = page.getByRole('dialog', { name: 'Subscription usage' });
    const mockRow = usage.locator('[data-account-row="mock"]');
    await expect(mockRow).toHaveAttribute('data-health', 'exhausted');
    await expect(mockRow.getByTestId('usage-state')).toContainText(
      /Exhausted until .+ \((5-hour|weekly)\)/,
    );
    await usage.press('Escape');
    await expect(usage).toBeHidden();

    // Pre-send warning offers the other account; switching works and the next turn succeeds.
    const warning = chat.getByTestId('composer-warning');
    await expect(warning).toContainText(/Mock is out of (5-hour|weekly) usage until/);
    await expect(warning).toContainText('Use Mock 2 instead?');
    await warning.getByTestId('switch-account').click();
    await expect(chip).toContainText('Mock 2');
    await expect(warning).toBeHidden();

    await send(chat, 'continue on the second account');
    await expect(
      chat
        .getByTestId('assistant-message')
        .filter({ hasText: 'Mock reply from mock-smart: continue on the second account' }),
    ).toBeVisible();
    await expect(statusPill(chat)).toHaveAttribute('data-status', 'done');
    // The hub moved the chat itself to mock2 (not just the composer).
    const { sessions } = await getJSON<{ sessions: Array<{ title: string; account_id?: string }> }>(
      page,
      '/api/chats',
    );
    expect(sessions.filter((s) => s.title === 'limit').map((s) => s.account_id)).toContain('mock2');
  });

  // SYR-PYG35 regression: the chip must turn exhausted from the live push alone (no reload,
  // no Refresh). It exhausts mock2 too, so it stays last.
  test('usage limit: the account chip turns exhausted live, without a refresh', async ({
    page,
  }, testInfo) => {
    test.skip(testInfo.project.name !== 'desktop', 'desktop only');
    const chat = await newChat(page, 'mock2');
    await send(chat, 'limit');
    await expect(chat.locator('[data-error-kind="usage_limit"]')).toBeVisible();
    await expect(chat.getByTestId('account-chip')).toContainText('Out', { timeout: 5_000 });
  });
});

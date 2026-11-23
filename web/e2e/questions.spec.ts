import fs from 'node:fs';
import { expect, Locator, Page, test } from '@playwright/test';
import { E2E_STATE_FILE } from './global-setup';

/**
 * Questions and steering (KON-2Q0CH, PRD §5.8b) against the mock engine, which calls
 * Konsert's ask_user over MCP like a real agent ("question"), forks its session for side
 * conversations and decides delegated questions as the orchestrator. Desktop and phone
 * (390px): answer via an option, via free text, after a side conversation, and by letting
 * the orchestrator decide.
 */

interface E2EEnv {
  baseURL: string;
  adminToken: string;
  workerId: string;
}

const QUESTION = 'Accept the shortened exit criteria on KON-2Q0CH?';

function loadEnv(): E2EEnv {
  return JSON.parse(fs.readFileSync(E2E_STATE_FILE, 'utf8')) as E2EEnv;
}

async function signIn(page: Page, env: E2EEnv): Promise<void> {
  await page.goto(env.baseURL);
  await page.locator('#token').fill(env.adminToken);
  await page.getByRole('button', { name: 'Sign In' }).click();
  await expect(page.locator('#token')).toBeHidden();
  await expect(page.locator('app-chat-view')).toBeVisible();
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

async function newChat(page: Page): Promise<Locator> {
  const chat = page.locator('app-chat-view');
  await chat.getByTestId('new-chat').click();
  const chip = chat.getByTestId('account-chip');
  await expect(chip).toContainText('Mock');
  await chip.click();
  const menu = chat.getByRole('menu', { name: 'Agent account' });
  await menu.locator('[data-account="mock2"]').click();
  await expect(menu).toBeHidden();
  return chat;
}

async function send(chat: Locator, text: string): Promise<void> {
  await chat.getByTestId('composer-input').fill(text);
  await chat.getByTestId('send-button').click();
  await expect(chat.getByTestId('user-bubble').filter({ hasText: text })).toBeVisible();
}

/** Sends "question" and returns the sid of the question card that appears in the chat. */
async function askQuestion(chat: Locator): Promise<string> {
  await send(chat, 'question');
  const inline = chat.getByTestId('question-card').filter({ hasText: QUESTION });
  await expect(inline).toBeVisible();
  const sid = await inline.getAttribute('data-question');
  expect(sid).toMatch(/^q-[0-9a-z]{5,12}$/);
  return sid!;
}

/** Opens the Questions panel from the top bar on the given question. */
async function openQueue(page: Page, sid: string): Promise<Locator> {
  await expect(page.getByTestId('questions-button-count')).toBeVisible();
  await page.getByTestId('questions-button').click();
  const panel = page.getByTestId('questions-panel');
  await expect(panel).toBeVisible();
  await panel.locator(`[data-queue-item="${sid}"]`).click();
  return panel;
}

function assistant(chat: Locator, text: string): Locator {
  return chat.getByTestId('assistant-message').filter({ hasText: text });
}

test.describe('Questions (mock engine)', () => {
  test.beforeEach(async ({ page }) => {
    await signIn(page, loadEnv());
  });

  test('answer via an option: the card explains the question; the agent gets the answer in-turn', async ({ page }) => {
    const chat = await newChat(page);
    const sid = await askQuestion(chat);
    const panel = await openQueue(page, sid);
    const card = panel.getByTestId('questions-detail').getByTestId('question-card');
    await expect(card).toHaveAttribute('data-question', sid);
    await expect(card.getByTestId('question-title')).toHaveText(QUESTION);
    await expect(card.getByTestId('question-context')).toContainText('requires a live phone run');
    await expect(card.getByTestId('question-context')).toContainText('Closing the epic waits on this');
    await expect(card.getByTestId('question-refs')).toContainText('KON-2Q0CH');
    const yes = card.getByTestId('question-option-option_1');
    await expect(yes).toContainText('Recommended');
    await expect(yes).toContainText('Why: The mobile e2e covers the same flow');
    await expect(card.getByTestId('question-option-option_2')).toContainText('stays open until someone does the phone run');

    await card.getByTestId('question-option-option_2').click();
    await expect(card.getByTestId('question-receipt')).toContainText('Sent “No, keep the strict exit criteria”');
    await panel.getByTestId('questions-close').click();
    await expect(assistant(chat, 'Ask result: The user chose: "No, keep the strict exit criteria".')).toBeVisible();
  });

  test('answer with free text, using the keyboard to move through the queue', async ({ page, isMobile }) => {
    const chat = await newChat(page);
    const sid = await askQuestion(chat);
    const panel = await openQueue(page, sid);
    const card = panel.getByTestId('questions-detail').getByTestId('question-card');
    await expect(card).toHaveAttribute('data-question', sid);
    if (!isMobile) {
      // j/k move the selection (other open questions may be queued) and come back.
      await page.keyboard.press('j');
      await page.keyboard.press('k');
      await panel.locator(`[data-queue-item="${sid}"]`).click();
    }
    await card.getByTestId('question-other').click();
    await card.getByTestId('question-other-input').fill('Do the phone run on Friday, then close');
    await card.getByTestId('question-other-send').click();
    await expect(card.getByTestId('question-receipt')).toContainText('Sent “Do the phone run on Friday, then close”');
    if (isMobile) await panel.getByTestId('questions-close').click();
    else await page.keyboard.press('Escape');
    await expect(panel).toBeHidden();
    await expect(assistant(chat, 'Ask result: The user answered: Do the phone run on Friday, then close')).toBeVisible();
  });

  test('side conversation with a fork of the agent, then answer with a summary', async ({ page, isMobile }) => {
    const chat = await newChat(page);
    const sid = await askQuestion(chat);
    // "Ask about this" from the card in the chat opens the queue on that question.
    await chat.getByTestId('question-card').filter({ hasText: QUESTION }).getByTestId('question-ask').click();
    const panel = page.getByTestId('questions-panel');
    const thread = panel.getByTestId('side-conversation');
    await expect(thread).toBeVisible();
    await expect(panel.getByTestId('questions-detail').getByTestId('question-card')).toHaveAttribute('data-question', sid);
    await expect(thread.getByTestId('thread-pinned-options')).toContainText('No, keep the strict exit criteria');

    await thread.getByTestId('thread-input').fill('What is KON-2Q0CH, why change the exit criteria, and why do you recommend Yes?');
    await thread.getByTestId('thread-send').click();
    await expect(thread.getByTestId('thread-user')).toContainText('why do you recommend Yes?');
    await expect(thread.getByTestId('thread-answer')).toContainText('read-only fork of');
    await expect(thread.getByTestId('thread-answerer')).toContainText('Read-only copy');
    // The original is still blocked on its question.
    await expect(chat.locator('ks-status-pill:visible [data-status]').first()).toHaveAttribute('data-status', 'needs_you');

    await expect(thread.getByTestId('thread-draft')).toContainText('Yes');
    await thread.getByTestId('thread-use-draft').click();
    await expect(thread.getByTestId('thread-receipt')).toContainText('with a summary of your side conversation', { timeout: 20_000 });
    if (isMobile) await thread.getByTestId('thread-close').click(); // the sheet covers the header
    await panel.getByTestId('questions-close').click();
    await expect(assistant(chat, 'Side conversation: The user asked what KON-2Q0CH is')).toBeVisible();
  });

  test('let the orchestrator decide: a receipt with its reason, then the agent gets it', async ({ page }) => {
    await api(page, 'PUT', '/api/questions/settings', { auto_delegate: [], delegation_grace_seconds: 1 });
    await api(page, 'POST', '/api/chats/orchestrator', {});
    const chat = await newChat(page);
    const sid = await askQuestion(chat);
    const panel = await openQueue(page, sid);
    const card = panel.getByTestId('questions-detail').getByTestId('question-card');
    await card.getByTestId('question-delegate').click();
    await expect(card.getByTestId('question-receipt')).toContainText('The orchestrator sent “Yes”', { timeout: 25_000 });
    await expect(card.getByTestId('question-receipt')).toContainText('Mock orchestrator');
    await panel.getByTestId('questions-close').click();
    await expect(assistant(chat, "The orchestrator answered on the user's behalf")).toBeVisible();
  });
});

test.describe('Questions on a phone (390px)', () => {
  test.skip(({ isMobile }) => !isMobile, 'phone layout');
  test.beforeEach(async ({ page }) => {
    await signIn(page, loadEnv());
  });

  test('inbox, full-screen question, side-conversation sheet with pinned options, answer', async ({ page }) => {
    const chat = await newChat(page);
    const sid = await askQuestion(chat);

    await page.getByTestId('questions-button').click();
    const panel = page.getByTestId('questions-panel');
    const queue = panel.getByTestId('questions-queue');
    await expect(queue).toBeVisible();
    await queue.locator(`[data-queue-item="${sid}"]`).click();
    // Full-screen question: the inbox is hidden, the options are reachable.
    await expect(queue).toBeHidden();
    const card = panel.getByTestId('questions-detail').getByTestId('question-card');
    await expect(card).toHaveAttribute('data-question', sid);
    await expect(card.getByTestId('question-option-option_1')).toBeVisible();

    await card.getByTestId('question-ask').click();
    const sheet = panel.getByTestId('side-conversation');
    await expect(sheet).toBeVisible();
    const pinned = sheet.getByTestId('thread-pinned-options');
    await expect(pinned).toBeInViewport();
    await sheet.getByTestId('thread-input').fill('Why now?');
    await sheet.getByTestId('thread-send').click();
    await expect(sheet.getByTestId('thread-answer')).toContainText('read-only fork of');
    await expect(pinned).toBeInViewport();

    await pinned.getByTestId('thread-option-option_1').click();
    await expect(sheet.getByTestId('thread-receipt')).toContainText('Sent “Yes”', { timeout: 20_000 });
    await sheet.getByTestId('thread-close').click();
    await panel.getByTestId('questions-back').click();
    await expect(queue).toBeVisible();
    await panel.getByTestId('questions-close').click();
    await expect(assistant(chat, 'Side conversation:')).toBeVisible();
  });
});

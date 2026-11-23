import fs from 'node:fs';
import { defineConfig } from '@playwright/test';

const localChrome =
  !process.env['CI'] && fs.existsSync('/Applications/Google Chrome.app/Contents/MacOS/Google Chrome')
    ? { channel: 'chrome' as const }
    : {};

export default defineConfig({
  testDir: './e2e',
  timeout: 30_000,
  expect: {
    timeout: 10_000,
  },
  fullyParallel: false,
  workers: 1,
  retries: process.env['CI'] ? 1 : 0,
  reporter: [['list']],
  globalSetup: './e2e/global-setup.ts',
  use: {
    headless: true,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    ...localChrome,
  },
  projects: [
    {
      name: 'desktop',
      use: {
        browserName: 'chromium',
        viewport: { width: 1440, height: 900 },
      },
      testMatch: /(konsert|subscription-chat|first-run|human-control|questions|activity)\.spec\.ts/,
    },
    {
      name: 'mobile',
      use: {
        browserName: 'chromium',
        viewport: { width: 390, height: 844 },
        isMobile: true,
        hasTouch: true,
      },
      testMatch: /(konsert|subscription-chat|questions)\.spec\.ts/,
    },
  ],
});

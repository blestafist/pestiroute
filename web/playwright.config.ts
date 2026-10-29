import { defineConfig } from '@playwright/test'

const executablePath = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH

export default defineConfig({
  testDir: './browser',
  testMatch: '**/*.pw.ts',
  workers: 1,
  timeout: 90_000,
  use: {
    headless: true,
    launchOptions: executablePath ? { executablePath } : {},
  },
})

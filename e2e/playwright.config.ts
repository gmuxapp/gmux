import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: './tests',
  timeout: 30_000,
  retries: 0,
  workers: 1, // serial — tests share a backend
  use: {
    // Port is written to env by global-setup. Tests run in a worker that
    // inherits the env, so this is evaluated after globalSetup sets the var.
    baseURL: `http://127.0.0.1:${process.env.GMUXD_TEST_PORT || '18790'}`,
    headless: true,
    viewport: { width: 1200, height: 800 },
  },
  globalSetup: './global-setup.ts',
  globalTeardown: './global-teardown.ts',
})

import type { Page } from '@playwright/test'

export interface TermState {
  termCols: number | undefined
  termRows: number | undefined
}

/**
 * Navigate to the app, authenticating via the QR-code URL flow first.
 * `/auth/login?token=X` validates the token and sets a session cookie, then
 * redirects to `/`. Subsequent navigations in this context use the cookie.
 */
export async function openApp(page: Page, urlPath: string = '/'): Promise<void> {
  const token = process.env.GMUX_TEST_TOKEN
  if (!token) throw new Error('GMUX_TEST_TOKEN not set; global-setup did not run')
  // Always go through the login endpoint first; it'll redirect to `/`.
  await page.goto(`/auth/login?token=${token}`)
  // If the target path isn't `/`, navigate there after auth is established.
  if (urlPath !== '/') await page.goto(urlPath)
}

/** Read the xterm terminal dimensions exposed via window.__gmuxTerm. */
export async function getTermState(page: Page): Promise<TermState> {
  return page.evaluate(() => {
    const term = (window as any).__gmuxTerm
    return {
      termCols: term?.cols as number | undefined,
      termRows: term?.rows as number | undefined,
    }
  })
}

/** Check whether the resize pill is visible. */
export async function isPillVisible(page: Page): Promise<boolean> {
  return page.locator('.terminal-resize-overlay').isVisible().catch(() => false)
}

/**
 * Wait for a session's terminal to be visible. The app auto-selects the
 * first alive session on load, so we don't need to click anything. If
 * auto-select hasn't kicked in (e.g. no sessions yet), the wait will time
 * out and the caller will see a clear failure.
 */
export async function selectFirstSession(page: Page): Promise<void> {
  await page.locator('.xterm').waitFor({ state: 'visible', timeout: 5_000 })
  // Give the WS connection time to establish and replay scrollback
  await page.waitForTimeout(1500)
}

/** Click the resize pill. */
export async function clickPill(page: Page): Promise<void> {
  await page.locator('.terminal-resize-overlay').click()
  await page.waitForTimeout(1000)
}

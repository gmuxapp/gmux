import type { Page } from '@playwright/test'

/**
 * Bearer-auth wrapper around `fetch` against the test gmuxd. Returns
 * the parsed JSON body and the response status; tests assert on both.
 *
 * Targeting `127.0.0.1:${GMUXD_TEST_PORT}` directly (not via Playwright's
 * baseURL) keeps these calls independent of any browser/page context
 * so they work in `test.beforeAll`, helpers, and global setup teardown.
 */
export async function apiGet<T = unknown>(urlPath: string): Promise<{ status: number; body: T }> {
  const port = process.env.GMUXD_TEST_PORT
  const token = process.env.GMUX_TEST_TOKEN
  if (!port) throw new Error('GMUXD_TEST_PORT not set; global-setup did not run')
  if (!token) throw new Error('GMUX_TEST_TOKEN not set; global-setup did not run')
  const resp = await fetch(`http://127.0.0.1:${port}${urlPath}`, {
    headers: { Authorization: `Bearer ${token}` },
  })
  // 404 bodies aren't JSON; tolerate that.
  let body: T = undefined as unknown as T
  try {
    body = await resp.json() as T
  } catch { /* non-JSON, leave body undefined */ }
  return { status: resp.status, body }
}

/**
 * Poll `fn` until it returns a defined, truthy value (or until
 * `timeoutMs` elapses). Returns the resolved value or throws.
 *
 * Use for assertions that depend on async work the daemon does after
 * a side-effect (e.g. "file written → reachable in API"). Default
 * 2s ceiling intentionally tight: the watcher path is sub-second, so
 * a longer wait masks regression to a periodic scan.
 */
export async function pollUntil<T>(
  fn: () => Promise<T | null | undefined | false>,
  opts: { timeoutMs?: number; intervalMs?: number; description?: string } = {},
): Promise<T> {
  const timeoutMs = opts.timeoutMs ?? 2_000
  const intervalMs = opts.intervalMs ?? 50
  const description = opts.description ?? 'condition'
  const start = Date.now()
  let lastValue: T | null | undefined | false = undefined
  while (Date.now() - start < timeoutMs) {
    lastValue = await fn()
    if (lastValue) return lastValue as T
    await new Promise(r => setTimeout(r, intervalMs))
  }
  throw new Error(`pollUntil: ${description} did not become truthy within ${timeoutMs}ms (last=${JSON.stringify(lastValue)})`)
}

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
 * Navigate to the test session and wait for the terminal to be visible.
 *
 * The home page no longer auto-selects a session, so we drive
 * navigation via the test-only `__gmuxNavigateToSession(id)` hook
 * installed by main.tsx. The session ID is set by global-setup and
 * exposed via GMUX_TEST_SESSION_ID.
 */
export async function gotoTestSession(page: Page): Promise<void> {
  const sessionId = process.env.GMUX_TEST_SESSION_ID
  if (!sessionId) throw new Error('GMUX_TEST_SESSION_ID not set; global-setup did not run')

  // Wait for the store to load sessions/projects, then drive
  // navigation. The hook returns false until the session appears in
  // the store.
  await page.waitForFunction((id) => {
    const navigate = (window as any).__gmuxNavigateToSession
    if (typeof navigate !== 'function') return false
    return navigate(id) === true
  }, sessionId, { timeout: 10_000 })

  // Confirm navigation actually changed the URL before waiting for
  // the terminal. If something redirects us back to /, fail loudly
  // here. The slug `test-project` is seeded by global-setup.
  await page.waitForURL(/\/test-project\/shell\//, { timeout: 5_000 })

  await page.locator('.xterm').waitFor({ state: 'visible', timeout: 5_000 })
  // Give the WS connection time to establish and replay scrollback.
  await page.waitForTimeout(1500)
}

/** Click the resize pill. */
export async function clickPill(page: Page): Promise<void> {
  await page.locator('.terminal-resize-overlay').click()
  await page.waitForTimeout(1000)
}

import type { Page } from '@playwright/test'
import { spawn, type ChildProcess } from 'node:child_process'
import * as fs from 'node:fs'
import * as path from 'node:path'

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
  let body: T = undefined as unknown as T
  try {
    body = await resp.json() as T
  } catch { /* non-JSON, leave body undefined */ }
  return { status: resp.status, body }
}

export async function apiPost<T = unknown>(urlPath: string, payload?: unknown): Promise<{ status: number; body: T }> {
  const port = process.env.GMUXD_TEST_PORT
  const token = process.env.GMUX_TEST_TOKEN
  if (!port) throw new Error('GMUXD_TEST_PORT not set; global-setup did not run')
  if (!token) throw new Error('GMUX_TEST_TOKEN not set; global-setup did not run')
  const resp = await fetch(`http://127.0.0.1:${port}${urlPath}`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: payload !== undefined ? JSON.stringify(payload) : undefined,
  })
  let body: T = undefined as unknown as T
  try { body = await resp.json() as T } catch { /* non-JSON */ }
  return { status: resp.status, body }
}

export async function apiDelete<T = unknown>(urlPath: string, payload?: unknown): Promise<{ status: number; body: T }> {
  const port = process.env.GMUXD_TEST_PORT
  const token = process.env.GMUX_TEST_TOKEN
  if (!port) throw new Error('GMUXD_TEST_PORT not set; global-setup did not run')
  if (!token) throw new Error('GMUX_TEST_TOKEN not set; global-setup did not run')
  const resp = await fetch(`http://127.0.0.1:${port}${urlPath}`, {
    method: 'DELETE',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: payload !== undefined ? JSON.stringify(payload) : undefined,
  })
  let body: T = undefined as unknown as T
  try { body = await resp.json() as T } catch { /* non-JSON */ }
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

  // Drive navigation via the test hook. It returns true only once
  // the session and its project are both in the store and the URL
  // change has been dispatched, so by the time this resolves the
  // app is on the session route.
  await page.waitForFunction((id) => {
    const navigate = (window as any).__gmuxNavigateToSession
    if (typeof navigate !== 'function') return false
    return navigate(id) === true
  }, sessionId, { timeout: 10_000 })

  await page.locator('.terminal-container canvas').waitFor({ state: 'visible', timeout: 5_000 })
  // Give the WS connection time to establish and replay scrollback.
  await page.waitForTimeout(1500)
}

/** Click the resize pill. */
export async function clickPill(page: Page): Promise<void> {
  await page.locator('.terminal-resize-overlay').click()
  await page.waitForTimeout(1000)
}

// ── Session spawn helper ──

/**
 * Spawn a fresh gmux session against the test daemon.
 *
 * The default global-setup test session runs an idle bash. Any
 * test that needs a session shaped differently (long pi-style
 * output, specific exit codes, etc.) builds it with this helper.
 *
 * Each spawned session uses a unique cwd under GMUX_TEST_WORKSPACE
 * so the daemon's session list can disambiguate it from siblings.
 * The cwd is created on first call and reused per-session.
 *
 * The returned `id` is the session id assigned by gmuxd; `kill`
 * sends SIGTERM to the gmux process and is safe to call even after
 * the session has exited (the daemon will move it to dead state
 * and tests can then exercise the dead-session UI on it).
 */
export async function spawnTestSession(
  command: string[],
  opts: { cwdName?: string; timeoutMs?: number } = {},
): Promise<{ id: string; cwd: string; child: ChildProcess; kill: () => void }> {
  const socketDir = process.env.GMUX_TEST_SOCKET_DIR
  const configHome = process.env.GMUX_TEST_CONFIG_HOME
  const stateHome = process.env.GMUX_TEST_STATE_HOME
  const fakeHome = process.env.GMUX_TEST_HOME
  const workspace = process.env.GMUX_TEST_WORKSPACE
  if (!socketDir || !configHome || !stateHome || !fakeHome || !workspace) {
    throw new Error('spawnTestSession: GMUX_TEST_* env not set; global-setup did not run')
  }

  const cwdName = opts.cwdName ?? `spawn-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`
  const cwd = path.join(workspace, cwdName)
  fs.mkdirSync(cwd, { recursive: true })

  // Match the binary path conventions in global-setup.ts: bin/gmux
  // relative to the gmux project root. __dirname is e2e/, so go up
  // one level.
  const projectRoot = path.resolve(__dirname, '..')
  const gmuxBin = path.join(projectRoot, 'bin', 'gmux')

  const env: Record<string, string> = {
    PATH: process.env.PATH || '',
    HOME: fakeHome,
    TERM: 'xterm-256color',
    GMUX_SOCKET_DIR: socketDir,
    GMUXD_TOKEN: process.env.GMUX_TEST_TOKEN || '',
    XDG_CONFIG_HOME: configHome,
    XDG_STATE_HOME: stateHome,
    GMUX_CONFIG_DIR: path.join(configHome, 'gmux'),
  }

  const child = spawn(gmuxBin, command, {
    env,
    cwd,
    stdio: ['ignore', 'pipe', 'pipe'],
    detached: true,
  })

  // Surface child crashes early; tests that ignore them get a
  // dangling promise from waitForSpawnedSession instead of a clean
  // failure.
  child.stderr?.on('data', (d: Buffer) => {
    if (process.env.DEBUG) process.stderr.write(`[spawn-gmux ${cwdName}] ${d}`)
  })

  // Poll the daemon for the new session keyed on cwd.
  const timeoutMs = opts.timeoutMs ?? 10_000
  const id = await waitForSpawnedSession(cwd, timeoutMs)

  return {
    id,
    cwd,
    child,
    kill: () => {
      try { child.kill('SIGTERM') } catch { /* already dead */ }
    },
  }
}

async function waitForSpawnedSession(expectCwd: string, timeoutMs: number): Promise<string> {
  const port = process.env.GMUXD_TEST_PORT
  const token = process.env.GMUX_TEST_TOKEN
  if (!port) throw new Error('GMUXD_TEST_PORT not set')
  if (!token) throw new Error('GMUX_TEST_TOKEN not set')

  const start = Date.now()
  while (Date.now() - start < timeoutMs) {
    try {
      const resp = await fetch(`http://127.0.0.1:${port}/v1/sessions`, {
        headers: { Authorization: `Bearer ${token}` },
      })
      const body = await resp.json() as { data: Array<{ id: string; alive: boolean; cwd?: string }> }
      const match = body.data.find(s => s.alive && s.cwd === expectCwd)
      if (match) return match.id
    } catch { /* retry */ }
    await new Promise(r => setTimeout(r, 100))
  }
  throw new Error(`spawnTestSession: no alive session with cwd=${expectCwd} within ${timeoutMs}ms`)
}

/**
 * Read the on-disk scrollback file for a session. Used by tests
 * that need to compare what the daemon retains on disk versus
 * what the web client receives via the WS snapshot.
 */
export function readScrollbackFile(sessionId: string): Buffer | null {
  const stateHome = process.env.GMUX_TEST_STATE_HOME
  if (!stateHome) throw new Error('GMUX_TEST_STATE_HOME not set')
  const sessionDir = path.join(stateHome, 'gmux', 'sessions', sessionId)
  const active = path.join(sessionDir, 'scrollback')
  const previous = path.join(sessionDir, 'scrollback.0')
  const parts: Buffer[] = []
  if (fs.existsSync(previous)) parts.push(fs.readFileSync(previous))
  if (fs.existsSync(active)) parts.push(fs.readFileSync(active))
  if (parts.length === 0) return null
  return Buffer.concat(parts)
}

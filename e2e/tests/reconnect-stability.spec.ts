import { test, expect, type Page } from '@playwright/test'
import { spawn } from 'child_process'
import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'
import { gotoTestSession, openApp } from '../helpers'

type E2EState = { pids: number[]; daemonPids?: number[]; tmpDir: string; port: number; token: string }
const statePath = path.join(os.tmpdir(), 'gmux-e2e-state.json')

async function readState(): Promise<E2EState> {
  return JSON.parse(await fs.promises.readFile(statePath, 'utf8')) as E2EState
}

async function restartDaemon(state: E2EState): Promise<void> {
  const env = {
    ...process.env,
    HOME: path.join(state.tmpDir, 'home'),
    GMUX_SOCKET_DIR: path.join(state.tmpDir, 'sockets'),
    GMUXD_TOKEN: state.token,
    XDG_CONFIG_HOME: path.join(state.tmpDir, 'config'),
    XDG_STATE_HOME: path.join(state.tmpDir, 'state'),
    TERM: 'xterm-256color',
  }
  const daemon = spawn(path.resolve('bin/gmuxd'), ['run'], { env, stdio: 'ignore', detached: true })
  if (!daemon.pid) throw new Error('gmuxd did not start')
  state.daemonPids = [...(state.daemonPids ?? [state.pids[0]]), daemon.pid]
  state.pids.push(daemon.pid)
  // `pids[0]` is the shared "this is the daemon" slot for every other spec
  // (z-terminal-disconnect signals it to take the backend down). Leaving the
  // dead original there makes those specs fail with ESRCH, so hand the slot
  // over to the replacement. Appending as well keeps global teardown killing
  // every daemon this run started.
  state.pids[0] = daemon.pid
  await fs.promises.writeFile(statePath, JSON.stringify(state))

  const start = Date.now()
  while (Date.now() - start < 15_000) {
    try {
      const response = await fetch(`http://127.0.0.1:${state.port}/v1/health`, {
        headers: { Authorization: `Bearer ${state.token}` }
      })
      if (response.ok) return
    } catch { /* daemon is still starting */ }
    await new Promise(resolve => setTimeout(resolve, 100))
  }
  throw new Error('restarted gmuxd did not become healthy')
}

function currentDaemonPid(state: E2EState): number {
  return state.daemonPids?.at(-1) ?? state.pids[0]
}

async function installEventSourceProbe(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const Native = window.EventSource
    const sources: any[] = []
    ;(window as any).__gmuxEventSources = sources
    ;(window as any).EventSource = class extends Native {
      __openStates: number[] = []
      __errorStates: number[] = []
      __closes = 0
      close() {
        this.__closes++
        super.close()
      }
      constructor(url: string | URL, config?: EventSourceInitDict) {
        super(url, config)
        sources.push(this)
        this.addEventListener('open', () => this.__openStates.push(this.readyState))
        this.addEventListener('error', () => this.__errorStates.push(this.readyState))
      }
    }
  })
}

async function installWebSocketProbe(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const Native = window.WebSocket
    const sockets: any[] = []
    ;(window as any).__gmuxWebSockets = sockets
    ;(window as any).WebSocket = class extends Native {
      constructor(url: string | URL, protocols?: string | string[]) {
        super(url, protocols)
        sockets.push(this)
      }
    }
  })
}

test.describe('SSE reconnect stability', () => {
  test('recovers from a fatal 503 response without a page reload', async ({ page }) => {
    await installEventSourceProbe(page)
    await openApp(page)
    await page.waitForFunction(() => (window as any).__gmuxEventSources?.[0]?.readyState === 1)
    // Let both leading-edge snapshots commit so this is the established
    // stream path (otherwise the initial-connect error screen is expected).
    await page.waitForTimeout(1_000)

    const state = await readState()
    await page.route('**/v1/events*', route => route.fulfill({
      status: 503,
      headers: { 'Content-Type': 'text/plain', 'Retry-After': '1' },
      body: 'temporarily unavailable',
    }))
    // global setup stores gmuxd first and the shell runner second.
    process.kill(state.pids[0], 'SIGTERM')

    const pill = page.locator('.app-reconnecting-pill')
    await page.waitForFunction(
      () => (window as any).__gmuxEventSources.some((s: any) => s.__errorStates.includes(2)),
      undefined,
      { timeout: 20_000 },
    )
    await expect(pill).toBeVisible({ timeout: 2_000 })
    const fatal = await page.evaluate(() => (window as any).__gmuxEventSources.map((s: any) => ({
      errors: s.__errorStates, state: s.readyState,
    })))
    console.log('fatal SSE response:', fatal)

    await page.unroute('**/v1/events*')
    await restartDaemon(state)
    try {
      await expect(pill).not.toBeVisible({ timeout: 20_000 })
    } catch (error) {
      console.log('recovery state:', await page.evaluate(() => ({
        sources: (window as any).__gmuxEventSources.map((s: any) => ({ opens: s.__openStates, errors: s.__errorStates, closes: s.__closes, state: s.readyState })),
        body: document.body.innerText.slice(0, 300),
      })))
      throw error
    }
    await page.waitForFunction(
      () => (window as any).__gmuxEventSources.some((s: any) => s.__openStates.includes(1)),
      undefined,
      { timeout: 5_000 },
    )
  })

  test('a wake on a healthy stream creates no new EventSource', async ({ page }) => {
    await installEventSourceProbe(page)
    await openApp(page)
    await page.waitForFunction(() => (window as any).__gmuxEventSources?.[0]?.readyState === 1)
    await page.waitForTimeout(1_000)

    // Every bootstrap re-sends the full sessions transaction and world frame,
    // which is expensive on cellular at the operator's session count. A phone
    // unlock delivers several lifecycle signals; none may cost a bootstrap.
    for (let i = 0; i < 3; i++) {
      await page.evaluate(() => {
        window.dispatchEvent(new Event('online'))
        window.dispatchEvent(new Event('pageshow'))
        document.dispatchEvent(new Event('visibilitychange'))
        document.dispatchEvent(new Event('resume'))
      })
      await page.waitForTimeout(500)
    }
    await page.waitForTimeout(1_500)
    expect(await page.evaluate(() => (window as any).__gmuxEventSources.length)).toBe(1)
    await expect(page.locator('.app-reconnecting-pill')).not.toBeVisible()
  })

  test('wakes automatically after the bounded retry window is exhausted', async ({ page }) => {
    test.setTimeout(100_000)
    await installEventSourceProbe(page)
    await openApp(page)
    await page.waitForFunction(() => (window as any).__gmuxEventSources?.[0]?.readyState === 1)
    await page.waitForTimeout(1_000)

    const state = await readState()
    await page.route('**/v1/events*', route => route.fulfill({
      status: 503,
      headers: { 'Content-Type': 'text/plain', 'Retry-After': '1' },
      body: 'temporarily unavailable',
    }))
    process.kill(currentDaemonPid(state), 'SIGTERM')

    // The supervisor's production budget is 60 seconds. Waiting for the
    // actual exhaustion, rather than changing it through a test hook, proves
    // a phone returning after a long absence gets a fresh wake budget.
    const retryButton = page.locator('.app-reconnecting-pill button')
    await expect(retryButton).toBeVisible({ timeout: 75_000 })

    // Restore the real daemon while the fault is still installed. The wake
    // below, not a button click, must reopen the exhausted supervisor.
    await restartDaemon(state)
    await page.unroute('**/v1/events*')
    await page.evaluate(() => {
      window.dispatchEvent(new Event('online'))
      window.dispatchEvent(new Event('pageshow'))
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await expect(retryButton).not.toBeVisible({ timeout: 15_000 })
    await page.waitForFunction(
      () => (window as any).__gmuxEventSources.some((s: any) => s.__openStates.includes(1)),
      undefined,
      { timeout: 5_000 },
    )
    // Exactly one live source: replacement must never leak a parallel stream.
    expect(await page.evaluate(() => (window as any).__gmuxEventSources
      .filter((s: any) => s.readyState !== 2).length)).toBe(1)
  })

  test('terminal WebSocket revalidation avoids healthy churn and detects stale sockets', async ({ page }) => {
    await installWebSocketProbe(page)
    await openApp(page)
    await gotoTestSession(page)
    const healthyCount = await page.evaluate(() => (window as any).__gmuxWebSockets.length)
    await page.evaluate(() => window.dispatchEvent(new Event('online')))
    await page.waitForTimeout(1_200)
    expect(await page.evaluate(() => (window as any).__gmuxWebSockets.length)).toBe(healthyCount)

    await page.evaluate(() => {
      const realNow = Date.now.bind(Date)
      ;(window as any).__gmuxRealDateNow = realNow
      Date.now = () => realNow() + 61_000
      window.dispatchEvent(new Event('online'))
    })
    await expect.poll(
      () => page.evaluate(() => (window as any).__gmuxWebSockets.length),
      { timeout: 5_000 },
    ).toBeGreaterThan(healthyCount)
    await page.evaluate(() => { Date.now = (window as any).__gmuxRealDateNow })
  })

  test('terminal WebSocket reconnects after the daemon restarts', async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)
    const pill = page.locator('.terminal-disconnected-pill')
    await expect(pill).not.toBeVisible()

    const state = await readState()
    // The first test restarted gmuxd and appended its PID; the current
    // daemon is therefore the last daemon PID in the disposable state.
    process.kill(currentDaemonPid(state), 'SIGTERM')
    await expect(pill).toBeVisible({ timeout: 10_000 })

    await restartDaemon(state)
    await expect(pill).not.toBeVisible({ timeout: 20_000 })
  })
})

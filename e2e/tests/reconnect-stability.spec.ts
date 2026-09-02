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
  state.pids.push(daemon.pid)
  state.daemonPids = [...(state.daemonPids ?? [state.pids[0]]), daemon.pid]
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

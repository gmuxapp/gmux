import { test, expect, type Page } from '@playwright/test'
import { spawn, type ChildProcess } from 'child_process'
import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'
import { gotoSession, openApp, pollUntil } from '../helpers'

/**
 * The actual promote/demote mutation against the disposable test daemon:
 * a real family (stub `claude` parent — adapter resolution matches on the
 * command basename, so the daemon derives `semantic_agent: true` — plus a
 * shell child inheriting GMUX_SESSION_ID), promoted and returned through
 * the ⋮ menu, asserting the authoritative SSE round trip: the child gains
 * and loses its own sidebar row, the family slot row comes and goes, and
 * the router never leaves the session.
 *
 * Also captures the before/after screenshots for desktop, phone portrait
 * and phone landscape under .memory/screenshots/.
 */

const ROOT = path.resolve(__dirname, '..', '..')
const GMUX = path.join(ROOT, 'bin', 'gmux')
const STATE_FILE = path.join(os.tmpdir(), 'gmux-e2e-state.json')
const SCREENSHOT_DIR = path.join(ROOT, '.memory', 'screenshots')

type WireSession = {
  id: string
  alive: boolean
  cwd?: string
  title?: string
  adapter?: string
  parent_session_id?: string
  semantic_agent?: boolean
  promoted_to_root?: boolean
}

function api(): { base: string; headers: Record<string, string> } {
  const port = process.env.GMUXD_TEST_PORT
  const token = process.env.GMUX_TEST_TOKEN
  if (!port || !token) throw new Error('global-setup did not run')
  return { base: `http://127.0.0.1:${port}`, headers: { Authorization: `Bearer ${token}` } }
}

async function listSessions(): Promise<WireSession[]> {
  const { base, headers } = api()
  const resp = await fetch(`${base}/v1/sessions`, { headers })
  const body = await resp.json() as { data: WireSession[] }
  return body.data
}

async function post(pathname: string): Promise<number> {
  const { base, headers } = api()
  const resp = await fetch(`${base}${pathname}`, { method: 'POST', headers })
  return resp.status
}

let parentProc: ChildProcess | undefined
let childProc: ChildProcess | undefined
let parentId = ''
let childId = ''
let childTitle = ''
let env: Record<string, string>

test.describe.configure({ mode: 'serial' })

test.beforeAll(async () => {
  const state = JSON.parse(fs.readFileSync(STATE_FILE, 'utf-8')) as { tmpDir: string }
  const workspace = process.env.GMUX_TEST_WORKSPACE!
  const home = process.env.GMUX_TEST_HOME!

  // Stub agent binary: basename `claude` is all the adapter registry needs
  // to resolve the claude adapter, which the daemon maps to
  // semantic_agent=true (capability-derived, not behavior-derived).
  const stubBin = path.join(state.tmpDir, 'stub-bin')
  fs.mkdirSync(stubBin, { recursive: true })
  const stub = path.join(stubBin, 'claude')
  fs.writeFileSync(stub, '#!/bin/sh\necho stub agent ready\nexec sleep 600\n')
  fs.chmodSync(stub, 0o755)

  env = {
    PATH: `${stubBin}:${process.env.PATH || ''}`,
    HOME: home,
    TERM: 'xterm-256color',
    GMUX_SOCKET_DIR: path.join(state.tmpDir, 'sockets'),
    GMUXD_TOKEN: process.env.GMUX_TEST_TOKEN || '',
    XDG_CONFIG_HOME: path.join(state.tmpDir, 'config'),
    XDG_STATE_HOME: path.join(state.tmpDir, 'state'),
  }

  parentProc = spawn(GMUX, ['--', 'claude'], {
    env, cwd: workspace, stdio: ['ignore', 'pipe', 'pipe'], detached: true,
  })
  const parent = await pollUntil(async () =>
    (await listSessions()).find(s => s.alive && s.adapter === 'claude'),
  { timeoutMs: 15_000, description: 'stub claude session registered' })
  parentId = parent.id
  expect(parent.semantic_agent, 'stub claude parent must be a semantic agent').toBe(true)

  childProc = spawn(GMUX, ['--', 'sh', '-c', 'echo child ready; sleep 600'], {
    env: { ...env, GMUX_SESSION_ID: parentId },
    cwd: workspace, stdio: ['ignore', 'pipe', 'pipe'], detached: true,
  })
  const child = await pollUntil(async () =>
    (await listSessions()).find(s => s.alive && s.parent_session_id === parentId),
  { timeoutMs: 15_000, description: 'family child session registered' })
  childId = child.id
  childTitle = child.title || 'sh'
  expect(child.promoted_to_root ?? false).toBe(false)

  fs.mkdirSync(SCREENSHOT_DIR, { recursive: true })
})

test.afterAll(async () => {
  // Leave the shared daemon exactly as the other specs expect it: kill and
  // dismiss both spawned sessions, then reap the runner processes.
  for (const id of [childId, parentId]) {
    if (!id) continue
    await post(`/v1/sessions/${id}/kill`).catch(() => {})
  }
  await new Promise(r => setTimeout(r, 500))
  for (const id of [childId, parentId]) {
    if (!id) continue
    await post(`/v1/sessions/${id}/dismiss`).catch(() => {})
  }
  for (const proc of [childProc, parentProc]) {
    if (proc?.pid) {
      try { process.kill(-proc.pid, 'SIGKILL') } catch { /* already dead */ }
    }
  }
})

/** Reveal the sidebar (a no-op on desktop where it is persistent). On
 * coarse pointers it is an off-canvas overlay capped at 360px, so a tap
 * near the right edge always lands on the dismiss overlay. */
async function withSidebar(
  page: Page,
  mobile: boolean,
  viewport: { width: number; height: number },
  run: () => Promise<void>,
) {
  if (mobile) {
    await page.locator('.menu-btn').first().tap()
    await page.waitForTimeout(300) // slide-in transition
  }
  await run()
  if (mobile) {
    await page.touchscreen.tap(viewport.width - 8, Math.floor(viewport.height / 2))
    await page.waitForTimeout(300)
  }
}

const promoted = async () =>
  (await listSessions()).find(s => s.id === childId)?.promoted_to_root === true

for (const [name, viewport, mobile] of [
  ['desktop', { width: 1200, height: 800 }, false],
  ['portrait', { width: 390, height: 844 }, true],
  ['landscape', { width: 844, height: 390 }, true],
] as const) {
  test.describe(`promote/demote round trip (${name})`, () => {
    test.use({ viewport, hasTouch: mobile, isMobile: mobile })

    test('menu promote gives the child its own row; return regroups it', async ({ page }) => {
      const shot = (stage: string) =>
        page.screenshot({ path: path.join(SCREENSHOT_DIR, `daemon-${name}-${stage}.png`) })
      await openApp(page)
      await gotoSession(page, childId)

      // The child's own root row, matched by title: hrefs may serialize as
      // a slug rather than the immutable id, so the id is not in the URL.
      const childRow = page.locator('.session-item')
        .filter({ has: page.locator(`.session-title:text-is("${childTitle}")`) })
      const slotRow = page.locator('.family-slot.selected')

      // Before: the child is a family member — the sidebar shows it only as
      // the family entry's member row beneath the stub-claude root.
      await withSidebar(page, mobile, viewport, async () => {
        await expect(slotRow).toHaveCount(1)
        await expect(childRow).toHaveCount(0)
        await shot('1-before')
      })

      // Promote through the ⋮ menu.
      await page.locator('.session-menu-trigger').click()
      const item = page.locator('.session-menu-promotion')
      await expect(item).toContainText('Promote to root')
      await shot('2-menu')
      await item.click()
      await expect.poll(promoted, { timeout: 10_000 }).toBe(true)

      // The router stays on the session (no bounce home / false root state).
      expect(page.url()).toContain('/test-project/')
      await expect(page.locator('.xterm')).toBeVisible()

      // After: its own selected root row; the member slot row is gone.
      await withSidebar(page, mobile, viewport, async () => {
        await expect(childRow).toHaveCount(1)
        await expect(page.locator('.session-item.selected')).toHaveCount(1)
        await expect(slotRow).toHaveCount(0)
        await shot('3-promoted')
      })

      // Demote: the menu now offers the way back, naming the family.
      await page.locator('.session-menu-trigger').click()
      const demoteItem = page.locator('.session-menu-promotion')
      await expect(demoteItem).toContainText('Return to family')
      await demoteItem.click()
      await expect.poll(async () => !(await promoted()), { timeout: 10_000 }).toBe(true)

      expect(page.url()).toContain('/test-project/')
      await withSidebar(page, mobile, viewport, async () => {
        await expect(slotRow).toHaveCount(1)
        await expect(childRow).toHaveCount(0)
        await shot('4-demoted')
      })
    })
  })
}

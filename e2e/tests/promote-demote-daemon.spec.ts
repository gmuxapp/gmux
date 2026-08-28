import { test, expect, type Page } from '@playwright/test'
import { spawn, type ChildProcess } from 'child_process'
import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'
import { gotoSession, openApp, pollUntil } from '../helpers'

/**
 * The actual promote/reparent mutation against the disposable test daemon:
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

async function post(pathname: string, body?: unknown): Promise<number> {
  const { base, headers } = api()
  const resp = await fetch(`${base}${pathname}`, {
    method: 'POST',
    headers: body === undefined ? headers : { ...headers, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  return resp.status
}

async function putProjects(items: unknown[]): Promise<number> {
  const { base, headers } = api()
  const resp = await fetch(`${base}/v1/projects`, {
    method: 'PUT',
    headers: { ...headers, 'Content-Type': 'application/json' },
    body: JSON.stringify({ items }),
  })
  return resp.status
}

function spawnGmux(args: string[], cwd: string, extraEnv: Record<string, string> = {}): ChildProcess {
  return spawn(GMUX, args, {
    env: { ...env, ...extraEnv }, cwd, stdio: ['ignore', 'pipe', 'pipe'], detached: true,
  })
}

let parentProc: ChildProcess | undefined
let childProc: ChildProcess | undefined
let parentId = ''
let childId = ''
let childTitle = ''
let env: Record<string, string>
let tmpDir = ''
const extraProcs: ChildProcess[] = []
const extraIds: string[] = []

test.describe.configure({ mode: 'serial' })

test.beforeAll(async () => {
  const state = JSON.parse(fs.readFileSync(STATE_FILE, 'utf-8')) as { tmpDir: string }
  tmpDir = state.tmpDir
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
  expect(child.parent_session_id).toBe(parentId)

  fs.mkdirSync(SCREENSHOT_DIR, { recursive: true })
})

test.afterAll(async () => {
  // Leave the shared daemon exactly as the other specs expect it: restore
  // the seeded single-project catalog, kill and dismiss every spawned
  // session, then reap the runner processes.
  await putProjects([
    { slug: 'test-project', match: [{ path: process.env.GMUX_TEST_WORKSPACE! }] },
  ]).catch(() => {})
  for (const id of [...extraIds, childId, parentId]) {
    if (!id) continue
    await post(`/v1/sessions/${id}/kill`).catch(() => {})
  }
  await new Promise(r => setTimeout(r, 500))
  for (const id of [...extraIds, childId, parentId]) {
    if (!id) continue
    await post(`/v1/sessions/${id}/dismiss`).catch(() => {})
  }
  for (const proc of [...extraProcs, childProc, parentProc]) {
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

// Single-axis promotion: a promoted session has no current parent at all.
const promoted = async () => {
  const child = (await listSessions()).find(s => s.id === childId)
  return !!child && child.parent_session_id === undefined
}

for (const [name, viewport, mobile] of [
  ['desktop', { width: 1200, height: 800 }, false],
  ['portrait', { width: 390, height: 844 }, true],
  ['landscape', { width: 844, height: 390 }, true],
] as const) {
  test.describe(`promote/reparent round trip (${name})`, () => {
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
      // The success announcement is authoritative and survives the dropdown
      // closure; this assertion fails if only the promote direction is wired.
      await expect(page.locator('[data-promotion-status]')).toHaveText('Promoted to root.')

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
      await expect(page.locator('[data-promotion-status]')).toHaveText('Returned to family.')

      expect(page.url()).toContain('/test-project/')
      await withSidebar(page, mobile, viewport, async () => {
        await expect(slotRow).toHaveCount(1)
        await expect(childRow).toHaveCount(0)
        await shot('4-demoted')
      })
    })
  })
}

test.describe('promotion placement edges (real daemon, desktop)', () => {
  test('a child outside every project offers Promote blocked and never strands', async ({ page }) => {
    // The daemon leaves a session unplaced when no project claims its cwd,
    // and promotion cannot invent a placement (parentage never overrides
    // matching). Promoting would therefore strand the session: no sidebar
    // row, dead URL, no menu to demote from. The UI refuses with the reason.
    const outsideDir = path.join(tmpDir, 'outside')
    fs.mkdirSync(outsideDir, { recursive: true })
    extraProcs.push(spawnGmux(['--', 'sh', '-c', 'echo outside ready; sleep 600'], outsideDir, { GMUX_SESSION_ID: parentId }))
    const outside = await pollUntil(async () =>
      (await listSessions()).find(s => s.alive && s.parent_session_id === parentId && s.cwd === outsideDir),
    { timeoutMs: 15_000, description: 'outside child registered' })
    extraIds.push(outside.id)

    await openApp(page)
    await gotoSession(page, outside.id)
    expect(page.url()).toContain('/test-project/') // presents under its family root

    await page.locator('.session-menu-trigger').click()
    const item = page.locator('.session-menu-promotion')
    await expect(item).not.toHaveAttribute('disabled')
    await expect(item).toHaveAttribute('aria-disabled', 'true')
    await expect(item).toContainText('Promote to root')
    await expect(item.locator('.session-menu-action-note')).toContainText('Needs a project')
    await item.focus()
    await expect(item).toBeFocused()
    await item.press('Enter')
    await page.waitForTimeout(300)
    // No mutation left the browser; the daemon still reports the family
    // edge intact, and the session is exactly where it was.
    expect((await listSessions()).find(s => s.id === outside.id)?.parent_session_id).toBe(parentId)
    expect(page.url()).toContain('/test-project/')
    await expect(page.locator('.xterm')).toBeVisible()

    // The CLI/API can still promote such a session. Pin the documented
    // consequence — an unplaced promoted root renders nowhere, like any
    // unplaced session — and the recovery: demote restores the family route.
    expect(await post(`/v1/sessions/${outside.id}/reparent`, { parent_session_id: null })).toBe(200)
    await pollUntil(async () =>
      (await listSessions()).find(s => s.id === outside.id)?.parent_session_id === undefined,
    { timeoutMs: 10_000, description: 'promoted via API' })
    await expect.poll(
      () => page.evaluate(id => (window as any).__gmuxNavigateToSession(id), outside.id),
      { timeout: 10_000 },
    ).toBe(false) // unroutable while promoted and unplaced
    expect(await post(`/v1/sessions/${outside.id}/reparent`, { parent_session_id: parentId })).toBe(200)
    await expect.poll(
      () => page.evaluate(id => (window as any).__gmuxNavigateToSession(id), outside.id),
      { timeout: 10_000 },
    ).toBe(true) // demote makes it reachable again
    await expect(page.locator('.xterm')).toBeVisible()
  })

  test('blocks demote when the family root is outside every project', async ({ page }) => {
    // Mirror the promote-stranding schedule: the promoted child is stamped and
    // routable, but returning it would rejoin an unplaced parent.
    const outsideParentDir = path.join(tmpDir, 'outside-parent')
    fs.mkdirSync(outsideParentDir, { recursive: true })
    extraProcs.push(spawnGmux(['--', 'claude'], outsideParentDir))
    const outsideParent = await pollUntil(async () =>
      (await listSessions()).find(s => s.alive && s.adapter === 'claude' && s.cwd === outsideParentDir),
    { timeoutMs: 15_000, description: 'outside semantic parent registered' })
    expect(outsideParent.semantic_agent).toBe(true)
    extraIds.push(outsideParent.id)

    extraProcs.push(spawnGmux(['--', 'sh', '-c', 'echo placed child; sleep 600'], process.env.GMUX_TEST_WORKSPACE!, {
      GMUX_SESSION_ID: outsideParent.id,
    }))
    const placedChild = await pollUntil(async () =>
      (await listSessions()).find(s => s.alive && s.parent_session_id === outsideParent.id && s.cwd === process.env.GMUX_TEST_WORKSPACE),
    { timeoutMs: 15_000, description: 'placed child under outside parent registered' })
    extraIds.push(placedChild.id)
    expect(placedChild.project_slug).toBe('test-project')

    expect(await post(`/v1/sessions/${placedChild.id}/reparent`, { parent_session_id: null })).toBe(200)
    await pollUntil(async () =>
      (await listSessions()).find(s => s.id === placedChild.id)?.parent_session_id === undefined,
    { timeoutMs: 10_000, description: 'placed child promoted' })

    await openApp(page)
    await gotoSession(page, placedChild.id)
    expect(page.url()).toContain('/test-project/')
    const posts: string[] = []
    await page.route(`**/v1/sessions/${placedChild.id}/reparent`, async route => {
      posts.push(route.request().url())
      await route.fulfill({ status: 200, body: '{}' })
    })
    await page.locator('.session-menu-trigger').click()
    const item = page.locator('.session-menu-promotion')
    await expect(item).toContainText('Return to family')
    await expect(item).toHaveAttribute('aria-disabled', 'true')
    await expect(item.locator('.session-menu-action-note')).toContainText('family root has no project-backed sidebar row')
    await item.focus()
    await item.press('Enter')
    await expect.poll(() => posts).toHaveLength(0)
    expect((await listSessions()).find(s => s.id === placedChild.id)?.parent_session_id).toBe(undefined)
    expect(page.url()).toContain('/test-project/')
    await expect(page.locator('.xterm')).toBeVisible()
  })

  test('cross-project promote moves the URL and sidebar row; demote returns them', async ({ page }) => {
    const workspaceB = path.join(tmpDir, 'workspace-b')
    fs.mkdirSync(workspaceB, { recursive: true })
    expect(await putProjects([
      { slug: 'test-project', match: [{ path: process.env.GMUX_TEST_WORKSPACE! }] },
      { slug: 'project-b', match: [{ path: workspaceB }] },
    ])).toBe(200)
    extraProcs.push(spawnGmux(['--', 'sh', '-c', 'echo crosser ready; sleep 600'], workspaceB, { GMUX_SESSION_ID: parentId }))
    const crosser = await pollUntil(async () =>
      (await listSessions()).find(s => s.alive && s.parent_session_id === parentId && s.cwd === workspaceB),
    { timeoutMs: 15_000, description: 'cross-project child registered' })
    extraIds.push(crosser.id)

    await openApp(page)
    await gotoSession(page, crosser.id)
    // Unpromoted: presents through its family root's project.
    expect(page.url()).toContain('/test-project/')

    await page.locator('.session-menu-trigger').click()
    const item = page.locator('.session-menu-promotion')
    await expect(item).toContainText('Promote to root')
    await item.click()
    // The authoritative snapshot moves the URL to the child's own project in
    // the same commit; the view never leaves the session.
    await expect(page).toHaveURL(/\/project-b\//)
    await expect(page.locator('.xterm')).toBeVisible()
    const folderB = page.locator('.folder').filter({ hasText: 'project-b' })
    await expect(folderB.locator('.session-item.selected')).toHaveCount(1)

    // Return to family: URL and folder revert.
    await page.locator('.session-menu-trigger').click()
    await expect(page.locator('.session-menu-promotion')).toContainText('Return to family')
    await page.locator('.session-menu-promotion').click()
    await expect(page).toHaveURL(/\/test-project\//)
    await expect(folderB.locator('.session-item')).toHaveCount(0)
    await expect(page.locator('.family-slot.selected')).toHaveCount(1)
  })
})

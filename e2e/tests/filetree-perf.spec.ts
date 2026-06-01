/**
 * File-tree performance regression tests.
 *
 * Runs in a real browser (Chromium) against a live gmuxd instance.
 * All timing is measured browser-side via the Long Tasks API — not
 * wall-clock from the test runner.
 *
 * WHAT WE ARE ACTUALLY TESTING
 * ─────────────────────────────
 * The regression (db67a38 + 99ec273) is not slow click→navigate execution.
 * navigate() itself takes < 1 ms. The problem is the synchronous work that
 * runs on the main thread after every `await apiWalkPaths()` resolves:
 *
 *   getExpandedPaths(model, dirPaths)   ← O(n) model.getItem() per dir
 *   model.resetPaths(paths, …)          ← tree model update
 *
 * This fires every 2.5 s on the poll interval. On a project with 95 000+
 * directories (representative of a large monorepo) it can block the main
 * thread for hundreds of milliseconds. Any click dispatched during that
 * window sits in the event queue until the block clears.
 *
 * HOW WE DETECT IT
 * ─────────────────
 * The browser's Long Tasks API reports any task that blocks the event loop
 * for > 50 ms. We intercept the walk API response via page.route() and
 * inject synthetic path lists (100 000 and 1 000 000 entries) so we can
 * test at scale without creating real directories on disk. The browser's
 * JS processing path is identical regardless of whether paths came from the
 * real filesystem or a synthetic response.
 *
 * Test structure:
 *   1. Install PerformanceObserver for "longtask" before any app code runs.
 *   2. Intercept /v1/fs/{slug}/walk to return N synthetic directory paths.
 *   3. Load the project hub; let the tree render (initial load long tasks excluded).
 *   4. Wait for ≥ 2 poll cycles (6 s) — each one re-fetches the intercepted walk.
 *   5. Assert zero long tasks recorded after initial load completes.
 *
 * If the regression is present the poll work WILL produce long tasks and
 * the tests will fail. If the fix holds, no long tasks appear.
 */

import * as fs from 'node:fs'
import * as path from 'node:path'
import { test, expect, type Page } from '@playwright/test'
import { openApp } from '../helpers'

// ── Constants ─────────────────────────────────────────────────────────────────

const PROJECT = 'test-project'

// ── Helpers ───────────────────────────────────────────────────────────────────

function workspace(): string {
  const w = process.env.GMUX_TEST_WORKSPACE
  if (!w) throw new Error('GMUX_TEST_WORKSPACE not set; global-setup did not run')
  return w
}

/**
 * Install the Long Tasks observer and click/nav timing hooks via addInitScript
 * (runs before any app code, so nothing is missed).
 *
 *   window.__perf.longTasks  — entries from Long Tasks API (duration > 50 ms)
 *   window.__perf.clickTime  — capture-phase click timestamp
 *   window.__perf.navTime    — history.pushState / replaceState timestamp
 */
async function installPerfHooks(page: Page): Promise<void> {
  await page.addInitScript(() => {
    ;(window as any).__perf = {
      longTasks: [] as Array<{ duration: number; startTime: number }>,
      longTasksSupported: false,
      clickTime: null as number | null,
      navTime:   null as number | null,
    }

    try {
      const obs = new PerformanceObserver((list) => {
        for (const e of list.getEntries()) {
          ;(window as any).__perf.longTasks.push({
            duration:  e.duration,
            startTime: e.startTime,
          })
        }
      })
      obs.observe({ type: 'longtask', buffered: true })
      ;(window as any).__perf.longTasksSupported = true
    } catch { /* not supported in this browser */ }

    document.addEventListener(
      'click',
      () => { ;(window as any).__perf.clickTime = performance.now() },
      { capture: true, passive: true },
    )

    const origPush    = history.pushState.bind(history)
    const origReplace = history.replaceState.bind(history)
    history.pushState = (...args) => {
      ;(window as any).__perf.navTime = performance.now()
      return origPush(...args)
    }
    history.replaceState = (...args) => {
      ;(window as any).__perf.navTime ??= performance.now()
      return origReplace(...args)
    }
  })
}

/**
 * Intercept every walk API call and return a synthetic list of `count`
 * directory paths. The full-walk variant (?full=true) returns an empty
 * additions list so only the fast-walk processing is exercised.
 *
 * This replaces disk-seeding entirely: the browser's getExpandedPaths +
 * resetPaths sees the same data structure regardless of whether paths came
 * from the real filesystem or this synthetic response.
 */
async function interceptWalk(page: Page, count: number): Promise<void> {
  // Build the path list once in the test runner process; serialise to JSON.
  const paths: string[] = []
  for (let i = 0; i < count; i++) {
    paths.push(`dir-${String(i).padStart(7, '0')}/`)
  }
  const body = JSON.stringify({ ok: true, data: paths })

  await page.route(`**/v1/fs/**/walk**`, async (route) => {
    const url = route.request().url()
    if (url.includes('full=true')) {
      // Full walk: return empty additions so the batch() path is a no-op.
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ ok: true, data: [] }),
      })
    } else {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body,
      })
    }
  })
}

async function openProjectHub(page: Page): Promise<void> {
  await openApp(page, `/${PROJECT}`)
  await page.locator('.ft-root').waitFor({ state: 'visible', timeout: 10_000 })
}

/**
 * Core long-task assertion: load the hub with a synthetic walk of `count`
 * paths, wait for the tree to render, then wait for ≥ 2 poll cycles and
 * assert no task blocked the main thread for > 50 ms.
 */
async function assertNoPollLongTasks(page: Page, count: number): Promise<void> {
  await interceptWalk(page, count)
  await installPerfHooks(page)
  await openProjectHub(page)

  // Wait for at least one treeitem (first synthetic dir) to confirm the walk
  // response was processed and the tree rendered.
  await page.getByRole('treeitem').first().waitFor({ state: 'visible', timeout: 15_000 })

  // Exclude long tasks from initial load — only poll-phase tasks matter.
  const treeReadyAt = await page.evaluate(() => performance.now())

  // Wait for ≥ 2 complete poll cycles (2 × 2.5 s = 5 s, plus margin).
  await page.waitForTimeout(6_000)

  const { longTasks, longTasksSupported } = await page.evaluate(() => ({
    longTasks:          (window as any).__perf.longTasks as Array<{ duration: number; startTime: number }>,
    longTasksSupported: (window as any).__perf.longTasksSupported as boolean,
  }))

  if (!longTasksSupported) {
    test.skip(true, 'PerformanceObserver longtask not supported in this browser')
    return
  }

  const pollLongTasks = longTasks.filter(t => t.startTime > treeReadyAt)

  if (pollLongTasks.length > 0) {
    const worst = Math.max(...pollLongTasks.map(t => t.duration))
    console.log(
      `[perf] FAIL (${count.toLocaleString()} dirs): ${pollLongTasks.length} long task(s), worst ${worst.toFixed(0)} ms`,
    )
  } else {
    console.log(`[perf] OK (${count.toLocaleString()} dirs): no long tasks during poll`)
  }

  expect(
    pollLongTasks,
    `Poll cycle blocked main thread with ${count.toLocaleString()} dirs: ` +
    JSON.stringify(pollLongTasks.map(t => `${t.duration.toFixed(0)}ms`)),
  ).toHaveLength(0)
}

// ── Tests ─────────────────────────────────────────────────────────────────────

test.describe('file-tree performance — poll cycle blocking (Long Tasks API)', () => {

  test('100 000 dirs: poll cycle does not block main thread >50 ms', async ({ page }) => {
    test.setTimeout(60_000)
    await assertNoPollLongTasks(page, 100_000)
  })

  test('1 000 000 dirs: poll cycle does not block main thread >50 ms', async ({ page }) => {
    test.setTimeout(120_000)
    await assertNoPollLongTasks(page, 1_000_000)
  })

})

test.describe('file-tree performance — session switch (Long Tasks API)', () => {

  test('session switch with 100 000 dirs does not block main thread >50 ms', async ({ page }) => {
    test.setTimeout(60_000)

    const sessionId = process.env.GMUX_TEST_SESSION_ID
    if (!sessionId) {
      test.skip(true, 'GMUX_TEST_SESSION_ID not set')
      return
    }

    await interceptWalk(page, 100_000)
    await installPerfHooks(page)
    await openProjectHub(page)
    await page.getByRole('treeitem').first().waitFor({ state: 'visible', timeout: 15_000 })

    // Reset long task list immediately before the switch.
    await page.evaluate(() => { ;(window as any).__perf.longTasks = [] })

    await page.waitForFunction((id) => {
      const nav = (window as any).__gmuxNavigateToSession
      return typeof nav === 'function' && nav(id) === true
    }, sessionId, { timeout: 10_000 })

    await page.locator('.terminal-container.wterm').waitFor({ state: 'visible', timeout: 8_000 })
    // Give the tree one poll tick to reload after the switch.
    await page.waitForTimeout(3_000)

    const longTasks = await page.evaluate(
      () => (window as any).__perf.longTasks as Array<{ duration: number; startTime: number }>,
    )

    if (longTasks.length > 0) {
      const worst = Math.max(...longTasks.map(t => t.duration))
      console.log(`[perf] FAIL session switch: ${longTasks.length} long task(s), worst ${worst.toFixed(0)} ms`)
    } else {
      console.log('[perf] OK session switch: no long tasks')
    }

    expect(
      longTasks,
      `Session switch blocked main thread: ${JSON.stringify(longTasks.map(t => `${t.duration.toFixed(0)}ms`))}`,
    ).toHaveLength(0)
  })

})

test.describe('file-tree performance — click handler latency', () => {
  // These measure JS handler execution time (click event → navigate() →
  // history.pushState). This is NOT input queue delay; it is the cost of
  // the selection handler itself. Input queue delay under load is covered
  // by the long-tasks tests above.

  test('click handler executes in <20 ms on a normal tree', async ({ page }) => {
    fs.writeFileSync(path.join(workspace(), 'perf-handler-click.md'), '# test')

    await installPerfHooks(page)
    await openProjectHub(page)

    const item = page.getByRole('treeitem', { name: 'perf-handler-click.md' })
    await item.waitFor({ state: 'visible', timeout: 5_000 })

    await page.evaluate(() => { ;(window as any).__perf.navTime = null })
    await item.click()

    const { clickTime, navTime } = await page.evaluate(() => ({
      clickTime: (window as any).__perf.clickTime as number,
      navTime:   (window as any).__perf.navTime   as number,
    }))

    expect(navTime, 'history.pushState never called after click').not.toBeNull()
    const latency = navTime - clickTime
    console.log(`[perf] handler exec: ${latency.toFixed(2)} ms`)
    expect(latency).toBeLessThan(20)
  })

})

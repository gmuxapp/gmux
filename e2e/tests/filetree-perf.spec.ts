/**
 * File-tree performance regression tests.
 *
 * These tests run in a real browser (Chromium) against a live gmuxd instance.
 * Timing is measured browser-side using the Long Tasks API and the
 * Performance API — not wall-clock time from the test runner.
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
 * This fires every 2.5 s on the poll interval. With 300+ dirs it can block
 * the main thread for 100–400 ms. Any click dispatched during that window
 * sits in the event queue until the block clears — so the user sees a frozen
 * UI, even though navigate() itself is instant.
 *
 * HOW WE DETECT IT
 * ─────────────────
 * The browser's Long Tasks API reports any task that blocks the event loop
 * for > 50 ms. We:
 *
 *   1. Install a PerformanceObserver for "longtask" entries via addInitScript
 *      (runs before any app code, so nothing is missed).
 *   2. Seed 300 directories so the walk result is large.
 *   3. Let the tree load, then wait for ≥ 2 complete poll cycles (6 s).
 *   4. Assert that no long task was recorded after the initial page load.
 *
 * If the regression is present, long tasks WILL appear. If the fix holds,
 * the poll work stays < 50 ms and no tasks are reported.
 *
 * We also retain a click→navigate handler-latency test — this measures the
 * JS execution path (navigate() call), not input queue delay. Its value is
 * as a sanity check that the selection handler itself hasn't grown expensive.
 */

import * as fs from 'node:fs'
import * as path from 'node:path'
import { test, expect, type Page } from '@playwright/test'
import { openApp } from '../helpers'

// ── Constants ─────────────────────────────────────────────────────────────────

const PROJECT = 'test-project'

/**
 * Any task blocking the main thread for longer than this is a regression.
 * The Long Tasks API reports tasks > 50 ms; we use 50 ms as our budget.
 */
const MAIN_THREAD_BLOCK_BUDGET_MS = 50

/**
 * Handler execution budget: time from click event to navigate() calling
 * history.pushState. Measures pure JS execution, not input queue delay.
 */
const HANDLER_EXEC_BUDGET_MS = 20

// ── Helpers ───────────────────────────────────────────────────────────────────

function workspace(): string {
  const w = process.env.GMUX_TEST_WORKSPACE
  if (!w) throw new Error('GMUX_TEST_WORKSPACE not set; global-setup did not run')
  return w
}

/**
 * Install two observers via addInitScript (runs before any app code):
 *
 *   window.__perf.longTasks  — Long Tasks API entries (each blocks > 50 ms)
 *   window.__perf.clickTime  — capture-phase click timestamp
 *   window.__perf.navTime    — history.pushState / replaceState timestamp
 */
async function installPerfHooks(page: Page): Promise<void> {
  await page.addInitScript(() => {
    ;(window as any).__perf = {
      longTasks: [] as Array<{ duration: number; startTime: number }>,
      clickTime: null as number | null,
      navTime:   null as number | null,
    }

    // Long Tasks: reports any task that blocks the main thread > 50 ms.
    // This is the primary regression signal — catches getExpandedPaths +
    // resetPaths blocking the event loop on the poll cycle.
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
    } catch {
      // Long Tasks not supported — test will skip the assertion below.
    }

    // Click timing: capture-phase fires before any component handler.
    document.addEventListener(
      'click',
      () => { ;(window as any).__perf.clickTime = performance.now() },
      { capture: true, passive: true },
    )

    // Navigate timing: intercept history mutations called by preact-iso's
    // loc.route() — which is what navigate() delegates to.
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

/** Seed 300 directories into the test workspace. Idempotent. */
function seedLargeTree(): void {
  const bigDir = path.join(workspace(), 'perf-big-tree')
  if (!fs.existsSync(bigDir)) fs.mkdirSync(bigDir)
  for (let i = 0; i < 300; i++) {
    const d = path.join(bigDir, `dir-${String(i).padStart(4, '0')}`)
    if (!fs.existsSync(d)) fs.mkdirSync(d)
  }
}

async function openProjectHub(page: Page): Promise<void> {
  await openApp(page, `/${PROJECT}`)
  await page.locator('.ft-root').waitFor({ state: 'visible', timeout: 10_000 })
}

// ── Tests ─────────────────────────────────────────────────────────────────────

test.describe('file-tree performance — main thread blocking', () => {
  // ── 1. Poll cycle with a large tree must not produce long tasks ───────────

  test('poll cycle does not block main thread >50 ms with 300 directories', async ({ page }) => {
    seedLargeTree()
    fs.writeFileSync(path.join(workspace(), 'perf-big-tree-sentinel.txt'), 'x')

    await installPerfHooks(page)
    await openProjectHub(page)

    // Wait for the large tree dir to appear so the initial walk has completed.
    await page.getByRole('treeitem', { name: 'perf-big-tree', exact: true })
      .waitFor({ state: 'visible', timeout: 10_000 })

    // Record the time after initial load so we can exclude startup long tasks.
    const treeReadyAt = await page.evaluate(() => performance.now())

    // Wait for at least 2 full poll cycles (2 × 2.5 s = 5 s, plus margin).
    await page.waitForTimeout(6_000)

    const { longTasks, longTasksSupported } = await page.evaluate(() => {
      const p = (window as any).__perf
      return {
        longTasks: p.longTasks as Array<{ duration: number; startTime: number }>,
        longTasksSupported: p.longTasks !== undefined,
      }
    })

    if (!longTasksSupported) {
      test.skip(true, 'PerformanceObserver longtask not supported in this browser')
      return
    }

    // Exclude long tasks from the initial page load; only poll-phase tasks matter.
    const pollLongTasks = longTasks.filter(t => t.startTime > treeReadyAt)

    if (pollLongTasks.length > 0) {
      const worst = Math.max(...pollLongTasks.map(t => t.duration))
      console.log(
        `[perf] FAIL: ${pollLongTasks.length} long task(s) during poll, worst: ${worst.toFixed(1)} ms`,
        pollLongTasks,
      )
    } else {
      console.log('[perf] OK: no long tasks during poll cycles')
    }

    expect(
      pollLongTasks,
      `Poll cycle blocked main thread: ${JSON.stringify(pollLongTasks.map(t => `${t.duration.toFixed(0)}ms`))}`,
    ).toHaveLength(0)
  })

  // ── 2. Session switch must not produce long tasks (tree reload path) ──────

  test('session switch does not block main thread >50 ms', async ({ page }) => {
    seedLargeTree()

    const sessionId = process.env.GMUX_TEST_SESSION_ID
    if (!sessionId) {
      test.skip(true, 'GMUX_TEST_SESSION_ID not set')
      return
    }

    await installPerfHooks(page)
    await openProjectHub(page)
    await page.getByRole('treeitem', { name: 'perf-big-tree', exact: true })
      .waitFor({ state: 'visible', timeout: 10_000 })

    // Reset long task list to zero just before the switch.
    await page.evaluate(() => { ;(window as any).__perf.longTasks = [] })

    // Trigger a session switch (causes project change → full tree reload).
    await page.waitForFunction((id) => {
      const nav = (window as any).__gmuxNavigateToSession
      return typeof nav === 'function' && nav(id) === true
    }, sessionId, { timeout: 10_000 })

    // Wait for the terminal to settle.
    await page.locator('.terminal-container.wterm').waitFor({ state: 'visible', timeout: 8_000 })
    // Give the tree reload one extra poll tick to complete.
    await page.waitForTimeout(3_000)

    const longTasks = await page.evaluate(
      () => (window as any).__perf.longTasks as Array<{ duration: number; startTime: number }>,
    )

    if (longTasks.length > 0) {
      const worst = Math.max(...longTasks.map(t => t.duration))
      console.log(`[perf] FAIL: long task during session switch: worst ${worst.toFixed(1)} ms`)
    } else {
      console.log('[perf] OK: no long tasks during session switch')
    }

    expect(
      longTasks,
      `Session switch blocked main thread: ${JSON.stringify(longTasks.map(t => `${t.duration.toFixed(0)}ms`))}`,
    ).toHaveLength(0)
  })
})

test.describe('file-tree performance — selection handler latency', () => {
  // NOTE: these tests measure the JS execution time of the click handler
  // (click event → navigate() → history.pushState). This is NOT input queue
  // latency. For input queue latency under load, see the long-tasks tests above.

  // ── 3. Handler execution time: normal tree ────────────────────────────────

  test('click handler executes in <20 ms on a normal-sized tree', async ({ page }) => {
    fs.writeFileSync(path.join(workspace(), 'perf-handler-normal.md'), '# test')

    await installPerfHooks(page)
    await openProjectHub(page)

    const item = page.getByRole('treeitem', { name: 'perf-handler-normal.md' })
    await item.waitFor({ state: 'visible', timeout: 5_000 })

    await page.evaluate(() => { ;(window as any).__perf.navTime = null })
    await item.click()

    const { clickTime, navTime } = await page.evaluate(() => ({
      clickTime: (window as any).__perf.clickTime as number,
      navTime:   (window as any).__perf.navTime   as number,
    }))

    expect(navTime, 'history.pushState never called').not.toBeNull()
    const latency = navTime - clickTime
    console.log(`[perf] handler exec (normal tree): ${latency.toFixed(2)} ms`)
    expect(latency).toBeLessThan(HANDLER_EXEC_BUDGET_MS)
  })

  // ── 4. Handler execution time: large tree ────────────────────────────────

  test('click handler executes in <20 ms with 300 directories in the tree', async ({ page }) => {
    seedLargeTree()
    fs.writeFileSync(path.join(workspace(), 'perf-handler-large.md'), '# large')

    await installPerfHooks(page)
    await openProjectHub(page)

    await page.getByRole('treeitem', { name: 'perf-big-tree', exact: true })
      .waitFor({ state: 'visible', timeout: 10_000 })
    const item = page.getByRole('treeitem', { name: 'perf-handler-large.md' })
    await item.waitFor({ state: 'visible', timeout: 5_000 })

    await page.evaluate(() => { ;(window as any).__perf.navTime = null })
    await item.click()

    const { clickTime, navTime } = await page.evaluate(() => ({
      clickTime: (window as any).__perf.clickTime as number,
      navTime:   (window as any).__perf.navTime   as number,
    }))

    expect(navTime, 'history.pushState never called').not.toBeNull()
    const latency = navTime - clickTime
    console.log(`[perf] handler exec (300-dir tree): ${latency.toFixed(2)} ms`)
    expect(latency).toBeLessThan(HANDLER_EXEC_BUDGET_MS)
  })
})

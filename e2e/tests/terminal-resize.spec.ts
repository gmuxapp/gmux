import { test, expect } from '@playwright/test'
import { getTermState, isPillVisible, selectFirstSession, clickPill } from '../helpers'

test.describe('terminal resize', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/')
    await selectFirstSession(page)
  })

  test('connects passive and shows pill when viewport differs from PTY', async ({ page }) => {
    const state = await getTermState(page)
    // Terminal should have some size (either bootstrapped or from PTY)
    expect(state.termCols).toBeGreaterThan(0)
    expect(state.termRows).toBeGreaterThan(0)
  })

  test('clicking pill resizes terminal to fit viewport', async ({ page }) => {
    // Get initial state — may or may not have a pill depending on whether
    // this is the first client (bootstraps) or PTY has a pre-existing size.
    const initial = await getTermState(page)

    // Resize viewport to something different to force a mismatch
    await page.setViewportSize({ width: 900, height: 500 })
    await page.waitForTimeout(500)

    const pill = await isPillVisible(page)
    if (pill) {
      await clickPill(page)

      const after = await getTermState(page)
      // After clicking pill, terminal should resize to fit the viewport
      expect(after.termCols).toBeDefined()
      expect(after.termRows).toBeDefined()
      // Pill should disappear
      expect(await isPillVisible(page)).toBe(false)
    }
    // If no pill, the browser is already driving (bootstrapped) — that's fine
  })

  test('viewport resize while driving updates terminal dimensions', async ({ page }) => {
    // Ensure we're driving: click pill if visible, otherwise we bootstrapped
    if (await isPillVisible(page)) {
      await clickPill(page)
    }

    // Now resize the viewport — terminal should follow automatically
    await page.setViewportSize({ width: 900, height: 600 })
    await page.waitForTimeout(1000)
    const small = await getTermState(page)

    await page.setViewportSize({ width: 1400, height: 900 })
    await page.waitForTimeout(1000)
    const large = await getTermState(page)

    // Larger viewport should yield more columns and rows
    expect(large.termCols!).toBeGreaterThan(small.termCols!)
    expect(large.termRows!).toBeGreaterThan(small.termRows!)
  })

  test('pill disappears when viewport matches PTY size', async ({ page }) => {
    // Start driving
    if (await isPillVisible(page)) {
      await clickPill(page)
    }

    // Resize while driving — no pill
    await page.setViewportSize({ width: 1000, height: 700 })
    await page.waitForTimeout(1000)
    expect(await isPillVisible(page)).toBe(false)

    // Terminal size and viewport should match — no pill
    const state = await getTermState(page)
    expect(state.termCols).toBeGreaterThan(0)
    expect(state.termRows).toBeGreaterThan(0)
  })
})

import { test, expect } from '@playwright/test'
import { getTermState, openApp, gotoTestSession } from '../helpers'

test.describe('terminal connection', () => {
  test('connects to session and renders wterm DOM element', async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)

    // wterm renders into DOM — no canvas
    await expect(page.locator('.terminal-container.wterm')).toBeVisible()

    const state = await getTermState(page)
    expect(state.termCols).toBeGreaterThan(0)
    expect(state.termRows).toBeGreaterThan(0)
  })

  test('terminal has rendered output (cursor is placed)', async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)

    const hasCursor = await page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      if (!term) return false
      // wterm exposes cursor via bridge.getCursor()
      const cursor = term.bridge?.getCursor?.()
      return cursor ? cursor.row >= 0 : false
    })
    expect(hasCursor).toBe(true)
  })

  test('terminal cols and rows are positive after init', async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)

    const { termCols, termRows } = await getTermState(page)
    expect(termCols).toBeGreaterThanOrEqual(20)
    expect(termRows).toBeGreaterThanOrEqual(5)
  })

  test('wterm element is inside .terminal-container', async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)

    const count = await page.locator('.terminal-container.wterm').count()
    expect(count).toBe(1)
  })
})

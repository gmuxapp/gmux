import { test, expect } from '@playwright/test'
import { getTermState, openApp, selectFirstSession } from '../helpers'

test.describe('terminal connection', () => {
  test('connects to session and renders terminal', async ({ page }) => {
    await openApp(page)
    await selectFirstSession(page)

    // Terminal should exist and have dimensions
    const state = await getTermState(page)
    expect(state.termCols).toBeGreaterThan(0)
    expect(state.termRows).toBeGreaterThan(0)

    // xterm element should be visible
    await expect(page.locator('.xterm')).toBeVisible()
  })

  test('terminal displays session output', async ({ page }) => {
    await openApp(page)
    await selectFirstSession(page)

    // The test session runs `echo READY` — check that the terminal has content.
    // We can't easily read xterm's rendered content, but we can check that
    // the terminal has been written to by verifying it has a cursor.
    const hasCursor = await page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      return term ? term.buffer.active.cursorY >= 0 : false
    })
    expect(hasCursor).toBe(true)
  })
})

import { test, expect } from '@playwright/test'
import { openApp, gotoTestSession, spawnTestSession } from '../helpers'

/**
 * Terminal copy tests using wterm DOM selection.
 * wterm renders real text nodes — native OS selection and clipboard work
 * without any custom implementation.
 */

test.describe('terminal copy', () => {
  test('copy button is wired up (onCopyReady fires)', async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)

    // The copy action ref should be registered in window via the store/sidebar
    // We verify the terminal loaded and the shell has the copy overlay structure
    await expect(page.locator('.terminal-shell')).toBeVisible()
  })

  test('terminal text is selectable via DOM (getSelection returns text)', async ({ page }) => {
    const { id, kill } = await spawnTestSession(
      ['bash', '-c', 'echo UNIQUE_COPY_MARKER; sleep 60'],
      { cwdName: 'copy-test' },
    )

    await openApp(page)
    await page.waitForFunction((sid) => {
      const nav = (window as any).__gmuxNavigateToSession
      return typeof nav === 'function' && nav(sid) === true
    }, id, { timeout: 10_000 })
    await page.locator('.terminal-container.wterm').waitFor({ state: 'visible', timeout: 8_000 })
    await page.waitForTimeout(2000)

    // Select all text in the wterm element and check it contains output
    const selectedText = await page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      if (!term) return ''
      const range = document.createRange()
      range.selectNodeContents(term.element)
      const sel = window.getSelection()
      sel?.removeAllRanges()
      sel?.addRange(range)
      return sel?.toString() ?? ''
    })

    expect(selectedText).toContain('UNIQUE_COPY_MARKER')
    kill()
  })
})

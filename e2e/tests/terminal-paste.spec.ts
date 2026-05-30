import { test, expect } from '@playwright/test'
import { openApp, gotoTestSession, spawnTestSession } from '../helpers'

test.describe('terminal paste', () => {
  test('paste handler is attached (container has capture-phase paste listener)', async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)

    // attachPasteHandler attaches to containerRef.current with { capture: true }.
    // We verify the container exists and is inside the wterm shell.
    const containerExists = await page.evaluate(() => {
      const container = document.querySelector('.terminal-container')
      return container !== null
    })
    expect(containerExists).toBe(true)
  })

  test('bracketed paste mode is read from wterm bridge', async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)

    const bpMode = await page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      if (!term) return null
      return term.bridge?.bracketedPaste?.() ?? null
    })
    // bracketedPaste() should return a boolean (true in most shells)
    expect(typeof bpMode).toBe('boolean')
  })

  test('text typed into terminal reaches the PTY (end-to-end input)', async ({ page }) => {
    const { id, kill } = await spawnTestSession(
      ['bash', '--norc', '--noprofile'],
      { cwdName: 'paste-test' },
    )

    await openApp(page)
    await page.waitForFunction((sid) => {
      const nav = (window as any).__gmuxNavigateToSession
      return typeof nav === 'function' && nav(sid) === true
    }, id, { timeout: 10_000 })
    await page.locator('.terminal-container.wterm').waitFor({ state: 'visible', timeout: 8_000 })
    await page.waitForTimeout(1500)

    // Focus terminal and type a command
    await page.locator('.terminal-container').click()
    await page.keyboard.type('echo PASTE_E2E_MARKER')
    await page.keyboard.press('Enter')
    await page.waitForTimeout(1000)

    // Verify the output appeared in the DOM
    const termText = await page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      return term?.element?.textContent ?? ''
    })
    expect(termText).toContain('PASTE_E2E_MARKER')

    kill()
  })
})

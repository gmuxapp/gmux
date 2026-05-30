import { test, expect } from '@playwright/test'
import { openApp, gotoTestSession, spawnTestSession } from '../helpers'

/**
 * Scroll position helpers using wterm's DOM-based scrolling.
 * wterm renders text into real DOM nodes — scroll state lives on term.element.
 */

async function getScrollState(page: Parameters<typeof page.evaluate>[1] extends infer T ? never : Page) {
  return (page as any).evaluate(() => {
    const term = (window as any).__gmuxTerm
    if (!term) return null
    const el = term.element as HTMLElement
    return {
      scrollTop: el.scrollTop,
      scrollHeight: el.scrollHeight,
      clientHeight: el.clientHeight,
      atBottom: el.scrollHeight - el.scrollTop - el.clientHeight < 5,
      scrollbackCount: term.bridge?.getScrollbackCount?.() ?? 0,
    }
  })
}

import type { Page } from '@playwright/test'

async function termScrollState(page: Page) {
  return page.evaluate(() => {
    const term = (window as any).__gmuxTerm
    if (!term) return null
    const el = term.element as HTMLElement
    return {
      scrollTop: el.scrollTop,
      scrollHeight: el.scrollHeight,
      clientHeight: el.clientHeight,
      atBottom: el.scrollHeight - el.scrollTop - el.clientHeight < 5,
      scrollbackCount: term.bridge?.getScrollbackCount?.() ?? 0,
    }
  })
}

async function scrollTermToBottom(page: Page) {
  await page.evaluate(() => {
    const term = (window as any).__gmuxTerm
    if (!term) return
    const el = term.element as HTMLElement
    el.scrollTop = el.scrollHeight
  })
}

async function scrollTermUp(page: Page, pixels: number) {
  await page.evaluate((px) => {
    const term = (window as any).__gmuxTerm
    if (!term) return
    const el = term.element as HTMLElement
    el.scrollTop = Math.max(0, el.scrollTop - px)
  }, pixels)
}

test.describe('terminal scroll', () => {
  test('terminal starts at bottom (no scrollback)', async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)

    const state = await termScrollState(page)
    expect(state).not.toBeNull()
    expect(state!.atBottom).toBe(true)
  })

  test('scroll-to-bottom button appears when scrolled up and disappears when back at bottom', async ({ page }) => {
    const { id, kill } = await spawnTestSession(
      ['bash', '-c', 'for i in $(seq 1 200); do echo "line $i"; done; sleep 60'],
      { cwdName: 'scroll-test' },
    )
    await openApp(page)

    await page.waitForFunction((sid) => {
      const nav = (window as any).__gmuxNavigateToSession
      return typeof nav === 'function' && nav(sid) === true
    }, id, { timeout: 10_000 })

    await page.locator('.terminal-container.wterm').waitFor({ state: 'visible', timeout: 8_000 })
    await page.waitForTimeout(2000) // let output arrive

    // Scroll up to reveal scroll-to-bottom button
    await scrollTermUp(page, 500)
    await page.waitForTimeout(300)

    // Button should appear
    await expect(page.locator('.terminal-scroll-end')).toBeVisible({ timeout: 3_000 })

    // Click it — should return to bottom and button hides
    await page.locator('.terminal-scroll-end').click()
    await page.waitForTimeout(500)
    await expect(page.locator('.terminal-scroll-end')).not.toBeVisible()

    kill()
  })

  test('wterm element is scrollable (has real DOM scroll)', async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)

    // wterm DOM element should have a scrollHeight (real DOM text nodes)
    const hasRealDOM = await page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      if (!term) return false
      const el = term.element as HTMLElement
      // Real DOM rendering: scrollHeight >= clientHeight (never 0)
      return el.scrollHeight > 0 && el.clientHeight > 0
    })
    expect(hasRealDOM).toBe(true)
  })
})

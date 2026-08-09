import { test, expect } from '@playwright/test'
import { openApp, gotoTestSession } from '../helpers'

/**
 * Regression guard: header popovers must close when you tap back into
 * the terminal on a touch device.
 *
 * They used to close on a document `mousedown`, which works for a mouse
 * but not for a tap on the terminal: `terminal.tsx` registers
 * capture-phase, non-passive touch handlers that `preventDefault()` on
 * several gesture paths, and that suppresses the browser's synthesized
 * mouse cascade. The `mousedown` simply never arrived, so the popover
 * stayed open on top of the session you were tapping into — while a tap
 * anywhere else (header, sidebar) dismissed it normally, which is what
 * made the bug look like a focus or keyboard problem.
 *
 * `pointerdown` is dispatched before `touchstart` and can't be cancelled
 * by it, so it survives whatever the terminal does with the gesture.
 * This can't be caught by a unit test: it depends on real browser
 * event-cascade suppression, not on our own listener logic.
 */
test.use({ hasTouch: true })

test.describe('touch dismissal of header popovers', () => {
  test('the session menu closes when you tap into the terminal', async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)

    const menu = page.locator('.session-menu-dropdown')
    await page.locator('.session-menu-trigger').click()
    await expect(menu).toBeVisible()

    // Tap inside the terminal — the gesture whose synthesized mouse
    // events the terminal suppresses.
    const screen = await page.locator('.xterm-screen').boundingBox()
    if (!screen) throw new Error('terminal screen has no box')
    await page.touchscreen.tap(screen.x + screen.width / 2, screen.y + screen.height * 0.7)

    await expect(menu).toBeHidden()
  })

  test('a tap outside the terminal still dismisses it', async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)

    const menu = page.locator('.session-menu-dropdown')
    await page.locator('.session-menu-trigger').click()
    await expect(menu).toBeVisible()

    // The header is an ordinary element with no touch handling, so this
    // path worked before the fix and must keep working after it.
    await page.touchscreen.tap(300, 20)
    await expect(menu).toBeHidden()
  })
})

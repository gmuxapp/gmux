import { test, expect } from '@playwright/test'
import { openApp } from '../helpers'

/**
 * The sidebar's Projects/Activity switch is an always-visible pair of
 * buttons; the arrange popover no longer carries a View section. These
 * are markup/interaction facts a unit test cannot see.
 */
test.describe('sidebar view toggle', () => {
  test('switches views, reports state, and owns the concern alone', async ({ page }) => {
    await openApp(page, '/my-project/claude/~fam2kid?mock')
    await page.waitForSelector('.sidebar-view-toggle')

    const projects = page.locator('.sidebar-view-btn', { hasText: 'Projects' })
    const activity = page.locator('.sidebar-view-btn', { hasText: 'Activity' })

    // Projects is the default: pressed, wearing the selected fill.
    await expect(projects).toHaveAttribute('aria-pressed', 'true')
    await expect(activity).toHaveAttribute('aria-pressed', 'false')
    await expect(page.locator('.session-family').first()).toBeVisible()

    // Switching swaps the list, the pressed state, and the URL mirror.
    await activity.click()
    await expect(activity).toHaveAttribute('aria-pressed', 'true')
    await expect(projects).toHaveAttribute('aria-pressed', 'false')
    await expect(page.locator('.session-row').first()).toBeVisible()
    await expect(page).toHaveURL(/sidebar=activity/)

    await projects.click()
    await expect(projects).toHaveAttribute('aria-pressed', 'true')
    await expect(page.locator('.session-family').first()).toBeVisible()

    // One control per concern: the arrange popover keeps Host and Show
    // but no longer offers the view radios the toggle replaced.
    await page.locator('.sidebar-settings-btn[aria-label="List options"]').click()
    const menu = page.locator('.view-menu')
    await menu.waitFor()
    await expect(menu.locator('.view-menu-label').first()).toHaveText('Host')
    await expect(menu.locator('.view-menu-option', { hasText: 'Projects' })).toHaveCount(0)
    await expect(menu.locator('.view-menu-option', { hasText: 'Activity' })).toHaveCount(0)
  })
})

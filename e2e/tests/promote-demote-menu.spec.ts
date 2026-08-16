import { test, expect, type Page } from '@playwright/test'
import * as fs from 'fs'
import * as path from 'path'
import { openApp } from '../helpers'

/**
 * The ⋮ session menu's promote/demote items, driven against the bundled
 * `?mock` fixtures (deterministic, no daemon state). Eligibility logic is
 * unit-tested (`family.test.ts` promotionAction matrix); these cover what a
 * unit test cannot: the item's presence and copy in the real menu, the POST
 * wiring, focus return, and that the affordance survives the phone layouts.
 *
 * The real state round-trip (snapshot reprojection, URL rewrite, sidebar
 * row appearing) runs against the disposable daemon in
 * `promote-demote-daemon.spec.ts`.
 */

const SCREENSHOT_DIR = path.resolve(__dirname, '../../.memory/screenshots')

async function openMock(page: Page, urlPath: string) {
  await openApp(page, `${urlPath}?mock`)
  await page.waitForSelector('.main-header')
}

const promotionItem = (page: Page) => page.locator('.session-menu-promotion')

async function openMenu(page: Page) {
  await page.locator('.session-menu-trigger').click()
  await page.locator('.session-menu-dropdown').waitFor()
}

test.describe('promote/demote in the ⋮ session menu (mock fixtures)', () => {
  test('a family child offers Promote to root, and the copy keeps ownership', async ({ page }) => {
    // fam2kid's organizational parent is fam1kid ("implement drawer").
    await openMock(page, '/my-project/claude/~fam2kid')
    await openMenu(page)
    const item = promotionItem(page)
    await expect(item).toHaveCount(1)
    await expect(item).toContainText('Promote to root')
    // Promotion must not read as severing ownership: the note names the
    // parent that keeps the child.
    await expect(item.locator('.session-menu-action-note'))
      .toHaveText('Shows as its own top-level session — implement drawer still owns it')

    const posts: string[] = []
    await page.route('**/v1/sessions/**', async (route) => {
      if (route.request().method() === 'POST') {
        posts.push(new URL(route.request().url()).pathname)
        return route.fulfill({ status: 200, body: '{"ok":true,"data":{}}' })
      }
      return route.continue()
    })
    await item.click()
    await expect.poll(() => posts).toContain('/v1/sessions/fam2kid/promote')
    // Exactly one mutation per activation.
    expect(posts.filter(p => p.endsWith('/promote'))).toHaveLength(1)
    // Menu closed, focus back on the trigger (keyboard users don't land on
    // <body> when the activated item unmounts with the dropdown).
    await expect(page.locator('.session-menu-dropdown')).toHaveCount(0)
    await expect(page.locator('.session-menu-trigger')).toBeFocused()
  })

  test('a promoted root offers Return to family and names the current parent', async ({ page }) => {
    // famApromoted carries promoted_to_root with famAroot as its
    // organizational parent — it renders as a root but can rejoin.
    await openMock(page, '/my-project/claude/~famApromoted')
    await openMenu(page)
    const item = promotionItem(page)
    await expect(item).toContainText('Return to family')
    await expect(item.locator('.session-menu-action-note'))
      .toHaveText('Groups back under a genuinely very long root agent title for truncation checks')

    const posts: string[] = []
    await page.route('**/v1/sessions/**', async (route) => {
      if (route.request().method() === 'POST') {
        posts.push(new URL(route.request().url()).pathname)
        return route.fulfill({ status: 200, body: '{"ok":true,"data":{}}' })
      }
      return route.continue()
    })
    await item.click()
    await expect.poll(() => posts).toContain('/v1/sessions/famApromoted/demote')
  })

  test('an in-flight promotion cannot be double-submitted from a reopened menu', async ({ page }) => {
    // The menu closes on activation, but the authoritative snapshot takes a
    // beat (and in mock never comes) — reopening used to offer the same verb
    // again and fire a second POST per click. The item must instead show the
    // busy label, disabled, until the projection flips or the request fails.
    await openMock(page, '/my-project/claude/~fam2kid')
    const posts: string[] = []
    await page.route('**/v1/sessions/**', async (route) => {
      if (route.request().method() === 'POST') {
        posts.push(new URL(route.request().url()).pathname)
        return route.fulfill({ status: 200, body: '{"ok":true,"data":{}}' })
      }
      return route.continue()
    })
    await openMenu(page)
    await promotionItem(page).click()
    await expect.poll(() => posts.filter(p => p.endsWith('/promote'))).toHaveLength(1)

    await openMenu(page)
    const item = promotionItem(page)
    await expect(item).toBeDisabled()
    await expect(item).toContainText('Promoting…')
    fs.mkdirSync(SCREENSHOT_DIR, { recursive: true })
    await page.screenshot({ path: path.join(SCREENSHOT_DIR, 'menu-promote-pending.png') })
    await item.click({ force: true }) // a forced click must still be inert
    await page.waitForTimeout(200)
    expect(posts.filter(p => p.endsWith('/promote'))).toHaveLength(1)
  })

  test('a failed promotion re-arms the item beside its toast', async ({ page }) => {
    await openMock(page, '/my-project/claude/~fam2kid')
    await page.route('**/v1/sessions/**', async (route) => {
      if (route.request().method() === 'POST') {
        return route.fulfill({
          status: 400,
          body: '{"ok":false,"error":{"code":"local_only","message":"promote is only available for sessions owned by this daemon"}}',
        })
      }
      return route.continue()
    })
    await openMenu(page)
    await promotionItem(page).click()
    await expect(page.locator('.toast-message').first()).toContainText('Promote failed')
    await openMenu(page)
    const item = promotionItem(page)
    await expect(item).toBeEnabled()
    await expect(item).toContainText('Promote to root')
  })

  test('Escape hands focus back to the ⋮ trigger, even from inside the dropdown', async ({ page }) => {
    await openMock(page, '/my-project/claude/~fam2kid')
    await openMenu(page)
    // The Escape listener attaches in a post-paint effect; give it a beat so
    // the keypress can't race the registration.
    await page.waitForTimeout(150)
    await page.keyboard.press('Tab') // focus moves onto a menu item
    await page.keyboard.press('Escape')
    await expect(page.locator('.session-menu-dropdown')).toHaveCount(0)
    // Without the explicit hand-back, focus fell to <body> here.
    await expect(page.locator('.session-menu-trigger')).toBeFocused()
  })

  test('the ⋮ trigger carries an accessible name, not a glyph', async ({ page }) => {
    await openMock(page, '/my-project/claude/~fam2kid')
    await expect(page.locator('.session-menu-trigger')).toHaveAttribute('aria-label', 'Session actions')
  })

  test('a plain root offers neither, and the rest of the menu is intact', async ({ page }) => {
    await openMock(page, '/my-project/claude/~fam0root')
    await openMenu(page)
    await expect(promotionItem(page)).toHaveCount(0)
    // The menu still renders its usual content around the absent item.
    await expect(page.locator('.session-menu-dropdown')).toContainText('Session info')
  })

  test('a promoted session renders as its own sidebar row, not a family member', async ({ page }) => {
    await openMock(page, '/my-project/claude/~famApromoted')
    // Own root row, selected.
    const row = page.locator('.session-item.selected').filter({ hasText: 'promoted research spike' })
    await expect(row).toHaveCount(1)
    // Not shown as a member row of the famAroot family entry.
    await expect(page.locator('.family-slot').filter({ hasText: 'promoted research spike' })).toHaveCount(0)
    // And no family panel trigger: it is not currently part of a family.
    await expect(page.locator('.family-trigger')).toHaveCount(0)
  })

  test('the family panel keeps its #485 tally/filter surface unchanged', async ({ page }) => {
    // Promotion adds no controls to the panel; the counts line is still the
    // one derived from the standard family numbers, and the promoted member
    // is out of the tree (it starts its own family).
    await openMock(page, '/my-project/claude/~famAkid')
    await page.locator('[aria-controls="agent-family-drawer"]').click()
    await page.locator('.family-counts').waitFor()
    const titles = await page.locator('.family-row .family-row-title').allTextContents()
    expect(titles.some(t => t.includes('promoted research spike'))).toBe(false)
    // No promotion verbs leaked into the drawer.
    await expect(page.locator('.family-drawer')).not.toContainText('Promote')
  })
})

for (const [name, viewport, mobile] of [
  ['phone portrait', { width: 390, height: 844 }, true],
  ['phone landscape', { width: 844, height: 390 }, true],
] as const) {
  test.describe(`promote/demote menu on ${name}`, () => {
    test.use({ viewport, hasTouch: mobile, isMobile: mobile })

    test('the menu path works and stays touch-safe', async ({ page }) => {
      await openMock(page, '/my-project/claude/~fam2kid')
      await openMenu(page)
      const item = promotionItem(page)
      await expect(item).toBeVisible()
      await expect(item).toContainText('Promote to root')
      // The note must not push the item off-screen on a narrow viewport.
      const box = (await item.boundingBox())!
      expect(box.x).toBeGreaterThanOrEqual(0)
      expect(box.x + box.width).toBeLessThanOrEqual(viewport.width)

      fs.mkdirSync(SCREENSHOT_DIR, { recursive: true })
      await page.screenshot({
        path: path.join(SCREENSHOT_DIR, `menu-promote-${name.replace(/\s+/g, '-')}.png`),
      })

      const posts: string[] = []
      await page.route('**/v1/sessions/**', async (route) => {
        if (route.request().method() === 'POST') {
          posts.push(new URL(route.request().url()).pathname)
          return route.fulfill({ status: 200, body: '{"ok":true,"data":{}}' })
        }
        return route.continue()
      })
      // A tap, not a hover-dependent affordance.
      await item.tap()
      await expect.poll(() => posts).toContain('/v1/sessions/fam2kid/promote')
      await expect(page.locator('.session-menu-dropdown')).toHaveCount(0)
    })
  })
}

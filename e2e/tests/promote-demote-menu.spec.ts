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

async function returnToFamilyMember(page: Page) {
  const family = page.locator('.session-family').filter({ hasText: 'orchestrator' })
  // Root selection deliberately restores no member history. Its summary is
  // the route back into the family map; choose the member there.
  await family.locator('.session-item').first().click()
  await family.locator('.family-activity').click()
  await page.locator('#agent-family-drawer .family-row')
    .filter({ hasText: 'wire up the protocol' }).click()
}

test.describe('promote/demote in the ⋮ session menu (mock fixtures)', () => {
  test('a family child offers Promote to root, and the copy keeps ownership', async ({ page }) => {
    // fam2kid's organizational parent is fam1kid ("implement drawer").
    await openMock(page, '/my-project/claude/~fam2kid')
    await openMenu(page)
    const item = promotionItem(page)
    await expect(item).toHaveCount(1)
    await expect(item).toContainText('Promote to root')
    // An offerable verb explains itself: subtext is reserved for
    // blocked actions, which owe their reason.
    await expect(item.locator('.session-menu-action-note')).toHaveCount(0)

    const posts: string[] = []
    await page.route('**/v1/sessions/**', async (route) => {
      if (route.request().method() === 'POST') {
        posts.push(new URL(route.request().url()).pathname)
        return route.fulfill({ status: 200, body: '{"ok":true,"data":{}}' })
      }
      return route.continue()
    })
    await item.click()
    await expect.poll(() => posts).toContain('/v1/sessions/fam2kid/reparent')
    // Exactly one mutation per activation.
    expect(posts.filter(p => p.endsWith('/reparent'))).toHaveLength(1)
    // Menu closed, focus back on the trigger (keyboard users don't land on
    // <body> when the activated item unmounts with the dropdown).
    await expect(page.locator('.session-menu-dropdown')).toHaveCount(0)
    await expect(page.locator('.session-menu-trigger')).toBeFocused()
  })

  test('a promoted root offers Return to family and names the current parent', async ({ page }) => {
    // famApromoted is parentless with famAroot as its launch parent, so it
    // renders as a root but can rejoin.
    await openMock(page, '/my-project/claude/~famApromoted')
    await openMenu(page)
    const item = promotionItem(page)
    await expect(item).toContainText('Return to family')
    await expect(item.locator('.session-menu-action-note')).toHaveCount(0)

    const posts: string[] = []
    await page.route('**/v1/sessions/**', async (route) => {
      if (route.request().method() === 'POST') {
        posts.push(new URL(route.request().url()).pathname)
        return route.fulfill({ status: 200, body: '{"ok":true,"data":{}}' })
      }
      return route.continue()
    })
    await item.click()
    await expect.poll(() => posts).toContain('/v1/sessions/famApromoted/reparent')
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
    await expect.poll(() => posts.filter(p => p.endsWith('/reparent'))).toHaveLength(1)

    await openMenu(page)
    const item = promotionItem(page)
    await expect(item).toBeDisabled()
    await expect(item).toContainText('Promoting…')
    fs.mkdirSync(SCREENSHOT_DIR, { recursive: true })
    await page.screenshot({ path: path.join(SCREENSHOT_DIR, 'menu-promote-pending.png') })
    await item.click({ force: true }) // a forced click must still be inert
    await page.waitForTimeout(200)
    expect(posts.filter(p => p.endsWith('/reparent'))).toHaveLength(1)
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
    // A rejected request announces only through the error toast, never as a
    // false success status.
    await expect(page.locator('[data-promotion-status]')).toHaveText('')
    await openMenu(page)
    const item = promotionItem(page)
    await expect(item).toBeEnabled()
    await expect(item).toContainText('Promote to root')
  })

  test('Escape hands focus back to the ⋮ trigger, even from inside the dropdown', async ({ page }) => {
    await openMock(page, '/my-project/claude/~fam2kid')
    // Deliberately no settle wait: the listener attaches in a layout effect,
    // synchronously with the commit that mounts the dropdown, so the very
    // first Escape after opening must work. (A passive effect is deferred to
    // a later task, and this exact immediate sequence reproduced a stranded-
    // open menu.)
    await page.locator('.session-menu-trigger').click()
    await page.keyboard.press('Tab') // focus moves onto a menu item
    await page.keyboard.press('Escape')
    await expect(page.locator('.session-menu-dropdown')).toHaveCount(0)
    // Without the explicit hand-back, focus fell to <body> here.
    await expect(page.locator('.session-menu-trigger')).toBeFocused()
  })

  test('keyboard-only: Enter opens, immediate Escape closes, focus restored', async ({ page }) => {
    await openMock(page, '/my-project/claude/~fam2kid')
    await page.locator('.session-menu-trigger').focus()
    await page.keyboard.press('Enter')
    await expect(page.locator('.session-menu-dropdown')).toHaveCount(1)
    await page.keyboard.press('Escape')
    await expect(page.locator('.session-menu-dropdown')).toHaveCount(0)
    await expect(page.locator('.session-menu-trigger')).toBeFocused()
  })

  test('the ⋮ trigger carries an accessible name and controls its dropdown', async ({ page }) => {
    await openMock(page, '/my-project/claude/~fam2kid')
    const trigger = page.locator('.session-menu-trigger')
    await expect(trigger).toHaveAttribute('aria-label', 'Session actions')
    await expect(trigger).toHaveAttribute('aria-controls', 'session-menu-dropdown')
    await expect(trigger).toHaveAttribute('aria-expanded', 'false')
    await trigger.click()
    await expect(trigger).toHaveAttribute('aria-expanded', 'true')
    await expect(page.locator('#session-menu-dropdown')).toHaveCount(1)
  })

  test('activation announces the pending state through a status region', async ({ page }) => {
    // The dropdown — and the only visible "Promoting…" — unmounts on
    // activation, so a persistent sr-only role=status carries the state for
    // assistive tech. Failures deliberately say nothing here: the error
    // toast's own live region announces those once.
    await openMock(page, '/my-project/claude/~fam2kid')
    await page.route('**/v1/sessions/**', async (route) => {
      if (route.request().method() === 'POST') {
        return route.fulfill({ status: 200, body: '{"ok":true,"data":{}}' })
      }
      return route.continue()
    })
    const status = page.locator('[data-promotion-status]')
    await expect(status).toHaveAttribute('aria-live', 'polite')
    await expect(status).toHaveAttribute('aria-atomic', 'true')
    await expect(status).toHaveText('')
    await openMenu(page)
    await promotionItem(page).click()
    await expect(status).toHaveText('Promoting to root…')
    // The toast live region exists separately and stays silent on success.
    await expect(page.locator('.toast-message')).toHaveCount(0)
  })

  test('a stale failure from session A cannot re-arm session B\u2019s pending item', async ({ page }) => {
    // Held-request A→B schedule: promote on fam2kid is held open (A), the
    // user navigates to famApromoted and submits demote (B, also held), then
    // A fails. A's completion may settle only A's entry; B stays disabled,
    // and a forced click emits no third POST.
    await openMock(page, '/my-project/claude/~fam2kid')
    const posts: string[] = []
    let failA: (() => void) | undefined
    await page.route('**/v1/sessions/fam2kid/reparent', async (route) => {
      posts.push('A')
      await new Promise<void>(resolve => { failA = resolve })
      await route.fulfill({ status: 400, body: '{"ok":false,"error":{"code":"internal","message":"held A failed"}}' })
    })
    await page.route('**/v1/sessions/famApromoted/reparent', async (route) => {
      posts.push('B')
      await new Promise(() => { /* held forever */ })
      await route.fulfill({ status: 200, body: '{}' })
    })

    await openMenu(page)
    await promotionItem(page).click() // A in flight
    // In-app navigation to the promoted session's own row.
    await page.locator('.session-item').filter({ hasText: 'promoted research spike' }).click()
    await expect(page).toHaveURL(/~famApromoted/)
    await openMenu(page)
    await expect(promotionItem(page)).toContainText('Return to family')
    await promotionItem(page).click() // B in flight
    await expect.poll(() => posts).toEqual(['A', 'B'])

    failA!() // A completes with 400 — must settle only A's entry
    await expect(page.locator('.toast-message')).toContainText('held A failed')
    await openMenu(page)
    const item = promotionItem(page)
    await expect(item).toBeDisabled()
    await expect(item).toContainText('Returning…')
    await item.click({ force: true })
    await page.waitForTimeout(200)
    expect(posts).toEqual(['A', 'B'])

    // And A's own item re-armed exactly where its failure toast points.
    await returnToFamilyMember(page)
    await expect(page).toHaveURL(/~fam2kid/)
    await openMenu(page)
    await expect(promotionItem(page)).toBeEnabled()
    await expect(promotionItem(page)).toContainText('Promote to root')
  })

  test('the pending guard survives navigating away and back', async ({ page }) => {
    // The guard is keyed by session in module scope, not per menu mount: a
    // request stays in flight regardless of where the user navigates.
    await openMock(page, '/my-project/claude/~fam2kid')
    const posts: string[] = []
    await page.route('**/v1/sessions/fam2kid/reparent', async (route) => {
      posts.push(new URL(route.request().url()).pathname)
      await new Promise(() => { /* held forever */ })
    })
    await openMenu(page)
    await promotionItem(page).click()
    await expect.poll(() => posts).toHaveLength(1)

    await page.locator('.session-item').filter({ hasText: 'promoted research spike' }).click()
    await returnToFamilyMember(page)
    await expect(page).toHaveURL(/~fam2kid/)
    await openMenu(page)
    const item = promotionItem(page)
    await expect(item).toBeDisabled()
    await expect(item).toContainText('Promoting…')
    await item.click({ force: true })
    await page.waitForTimeout(200)
    expect(posts).toHaveLength(1)
  })

  test('authoritative target then reversal clears an unmounted pending entry', async ({ page }) => {
    await openMock(page, '/my-project/claude/~fam2kid')
    await page.waitForFunction(() => Boolean((window as any).__store))
    const posts: string[] = []
    await page.route('**/v1/sessions/fam2kid/reparent', async route => {
      posts.push('promote')
      await new Promise(() => { /* held: snapshots, not HTTP completion, settle A */ })
    })
    await openMenu(page)
    await promotionItem(page).click()
    await expect.poll(() => posts).toHaveLength(1)

    // Unmount A, deliver its authoritative target, then an external reversal.
    await page.locator('.session-item').filter({ hasText: 'promoted research spike' }).click()
    await page.evaluate(() => {
      const store = (window as any).__store
      const current = store.sessions.value as any[]
      // Single-axis promotion: the authoritative target is the severed parent
      // edge, and the external reversal restores the original parent.
      store.applySessionsSnapshot(current.map(s => s.id === 'fam2kid' ? (({ parent_session_id: _, ...rest }) => rest)(s) : s))
      store.applySessionsSnapshot(current.map(s => s.id === 'fam2kid' ? { ...s, parent_session_id: 'fam1kid' } : s))
    })
    await returnToFamilyMember(page)
    await expect(page).toHaveURL(/~fam2kid/)
    await openMenu(page)
    await expect(promotionItem(page)).toBeEnabled()
    await expect(promotionItem(page)).toContainText('Promote to root')
    await expect(promotionItem(page)).not.toContainText('Promoting…')
    await expect(page.locator('[data-promotion-status]')).toHaveText('Promoted to root.')
  })

  test('a child outside every project offers Promote blocked, with the reason', async ({ page }) => {
    // famBoutside works in /tmp/scratch — no stamp, no matching rule. The
    // daemon has nowhere to place its promoted row (parentage never overrides
    // project matching), so promoting would strand it with no sidebar row and
    // a dead URL. The item is visible and disabled with the reason — never
    // silently hidden, never a POST.
    await openMock(page, '/my-project/claude/~famBoutside')
    const posts: string[] = []
    await page.route('**/v1/sessions/**', async (route) => {
      if (route.request().method() === 'POST') {
        posts.push(new URL(route.request().url()).pathname)
        return route.fulfill({ status: 200, body: '{}' })
      }
      return route.continue()
    })
    await openMenu(page)
    const item = promotionItem(page)
    await expect(item).not.toHaveAttribute('disabled')
    await expect(item).toHaveAttribute('aria-disabled', 'true')
    await expect(item).toHaveAttribute('aria-describedby', 'session-promotion-note')
    await expect(item).toContainText('Promote to root')
    await expect(item.locator('.session-menu-action-note'))
      .toHaveText('Needs a project: no project contains this session’s folder, so it would have no row of its own. Add one in Settings → Projects.')
    await item.focus()
    await expect(item).toBeFocused()
    await item.press('Enter')
    await page.waitForTimeout(200)
    expect(posts).toHaveLength(0)
    // The session itself stays fully reachable through its family.
    await expect(page).toHaveURL(/~famBoutside/)
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
    await page.locator('.family-trigger').click()
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
      await expect.poll(() => posts).toContain('/v1/sessions/fam2kid/reparent')
      await expect(page.locator('.session-menu-dropdown')).toHaveCount(0)
    })
  })
}

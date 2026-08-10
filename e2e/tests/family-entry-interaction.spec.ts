import { test, expect } from '@playwright/test'
import { openApp } from '../helpers'

/**
 * The sidebar's family entry is one clickable group with its own rows
 * inside it, and none of that lives in a testable unit: it is markup,
 * event targets and browser gesture semantics. These are the behaviours
 * that broke in review, each of which a unit test structurally cannot
 * see.
 *
 * Runs against `?mock`, so the fixtures are the bundled demo family
 * rather than daemon state: deterministic, and the same data the design
 * work was done against.
 */

/** `?mock` boots the frontend on bundled fixtures; auth still applies. */
async function openMockSidebar(page: import('@playwright/test').Page, path: string) {
  await openApp(page, `${path}?mock`)
  await page.waitForSelector('.sidebar-list')
  await page.locator('.session-family').first().waitFor()
}

const familyEntry = (page: import('@playwright/test').Page, title: string) =>
  page.locator('.session-family').filter({ hasText: title })

test.describe('sidebar family entry', () => {
  test('the slack around the rows selects the root, but declines link gestures', async ({ page }) => {
    await openMockSidebar(page, '/my-project/claude/~fam2kid')
    const entry = familyEntry(page, 'build watcher agent')
    const slack = entry.locator('.family-activity')

    // 1. An ordinary click on the counts line lands on the root: the
    //    whole group is the root's hit area.
    await slack.click()
    await expect(page).toHaveURL(/~famBroot/)

    // 2. A modified click is a link gesture (new tab, download, range
    //    select). The slack is not a link, so it declines rather than
    //    quietly doing something else.
    await openMockSidebar(page, '/my-project/claude/~fam2kid')
    await familyEntry(page, 'build watcher agent').locator('.family-activity').click({ modifiers: ['Control'] })
    await expect(page).toHaveURL(/~fam2kid/)

    // 3. A click that ends a text selection wants the text, not a
    //    navigation.
    const line = familyEntry(page, 'build watcher agent').locator('.family-activity')
    const box = (await line.boundingBox())!
    await page.mouse.move(box.x + 4, box.y + box.height / 2)
    await page.mouse.down()
    await page.mouse.move(box.x + box.width - 8, box.y + box.height / 2, { steps: 10 })
    await page.mouse.up()
    await expect(page).toHaveURL(/~fam2kid/)

    // 4. The rows themselves still own their own clicks.
    await familyEntry(page, 'orchestrator').locator('.family-slot').click()
    await expect(page).toHaveURL(/~fam2kid/)
  })

  test('a drop anywhere on the entry reorders exactly once', async ({ page }) => {
    await openMockSidebar(page, '/my-project/claude/~fam2kid')

    // The group is a drop target as well as the root row, so a drop on
    // the root row can reach both handlers in one dispatch. Count the
    // reorder requests rather than trusting the resulting order: two
    // identical PATCHes produce the right order and the wrong number of
    // writes (and two error toasts when the daemon says no).
    const reorders: string[] = []
    await page.route('**/sessions', async (route) => {
      const req = route.request()
      if (req.method() === 'PATCH') reorders.push(req.postData() ?? '')
      await route.fulfill({ status: 200, body: '{}' })
    })

    // Dispatch the browser's own event sequence, bubbling as it really
    // does, and count the writes. Two things can go wrong and neither
    // shows up in the resulting order: a sub-row can refuse the drop
    // outright (before the group took drag handlers, only the root row
    // preventDefault()ed the dragover, so the lower two thirds of a
    // three-row entry silently rejected the drag), and a drop on the
    // root row can run both the row's handler and the group's in one
    // dispatch — sending the same reorder twice, and toasting twice
    // when the daemon rejects it.
    // One event per step, with a beat between them: the handlers keep
    // drag state in component state, so a whole sequence dispatched in
    // a single tick never sees its own dragstart.
    const fire = (type: string, selector: string, within: string) => page.evaluate(({ type, selector, within }) => {
      const entry = [...document.querySelectorAll('.session-family')]
        .find(e => e.textContent?.includes(within))!
      const el = entry.querySelector(selector)!
      const ev = new DragEvent(type, { bubbles: true, cancelable: true, dataTransfer: new DataTransfer() })
      el.dispatchEvent(ev)
      return ev.defaultPrevented
    }, { type, selector, within })

    for (const target of ['.session-item', '.family-activity']) {
      reorders.length = 0
      await fire('dragstart', '.session-item', 'orchestrator')
      await page.waitForTimeout(100)
      expect(await fire('dragover', target, 'build watcher agent'), `dragover accepted over ${target}`).toBe(true)
      await page.waitForTimeout(100)
      await fire('drop', target, 'build watcher agent')
      await page.waitForTimeout(250)
      expect(reorders.length, `reorder writes for a drop on ${target}`).toBe(1)
      await fire('dragend', '.session-item', 'orchestrator')
      await page.waitForTimeout(100)
    }
  })
})

test.describe('the family line and the panel tally', () => {
  test('the plus lines up with what it adds to', async ({ page }) => {
    await openMockSidebar(page, '/my-project/claude/~fam2kid')
    const geometry = await page.evaluate(() => {
      const read = (entry: Element) => {
        const slot = entry.querySelector('.family-slot')
        const plus = entry.querySelector('.family-activity .family-plus')
        if (!plus) return null
        return {
          plusText: plus.textContent,
          plusX: Math.round(plus.getBoundingClientRect().x),
          memberGlyphX: slot
            ? Math.round(slot.querySelector('.family-glyph')!.getBoundingClientRect().x)
            : null,
          memberTitleX: slot
            ? Math.round(slot.querySelector('.family-slot-title')!.getBoundingClientRect().x)
            : null,
        }
      }
      return [...document.querySelectorAll('.session-family')].map(read).filter(Boolean)
    })

    const withMember = geometry.filter(g => g!.memberTitleX !== null)
    const withoutMember = geometry.filter(g => g!.memberTitleX === null)
    expect(withMember.length, 'a family with a member row on screen').toBeGreaterThan(0)
    expect(withoutMember.length, 'a family without one').toBeGreaterThan(0)

    for (const g of geometry) expect(g!.plusText).toBe('+')
    // Under the member's title: these are members in addition to the one
    // named above, so the line starts where that name starts.
    for (const g of withMember) expect(g!.plusX).toBe(g!.memberTitleX)
    // With nothing above to add to, it stays in the glyph column, level
    // with where a member's status would be.
    const glyphColumn = withMember[0]!.memberGlyphX
    for (const g of withoutMember) expect(g!.plusX).toBe(glyphColumn)
  })

  test("the panel's tally names states in the turn model's words", async ({ page }) => {
    await openMockSidebar(page, '/my-project/claude/~fam2kid')
    await page.locator('[aria-controls="agent-family-drawer"]').first().click()
    const counts = page.locator('.family-counts')
    await counts.waitFor()

    // `unread` on the wire is "waiting on you" to a reader, and the CSS
    // token `working` is the active dot; the header says what the turn
    // model says (ADR 0023), not what the fields are called.
    expect(await counts.textContent()).not.toMatch(/unread|working/)
    await expect(counts.locator('.family-count').last()).toHaveText(/\d+ total/)

    // Every state segment carries the dot its rows carry, so the header
    // reads as a key to the tree rather than a second vocabulary.
    const segments = await counts.locator('.family-count').evaluateAll(nodes => nodes.map(n => ({
      text: n.textContent?.replace(/\s+/g, ' ').trim(),
      dot: n.querySelector('.session-dot-indicator')?.className ?? null,
    })))
    for (const segment of segments) {
      if (/total/.test(segment.text ?? '')) {
        expect(segment.dot, 'total is not a state, so it gets no dot').toBeNull()
        continue
      }
      expect(segment.text).toMatch(/^\d+ (error|active|waiting)$/)
      expect(segment.dot).toMatch(/session-dot-indicator (error|working|unread)/)
    }
    expect(segments.some(s => /waiting|active|error/.test(s.text ?? ''))).toBe(true)
  })
})

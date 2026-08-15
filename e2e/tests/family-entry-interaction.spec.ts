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
    // `all` — the absence of a filter — goes first, where the panel
    // opens, and carries no count: it isn't a state being tallied.
    await expect(counts.locator('.family-count').first()).toHaveText('all')

    // Every state segment carries the dot its rows carry, so the header
    // reads as a key to the tree rather than a second vocabulary.
    const segments = await counts.locator('.family-count').evaluateAll(nodes => nodes.map(n => ({
      text: n.textContent?.replace(/\s+/g, ' ').trim(),
      dot: n.querySelector('.session-dot-indicator')?.className ?? null,
      proc: n.querySelector('.family-row-proc')?.textContent ?? null,
    })))
    for (const segment of segments) {
      if (segment.text === 'all') {
        expect(segment.dot, 'all is not a state, so it gets no glyph').toBeNull()
        expect(segment.proc).toBeNull()
        continue
      }
      expect(segment.text).toMatch(/^\$?\d+ (error|active|running|waiting)$/)
      // Running commands are counted apart from thinking agents and wear
      // the same `$` their rows do, because a family is routinely mostly
      // processes and one number for both hides that.
      if (/running/.test(segment.text ?? '')) {
        expect(segment.proc).toBe('$')
        expect(segment.dot).toBeNull()
      } else {
        expect(segment.dot).toMatch(/session-dot-indicator (error|working|unread)/)
        expect(segment.proc).toBeNull()
      }
    }
    expect(segments.some(s => /running/.test(s.text ?? '')), 'a running process in the fixtures').toBe(true)
    expect(segments.some(s => /waiting|active|error/.test(s.text ?? ''))).toBe(true)
  })

  test('each tally filters the tree to its own state, and back', async ({ page }) => {
    // Standing on an `active` member, so the `error` filter excludes the
    // very row you're on — the case that has to keep working.
    await openMockSidebar(page, '/my-project/claude/~fam2kid')
    await page.locator('[aria-controls="agent-family-drawer"]').first().click()
    const counts = page.locator('.family-counts')
    await counts.waitFor()
    const rows = page.locator('.family-row')
    const titles = () => rows.evaluateAll(nodes =>
      nodes.map(n => n.querySelector('.family-row-title')?.textContent ?? ''))
    const tally = (label: string) => counts.locator('.family-count').filter({ hasText: label })

    const unfiltered = await titles()
    expect(unfiltered.length).toBeGreaterThan(4)

    await tally('error').click()
    await expect(tally('error')).toHaveAttribute('aria-pressed', 'true')
    const errored = await titles()
    expect(errored.length).toBeLessThan(unfiltered.length)
    expect(errored.some(t => t.startsWith('investigate a really long descendant'))).toBe(true)
    // Everything still on screen is either the error, an ancestor that
    // reaches it, or you.
    await expect(page.locator('.family-row.selected')).toHaveCount(1)
    await expect(page.locator('.family-row[aria-current="page"]')).toBeVisible()

    // A filter for a state nothing on your spine is in still keeps you.
    await tally('running').click()
    await expect(tally('error')).toHaveAttribute('aria-pressed', 'false')
    const running = await titles()
    expect(running.some(t => t.includes('pnpm test'))).toBe(true)
    expect(running.some(t => t.startsWith('investigate a really long descendant'))).toBe(false)
    await expect(page.locator('.family-row[aria-current="page"]')).toBeVisible()

    // Pressing the live filter clears it; so does `all`.
    await tally('running').click()
    expect(await titles()).toEqual(unfiltered)
    await tally('error').click()
    expect((await titles()).length).toBeLessThan(unfiltered.length)
    await tally('all').click()
    expect(await titles()).toEqual(unfiltered)
  })

  test('each filter offers its own bulk action, and none is ambient', async ({ page }) => {
    await openMockSidebar(page, '/my-project/claude/~fam2kid')
    await page.locator('[aria-controls="agent-family-drawer"]').first().click()
    const counts = page.locator('.family-counts')
    await counts.waitFor()
    const tally = (label: string) => counts.locator('.family-count').filter({ hasText: label })
    const action = page.locator('.family-mark-read')

    // No filter, no verb: a bulk action only exists while you are
    // looking at the complete list of what it will touch.
    await expect(action).toHaveCount(0)

    const expected: [string, string][] = [
      ['waiting', 'Mark all read'],
      ['error', 'Mark all read'],
      ['running', 'Stop all'],
      ['active', 'Interrupt all'],
    ]
    for (const [state, verb] of expected) {
      await tally(state).click()
      await expect(action).toHaveText(verb)
      await tally('all').click()
      await expect(action).toHaveCount(0)
    }

    // The waiting action still acts — and only on the waiting members.
    const reads: string[] = []
    await page.route('**/v1/sessions/**/read*', route => {
      reads.push(route.request().url())
      route.fulfill({ status: 200, body: '{}' })
    })
    await tally('waiting').click()
    await action.click()
    await expect.poll(() => reads.length).toBeGreaterThan(0)
    // Only fam1kid is `waiting` in this family: the errored member is
    // under `error`, precedence keeps it out of this verb's reach.
    expect(reads.every(url => url.includes('fam1kid'))).toBe(true)
  })
})

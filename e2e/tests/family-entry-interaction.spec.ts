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
  test('the indicator is a nested button that opens the family panel', async ({ page }) => {
    await openMockSidebar(page, '/my-project/claude/~fam2kid')
    const entry = familyEntry(page, 'build watcher agent')
    const indicator = entry.locator('.family-activity')

    await expect(indicator).toHaveJSProperty('tagName', 'BUTTON')
    await expect(indicator).toHaveAttribute('aria-controls', 'agent-family-drawer')

    // It remains inside the family's broad hit area, but hovering the
    // nested target gives it the header button's pill treatment.
    await indicator.hover()
    const sidebarPill = await indicator.evaluate(el => {
      const s = getComputedStyle(el)
      return { borderRadius: s.borderRadius, borderColor: s.borderColor, background: s.backgroundColor }
    })
    expect(sidebarPill.borderRadius).toBe('999px')
    expect(sidebarPill.borderColor).not.toBe('rgba(0, 0, 0, 0)')
    expect(sidebarPill.background).not.toBe('rgba(0, 0, 0, 0)')

    await indicator.click()
    await expect(page).toHaveURL(/~famBroot/)
    await expect(page.locator('#agent-family-drawer')).toBeVisible()

    // It is a real button rather than a clickable div: keyboard users
    // can close the panel, return to it, and open it with Enter.
    await page.locator('.family-trigger').click()
    await expect(page.locator('#agent-family-drawer')).toBeHidden()
    await indicator.focus()
    await page.keyboard.press('Enter')
    await expect(page.locator('#agent-family-drawer')).toBeVisible()
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
  test('the family mark anchors to the glyph column, member row or not', async ({ page }) => {
    await openMockSidebar(page, '/my-project/claude/~fam2kid')
    const geometry = await page.evaluate(() => {
      const read = (entry: Element) => {
        const slot = entry.querySelector('.family-slot')
        const mark = entry.querySelector('.family-activity .family-activity-icon')
        if (!mark) return null
        return {
          markCX: Math.round(mark.getBoundingClientRect().x + mark.getBoundingClientRect().width / 2),
          memberGlyphCX: slot
            ? Math.round(
              slot.querySelector('.family-glyph')!.getBoundingClientRect().x
                + slot.querySelector('.family-glyph')!.getBoundingClientRect().width / 2,
            )
            : null,
        }
      }
      return [...document.querySelectorAll('.session-family')].map(read).filter(Boolean)
    })

    const withMember = geometry.filter(g => g!.memberGlyphCX !== null)
    const withoutMember = geometry.filter(g => g!.memberGlyphCX === null)
    expect(withMember.length, 'a family with a member row on screen').toBeGreaterThan(0)
    expect(withoutMember.length, 'a family without one').toBeGreaterThan(0)

    // The line is the standard family numbers — everyone beneath the
    // root — not "the others", so it anchors to the root's glyph column
    // and stays put whatever member row happens to be shown above it.
    // (The old `+` indented under the member's title, because it meant
    // "in addition to the one named"; that meaning is gone.)
    const columns = new Set(geometry.map(g => g!.markCX))
    expect(columns.size, 'one column for every family, slot row or not').toBe(1)
    for (const g of withMember) expect(g!.markCX).toBe(g!.memberGlyphCX)
  })

  test("the panel's tally names states in the turn model's words", async ({ page }) => {
    await openMockSidebar(page, '/my-project/claude/~fam2kid')
    await page.locator('.family-trigger').click()
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
      proc: n.querySelector('.family-proc')?.textContent ?? null,
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
    await page.locator('.family-trigger').click()
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
    await page.locator('.family-trigger').click()
    const counts = page.locator('.family-counts')
    await counts.waitFor()
    const tally = (label: string) => counts.locator('.family-count').filter({ hasText: label })
    const action = page.locator('.family-mark-read')

    // No filter, no verb: a bulk action only exists while you are
    // looking at the complete list of what it will touch.
    await expect(action).toHaveCount(0)

    const expected: [string, RegExp][] = [
      ['waiting', /^Mark all read$/],
      ['error', /^Mark all read$/],
      ['running', /^Stop all \d+$/],
      ['active', /^Interrupt all \d+$/],
    ]
    for (const [state, verb] of expected) {
      await tally(state).click()
      await expect(action).toHaveText(verb)
      await tally('all').click()
      await expect(action).toHaveCount(0)
    }

    // Each verb hits its own endpoint, with its own state's members.
    const hits: Record<string, string[]> = { read: [], kill: [], cancel: [] }
    for (const verb of ['read', 'kill', 'cancel']) {
      await page.route(`**/v1/sessions/**/${verb}*`, route => {
        hits[verb].push(new URL(route.request().url()).pathname)
        route.fulfill({ status: 200, body: '{}' })
      })
    }

    await tally('waiting').click()
    await action.click()
    await expect.poll(() => hits.read.length).toBeGreaterThan(0)
    // Only fam1kid is `waiting` here: the errored member is under
    // `error`, precedence keeps it out of this verb's reach.
    expect(hits.read.every(path => path.includes('fam1kid'))).toBe(true)

    // Stop all → /kill, and only the running process.
    await tally('all').click()
    await tally('running').click()
    await expect(action).toHaveText(/^Stop all \d+$/)
    await action.click()
    await expect.poll(() => hits.kill.length).toBe(1)
    expect(hits.kill[0]).toContain('fam0proc')
    expect(hits.cancel).toHaveLength(0)

    // Interrupt all → /cancel, on the active agents and nothing else.
    await tally('all').click()
    await tally('active').click()
    await expect(action).toHaveText(/^Interrupt all \d+$/)
    await action.click()
    await expect.poll(() => hits.cancel.length).toBeGreaterThan(0)
    // Never the root: the tally counts descendants only, the label
    // quotes the tally, and the verb touches what the label counted.
    // You act on the root by visiting it.
    expect(hits.cancel.every(path => !path.includes('fam0root'))).toBe(true)
    expect(hits.cancel.some(path => path.includes('fam2kid'))).toBe(true)
    expect(hits.cancel.every(path => !path.includes('fam0proc'))).toBe(true)
    expect(hits.kill).toHaveLength(1)
  })

  test('a bulk verb names its blast radius, including folded members', async ({ page }) => {
    // The panel's budget folds a big family, but the verb acts on the
    // filter, not the viewport — so the count in the label is the only
    // honest statement of what the click will touch.
    await openMockSidebar(page, '/my-project/claude/~fam2kid')
    await page.locator('.family-trigger').click()
    const counts = page.locator('.family-counts')
    await counts.waitFor()
    const tally = (label: string) => counts.locator('.family-count').filter({ hasText: label })
    await tally('active').click()
    const tallied = Number((await tally('active').textContent())?.match(/\d+/)?.[0])
    await expect(page.locator('.family-mark-read')).toHaveText(`Interrupt all ${tallied}`)
  })

  test('ancestors survive a long title, and quiet crumbs carry no dot hole', async ({ page }) => {
    // fam4kid: three ancestors (shown as root › … › parent) and a title
    // long enough to want every pixel the crumbs have.
    await page.setViewportSize({ width: 800, height: 700 })
    await openMockSidebar(page, '/my-project/claude/~fam4kid')
    const crumbs = page.locator('.header-crumb')
    await expect(crumbs).toHaveCount(2)
    // Survival means being readable, not merely attached: each crumb
    // title keeps real width even while the long title is ellipsizing.
    for (const width of await crumbs.locator('.header-crumb-title')
      .evaluateAll(nodes => nodes.map(n => n.getBoundingClientRect().width))) {
      expect(width).toBeGreaterThan(20)
    }
    // A quiet ancestor gets no dot at all — the sidebar's invisible
    // `none` placeholder would be a permanent hole in a crumb.
    expect(await page.locator('.header-crumb .session-dot-indicator.none').count()).toBe(0)
  })

  test('the header trigger previews the tally it opens onto', async ({ page }) => {
    await openMockSidebar(page, '/my-project/claude/~fam2kid')
    const trigger = page.locator('.family-trigger')
    // Segments, not the old icon-and-badge: dot + count per state, in
    // the same order the panel's tally will list them.
    const segs = await trigger.locator('.family-trigger-seg').evaluateAll(nodes => nodes.map(n => ({
      count: n.textContent?.trim(),
      dot: n.querySelector('.session-dot-indicator')?.className ?? n.querySelector('.family-trigger-proc')?.textContent,
    })))
    expect(segs.length).toBeGreaterThan(1)
    await trigger.click()
    const tally = await page.locator('.family-count').allTextContents()
    // The standard family numbers on both sides of the click: every
    // descendant of the root, the root excluded, whoever is viewing.
    // Exactly equal — the button is the tally's preview, and the same
    // dots must never wear different numbers.
    const tallyCount = (label: string) =>
      Number(tally.find(t => t.includes(label))?.match(/\d+/)?.[0] ?? 0)
    const segCount = (dot: string) =>
      Number(segs.find(s => s.dot?.includes(dot))?.count?.replace(/\D/g, '') ?? 0)
    expect(segCount('working')).toBe(tallyCount('active'))
    expect(segCount('error')).toBe(tallyCount('error'))
    expect(segCount('unread')).toBe(tallyCount('waiting'))
    expect(segCount('$')).toBe(tallyCount('running'))
    expect(tallyCount('active')).toBeGreaterThan(0)
  })

  test('a process row wears the state the tally counted it under', async ({ page }) => {
    // famBroot is a process-only family: one running command, one that
    // finished with output you have not seen. The `$` is the only place
    // that row's state can appear, so a stateless glyph would leave the
    // member the tally counts — and the `waiting` filter shows —
    // unmarked on screen.
    await openMockSidebar(page, '/my-project/claude/~famBroot')
    await page.locator('.family-trigger').click()
    const glyphs = await page.locator('.family-row').evaluateAll(rows => rows.map(row => ({
      title: row.querySelector('.family-row-title')?.textContent ?? '',
      glyph: row.querySelector('.family-proc')?.className ?? null,
    })))
    const gofmt = glyphs.find(g => g.title.startsWith('gofmt'))
    expect(gofmt?.glyph, 'the waiting process is marked as waiting').toContain('unread')
    const build = glyphs.find(g => g.title.startsWith('vite build'))
    expect(build?.glyph, 'and the running one as running').toContain('working')
  })
})

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

    // It remains inside the family's broad hit area, and hovering the
    // nested target answers in the sidebar's own language: the member
    // row's background treatment, not the header's bordered pill.
    const hoverBg = async (loc: typeof indicator) => {
      await loc.hover()
      return loc.evaluate(el => getComputedStyle(el).backgroundColor)
    }
    // It uses the sidebar's flat background treatment rather than a
    // bordered header pill.
    expect(await hoverBg(indicator)).not.toBe('rgba(0, 0, 0, 0)')
    expect(await indicator.evaluate(el => getComputedStyle(el).borderStyle)).toBe('none')

    // The hit area stops where the numbers do: the slack to its right
    // belongs to the root, like the rest of the entry's slack — a
    // click there must select the root without opening the panel.
    const rowBox = (await entry.boundingBox())!
    const buttonBox = (await indicator.boundingBox())!
    await page.mouse.click(rowBox.x + rowBox.width - 12, buttonBox.y + buttonBox.height / 2)
    await expect(page).toHaveURL(/~famBroot/)
    await expect(page.locator('#agent-family-drawer')).toBeHidden()

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

    // With the panel open and nowhere left to navigate, a second press
    // is a dismissal — the indicator toggles rather than insists.
    await indicator.click()
    await expect(page.locator('#agent-family-drawer')).toBeHidden()
    await indicator.click()
    await expect(page.locator('#agent-family-drawer')).toBeVisible()

    // Leaving the family closes its drawer, and coming back does not
    // pop it open again: open-ness is a statement about the present,
    // not a preference to be remembered.
    await familyEntry(page, 'orchestrator').locator('.session-item').first().click()
    await expect(page.locator('#agent-family-drawer')).toBeHidden()
    await page.goBack()
    await expect(page).toHaveURL(/~famBroot/)
    await expect(page.locator('#agent-family-drawer')).toBeHidden()
  })

  test('the family button is static; only the content after it changes', async ({ page }) => {
    await openMockSidebar(page, '/my-project/claude/~fam2kid')
    const family = familyEntry(page, 'orchestrator')
    // Member selected: the button stays (the panel's entry point never
    // teleports) but sheds its counts — the member row is the content.
    await expect(family.locator('.family-activity')).toHaveCount(1)
    await expect(family.locator('.family-slot')).toHaveCount(1)
    await expect(family.locator('.family-activity-seg')).toHaveCount(0)

    await page.locator('.session-item').filter({ hasText: 'design landing page' }).click()
    await expect(family.locator('.family-slot')).toHaveCount(0)
    await expect(family.locator('.family-activity')).toHaveCount(1)
    expect(await family.locator('.family-activity-seg').count()).toBeGreaterThan(0)

    // Root selection keeps the summary and never resurrects history.
    await family.locator('.session-item').first().click()
    await expect(family.locator('.family-slot')).toHaveCount(0)
    await expect(family.locator('.family-activity')).toHaveCount(1)
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
  test('the family mark anchors every subordinate row to one glyph column', async ({ page }) => {
    await openMockSidebar(page, '/my-project/claude/~fam2kid')
    const geometry = await page.evaluate(() => [...document.querySelectorAll('.session-family')].map(entry => {
      const slot = entry.querySelector('.family-slot')
      const mark = entry.querySelector('.family-activity .family-activity-icon')
      const segs = entry.querySelectorAll('.family-activity-seg').length
      const center = (el: Element | null) => el
        ? Math.round(el.getBoundingClientRect().x + el.getBoundingClientRect().width / 2)
        : null
      return { memberCX: center(slot?.querySelector('.family-glyph') ?? null), markCX: center(mark), segs }
    }))

    expect(geometry.some(g => g.memberCX !== null), 'the selected family names its member').toBe(true)
    expect(geometry.some(g => g.segs > 0), 'inactive families retain their summaries').toBe(true)
    for (const g of geometry) {
      // The mark is the row's static head: wherever a subordinate row
      // exists, the mark leads it — and the counts and the member row
      // never share it (one subordinate row's worth of content).
      if (g.memberCX !== null || g.segs > 0) expect(g.markCX, 'the mark leads every subordinate row').not.toBeNull()
      expect(g.memberCX === null || g.segs === 0, 'counts and member never share the row').toBe(true)
      if (g.memberCX !== null && g.markCX !== null)
        expect(g.memberCX, 'the member sits after the mark').toBeGreaterThan(g.markCX)
    }
    expect(new Set(geometry.flatMap(g => g.markCX === null ? [] : [g.markCX])).size,
      'one glyph column for every family mark').toBe(1)
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

  test('a selected active retry stays active but uses the hollow red ring', async ({ page }) => {
    await openMockSidebar(page, '/my-project/claude/~fam2kid')
    await page.locator('.family-trigger').click()

    const retry = page.locator('.family-row[aria-current="page"] .session-dot-indicator')
    await expect(retry).toHaveClass(/active-error/)
    const terminalError = page.locator('.family-row').filter({
      hasText: 'investigate a really long descendant',
    }).locator('.session-dot-indicator')
    await expect(terminalError).toHaveClass(/error/)

    const colors = await page.evaluate(() => {
      const retryDot = document.querySelector('.family-row[aria-current="page"] .session-dot-indicator')
      const errorRow = [...document.querySelectorAll('.family-row')].find(row =>
        row.textContent?.includes('investigate a really long descendant'))
      const errorDot = errorRow?.querySelector('.session-dot-indicator')
      if (!retryDot || !errorDot) throw new Error('expected retry and terminal-error dots')
      return {
        retryBackground: getComputedStyle(retryDot).backgroundColor,
        retryBorder: getComputedStyle(retryDot).borderColor,
        errorBackground: getComputedStyle(errorDot).backgroundColor,
      }
    })
    expect(colors.retryBackground).toBe('rgba(0, 0, 0, 0)')
    expect(colors.retryBorder).toBe(colors.errorBackground)
    await expect(page.locator('.family-count').filter({ hasText: '1 active' })).toBeVisible()
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

    // Processes are a type filter, not a state filter: it leaves the tree
    // for a flat task-runner view containing both running and finished work.
    await tally('running').click()
    await expect(tally('error')).toHaveAttribute('aria-pressed', 'false')
    const processes = await titles()
    expect(processes.some(t => t.includes('pnpm test'))).toBe(true)
    expect(processes.some(t => t.startsWith('investigate a really long descendant'))).toBe(false)
    await expect(page.locator('.family-process-section h3').first()).toContainText('Running')
    await expect(page.locator('.family-row[aria-current="page"]')).toHaveCount(0)

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
    // looking at the complete list of what it will touch. Reserving one
    // stable header height keeps the tree from jumping when the verb appears.
    await expect(action).toHaveCount(0)
    const head = page.locator('.family-drawer-head')
    const heightWithoutAction = await head.evaluate(node => node.getBoundingClientRect().height)
    await tally('waiting').click()
    await expect(action).toBeVisible()
    expect(await head.evaluate(node => node.getBoundingClientRect().height)).toBe(heightWithoutAction)
    await tally('all').click()

    const expected: [string, RegExp][] = [
      ['waiting', /^Mark all read$/],
      ['error', /^Mark all read$/],
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

    // Processes are history, not an attention queue; even while one is
    // running, the type filter has no bulk verb.
    await tally('all').click()
    await tally('running').click()
    await expect(action).toHaveCount(0)
    expect(hits.kill).toHaveLength(0)

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
    expect(hits.kill).toHaveLength(0)
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

  test('the processes filter separates running work from finished history', async ({ page }) => {
    // famBroot is process-only: one running command and one finished with
    // unread output. Unread completion is not agent attention, so the control
    // names only the running count while opening both lifecycle sections.
    await openMockSidebar(page, '/my-project/claude/~famBroot')
    await page.locator('.family-trigger').click()
    const counts = page.locator('.family-counts')
    await expect(counts).not.toContainText('waiting')
    const processes = counts.locator('.family-count').filter({ hasText: 'running' })
    await expect(processes).toHaveAccessibleName('Processes, 1 running')
    await processes.click()
    await expect(page.locator('.family-process-section h3')).toHaveText(['Running · 1', 'Finished · 1'])
    const glyphs = page.locator('.family-process-list .family-proc')
    await expect(glyphs).toHaveCount(2)
    // The $ never changes shape between rows, but it does carry the one
    // process fact: the running row's glyph is lit, the finished row's is
    // dimmed — two lifecycles, two tones.
    const colors = await glyphs.evaluateAll(nodes => nodes.map(node => getComputedStyle(node).color))
    expect(new Set(colors).size, 'running and finished use distinct tones').toBe(2)
  })

  test('agent-state filters do not pin the selected process into their rows', async ({ page }) => {
    await openMockSidebar(page, '/my-project/shell/~fam0proc')
    await page.locator('.family-trigger').click()
    const error = page.locator('.family-count').filter({ hasText: 'error' })
    await error.click()
    await expect(page.locator('.family-row.process')).toHaveCount(0)
    await expect(page.locator('.family-row[aria-current="page"]')).toHaveCount(0)
  })

  test('a selected finished process keeps one line when running fills the budget', async ({ page }) => {
    await openMockSidebar(page, '/my-project/shell/~famSfinished')
    await page.locator('.family-trigger').click()
    await page.locator('.family-count').filter({ hasText: 'running' }).click()
    const running = page.locator('.family-process-section').filter({ hasText: 'Running' })
    const finished = page.locator('.family-process-section').filter({ hasText: 'Finished' })
    await expect(running.locator('.family-row')).toHaveCount(24)
    await expect(running.locator('.family-more')).toHaveText(/\+1 more/)
    await expect(finished.locator('.family-row')).toHaveCount(1)
    await expect(finished.locator('.family-row')).toHaveAttribute('aria-current', 'page')
    await expect(page.locator('.family-process-list .family-row')).toHaveCount(25)
  })

  test('a finished-only family offers quiet process history without a summary', async ({ page }) => {
    await openMockSidebar(page, '/my-project/claude/~famQroot')
    // Nothing is running, so the header remains the quiet family icon rather
    // than advertising process activity.
    await expect(page.locator('.family-trigger-proc')).toHaveCount(0)
    await page.locator('.family-trigger').click()
    const control = page.locator('.family-count').filter({ hasText: 'processes' })
    await expect(control).toHaveAccessibleName('Processes')
    // Nothing runs here, so every $ in sight is quiet: the history control
    // and the finished rows share the same dimmed tone.
    const processColor = await control.locator('.family-proc').evaluate(node => getComputedStyle(node).color)
    const rowColor = await page.locator('.family-row.process .family-proc').first().evaluate(node => getComputedStyle(node).color)
    expect(processColor, 'finished history is uniformly quiet').toBe(rowColor)
    await control.click()
    await expect(page.locator('.family-process-section h3')).toHaveText(['Finished · 28'])
    const list = page.locator('.family-process-list')
    await expect(list.locator('.family-row')).toHaveCount(25)
    await expect(list.locator('.family-more')).toHaveText(/\+3 more/)
    await expect(list.locator('.family-more')).toHaveAttribute('aria-expanded', 'false')
    await list.locator('.family-more').click()
    await expect(list.locator('.family-row')).toHaveCount(28)
    await expect(list.locator('.family-more')).toHaveText(/show fewer/)
    await expect(list.locator('.family-more')).toHaveAttribute('aria-expanded', 'true')
  })
})

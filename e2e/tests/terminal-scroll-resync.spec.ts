import { expect, test, type Page } from '@playwright/test'
import { gotoTestSession, openApp } from '../helpers'

/**
 * The first wheel/touch scroll after a reflow must move one notch from the
 * row on screen, not jump back to wherever the user last scrolled by hand.
 *
 * xterm's Viewport applies wheel/touch deltas to its scrollable element's
 * position. Output-driven scrolls, reflow on resize and ED3 move the
 * buffer's ydisp without going through that element; the fork's sync used
 * to compare against `_latestYDisp`, which only viewport-driven scrolls
 * update, so after a reflow the scroll position stayed at the pre-resize
 * offset. Programs that redraw their live region in place (pi's
 * regular/main-screen TUI) never append a line that would resync it, so the
 * next scroll-up landed on the stale offset: the position from before the
 * agent started producing output.
 *
 * Bytes go through `__gmuxInject` (the same TerminalIO path as the
 * WebSocket); the resize is a real browser viewport change so gmux's own
 * fit/claim path performs it.
 */

const BSU = '\x1b[?2026h'
const ESU = '\x1b[?2026l'

async function inject(page: Page, frame: string): Promise<void> {
  await page.evaluate((data) => (window as any).__gmuxInject(data), Buffer.from(frame).toString('base64'))
  await settle(page)
}

async function settle(page: Page): Promise<void> {
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(() => r(null)))))
}

async function scroll(page: Page): Promise<{ viewportY: number, baseY: number, cols: number, rows: number }> {
  return page.evaluate(() => {
    const term = (window as any).__gmuxTerm
    const buffer = term.buffer.active
    return { viewportY: buffer.viewportY, baseY: buffer.baseY, cols: term.cols, rows: term.rows }
  })
}

async function waitForGeometryChange(page: Page, before: { cols: number, rows: number }): Promise<void> {
  await page.waitForFunction(({ cols, rows }) => {
    const term = (window as any).__gmuxTerm
    return term.cols !== cols || term.rows !== rows
  }, before, { timeout: 5_000 })
  // The fit can land in more than one step (rows and cols, claim echo);
  // wait until the geometry has been stable for a few frames.
  let last = ''
  for (let stable = 0; stable < 5;) {
    await page.waitForTimeout(60)
    const now = JSON.stringify(await page.evaluate(() => [(window as any).__gmuxTerm.cols, (window as any).__gmuxTerm.rows]))
    stable = now === last ? stable + 1 : 0
    last = now
  }
  await settle(page)
}

async function wheel(page: Page, deltaY: number, count: number): Promise<void> {
  const box = await page.locator('.xterm').boundingBox()
  if (!box) throw new Error('xterm has no bounding box')
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
  for (let i = 0; i < count; i++) {
    await page.mouse.wheel(0, deltaY)
    await page.waitForTimeout(40)
  }
  await settle(page)
}

/** Seed scrollback whose lines rewrap when the column count changes. */
async function seed(page: Page): Promise<void> {
  const lines = Array.from({ length: 300 }, (_, i) => `seed-${String(i).padStart(4, '0')} ${'w'.repeat(i % 90)}`)
  await inject(page, lines.join('\r\n') + '\r\n')
  await page.evaluate(() => (window as any).__gmuxScrollAnchor.follow())
  await settle(page)
}

/** pi-style live-region redraw: rewrite the last lines in place, no new rows. */
async function redrawLiveRegionInPlace(page: Page, frames: number): Promise<void> {
  for (let k = 0; k < frames; k++) {
    await inject(page, `${BSU}\x1b[1A\r\x1b[2Kworking ${'|/-\\'[k % 4]} ${k}\r\n\x1b[2Kstatus line${ESU}`)
  }
}

test.describe('desktop wheel after reflow', () => {
  test.use({ viewport: { width: 1200, height: 800 } })

  test.beforeEach(async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)
    await seed(page)
  })

  test('anchored: first wheel-up after resize moves one notch, not back to the pre-resize row', async ({ page }) => {
    await wheel(page, -100, 8)
    const anchored = await scroll(page)
    expect(anchored.viewportY).toBeLessThan(anchored.baseY)

    await page.setViewportSize({ width: 900, height: 600 })
    await waitForGeometryChange(page, anchored)
    await redrawLiveRegionInPlace(page, 10)

    const before = await scroll(page)
    // Reflow must actually move ydisp, otherwise the stale offset is
    // indistinguishable from the current one.
    expect(before.viewportY).not.toBe(anchored.viewportY)
    await wheel(page, -100, 1)
    const after = await scroll(page)
    const moved = before.viewportY - after.viewportY
    expect(moved, `before=${JSON.stringify(before)} after=${JSON.stringify(after)} anchored=${anchored.viewportY}`).toBeGreaterThan(0)
    expect(moved).toBeLessThanOrEqual(6)
  })

  test('following: first wheel-up after resize leaves the bottom by one notch', async ({ page }) => {
    // Scroll around first, like the user browsing scrollback before the
    // agent's next burst of output.
    await wheel(page, -100, 8)
    await wheel(page, 100, 20)
    const atBottom = await scroll(page)
    expect(atBottom.viewportY).toBe(atBottom.baseY)

    await page.setViewportSize({ width: 900, height: 600 })
    await waitForGeometryChange(page, atBottom)
    await redrawLiveRegionInPlace(page, 10)

    const before = await scroll(page)
    expect(before.viewportY).toBe(before.baseY)
    expect(before.baseY).not.toBe(atBottom.baseY)
    await wheel(page, -100, 1)
    const after = await scroll(page)
    const moved = before.viewportY - after.viewportY
    expect(moved, `before=${JSON.stringify(before)} after=${JSON.stringify(after)} oldBottom=${atBottom.baseY}`).toBeGreaterThan(0)
    expect(moved).toBeLessThanOrEqual(6)
  })
})

test.describe('desktop wheel after scrollback regrowth', () => {
  test.use({ viewport: { width: 1200, height: 800 } })

  test.beforeEach(async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)
    await seed(page)
  })

  /**
   * `_latestYDisp` is left at the row of the last viewport-driven scroll.
   * When output later lands ydisp on exactly that row (here: an unfenced
   * ED3 + redraw that regrows the same scrollback), the old guard
   * `ydisp !== _latestYDisp` skipped the final sync and the scroll position
   * stayed one row behind, so the next notch moved one row too far.
   */
  test('a redraw that regrows ydisp onto the last user-scrolled row keeps wheel notches exact', async ({ page }) => {
    const bottom = await scroll(page)
    await wheel(page, -100, 1)
    const notch = bottom.viewportY - (await scroll(page)).viewportY
    expect(notch).toBeGreaterThan(0)
    await page.evaluate(() => (window as any).__gmuxScrollAnchor.follow())
    await settle(page)
    expect((await scroll(page)).viewportY).toBe(bottom.baseY)

    await inject(page, '\x1b[2J\x1b[H\x1b[3J')
    const lines = Array.from({ length: bottom.baseY + bottom.rows - 1 }, (_, i) => `regrow-${i}`)
    await inject(page, lines.join('\r\n') + '\r\n')
    const before = await scroll(page)
    expect(before.baseY).toBe(bottom.baseY)
    expect(before.viewportY).toBe(before.baseY)

    await wheel(page, -100, 1)
    const after = await scroll(page)
    expect(before.viewportY - after.viewportY, `before=${JSON.stringify(before)} after=${JSON.stringify(after)}`).toBe(notch)
  })
})

test.describe('mobile touch after resize', () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true })

  async function swipe(page: Page, dy: number): Promise<void> {
    const box = await page.locator('.xterm-screen').boundingBox()
    if (!box) throw new Error('xterm has no bounding box')
    const client = await page.context().newCDPSession(page)
    const x = box.x + box.width / 2
    const startY = box.y + box.height / 2
    const steps = 6
    await client.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y: startY }] })
    for (let i = 1; i <= steps; i++) {
      await client.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x, y: startY + dy * i / steps }] })
      await page.waitForTimeout(16)
    }
    await client.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] })
    await client.detach()
    // Let any touch momentum finish before sampling.
    await page.waitForTimeout(300)
    await settle(page)
  }

  test.beforeEach(async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)
    await seed(page)
  })

  test('anchored: first swipe after the soft keyboard opens scrolls from the row on screen', async ({ page }) => {
    // Finger down = scroll toward older output.
    await swipe(page, 150)
    const anchored = await scroll(page)
    expect(anchored.viewportY).toBeLessThan(anchored.baseY)

    // Soft keyboard shrinking the visual viewport: fewer rows, so xterm
    // moves ydisp down to keep the same lines in view.
    await page.setViewportSize({ width: 390, height: 500 })
    await waitForGeometryChange(page, anchored)
    await redrawLiveRegionInPlace(page, 10)

    const before = await scroll(page)
    expect(before.viewportY).toBeGreaterThan(anchored.viewportY)
    await swipe(page, 60)
    const after = await scroll(page)
    const moved = before.viewportY - after.viewportY
    expect(moved, `before=${JSON.stringify(before)} after=${JSON.stringify(after)} anchored=${anchored.viewportY}`).toBeGreaterThan(0)
    expect(moved).toBeLessThanOrEqual(8)
  })
})

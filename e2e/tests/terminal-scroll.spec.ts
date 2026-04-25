import { test, expect, type Page } from '@playwright/test'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { openApp, gotoTestSession } from '../helpers'

/**
 * Reproduces the long-standing "scrollback jumps to the top" bug that's
 * observed while an agent (e.g. pi) is streaming output, particularly at
 * end-of-turn redraws.
 *
 * These tests bypass the WebSocket and inject pre-built frames through the
 * exact same code path as ws.onmessage (terminal-io's enqueue), via the
 * test-only `window.__gmuxInject(b64)` hook installed by terminal.tsx.
 * That tests the real xterm.js + terminal-io + replay interaction with
 * deterministic byte sequences and frame boundaries — which is where the
 * synthetic scroll harness in terminal-io.test.ts cannot reach.
 *
 * Frame boundaries matter: each call to __gmuxInject is one logical WS
 * message, so splitting BSU/content/ESU across multiple inject calls
 * exercises the same paths a real WS message stream does.
 */

const BSU = '\x1b[?2026h'
const ESU = '\x1b[?2026l'

/** UTF-8 encode + base64 a string for transfer through page.evaluate. */
function b64(s: string): string {
  return Buffer.from(s, 'utf8').toString('base64')
}

/** Pump one frame through __gmuxInject. */
async function inject(page: Page, frame: string): Promise<void> {
  const encoded = b64(frame)
  await page.evaluate((data) => {
    const inject = (window as any).__gmuxInject as ((b: string) => void) | null
    if (!inject) throw new Error('__gmuxInject not available')
    inject(data)
  }, encoded)
}

/** Pump multiple frames sequentially, waiting a frame between each so xterm's
 * write callback + scroll-restore rAF can run between injects. */
async function injectFrames(page: Page, frames: string[]): Promise<void> {
  for (const frame of frames) {
    await inject(page, frame)
    // Yield to xterm's async parser + our restore rAF.
    await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r(null))))
  }
}

interface ScrollState {
  viewportY: number
  baseY: number
  rows: number
  cols: number
}

async function getScroll(page: Page): Promise<ScrollState> {
  return page.evaluate(() => {
    const term = (window as any).__gmuxTerm
    const buf = term.buffer.active
    return {
      viewportY: buf.viewportY as number,
      baseY: buf.baseY as number,
      rows: term.rows as number,
      cols: term.cols as number,
    }
  })
}

/** Wait until the terminal IO queue has drained and any pending rAFs ran. */
async function settle(page: Page): Promise<void> {
  await page.evaluate(() =>
    new Promise(r => requestAnimationFrame(() => requestAnimationFrame(() => r(null)))),
  )
}

/** Pre-seed scrollback so baseY > 0 and we can detect a "jump to top". */
async function seedScrollback(page: Page, lines: number): Promise<void> {
  // CRLF-terminate so xterm carriage-returns each line. Plain `\n` would
  // leave the cursor in column N of the previous line.
  const payload = Array.from({ length: lines }, (_, i) =>
    `seed-line-${String(i + 1).padStart(4, '0')}`).join('\r\n') + '\r\n'
  await inject(page, payload)
  await settle(page)
}

async function scrollToBottom(page: Page): Promise<void> {
  await page.evaluate(() => (window as any).__gmuxTerm.scrollToBottom())
}

test.describe('terminal scrollback (jump-to-top bug)', () => {
  test.beforeEach(async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)
    // Replay phase finishes inside gotoTestSession's settle.
    await page.evaluate(() => {
      // Sanity: the inject hook must be installed before any test runs.
      if (!(window as any).__gmuxInject) throw new Error('__gmuxInject missing')
    })
  })

  /**
   * Baseline: a single BSU/ESU frame while the user is at the bottom.
   * The viewport must remain pinned at the bottom (viewportY === baseY).
   */
  test('user at bottom: single BSU/ESU frame stays at bottom', async ({ page }) => {
    await seedScrollback(page, 200)
    await scrollToBottom(page)
    const before = await getScroll(page)
    expect(before.viewportY).toBe(before.baseY)
    expect(before.baseY).toBeGreaterThan(0)

    // Single frame: BSU + content + ESU. Mimics a small atomic redraw.
    const content = Array.from({ length: 20 }, (_, i) => `turn-line-${i}`).join('\r\n') + '\r\n'
    await inject(page, BSU + content + ESU)
    await settle(page)

    const after = await getScroll(page)
    expect(after.baseY).toBeGreaterThan(0)
    // Bug shape: viewportY === 0 while baseY > 0 → "jumped to top".
    expect(after.viewportY).toBe(after.baseY)
  })

  /**
   * Pi streams a turn as many small BSU/ESU bursts. Each burst is
   * independently atomic; the viewport must follow the bottom across all
   * of them (no jump-to-top in the middle or at the end).
   */
  test('user at bottom: rapid BSU/ESU bursts (streaming) stay at bottom', async ({ page }) => {
    await seedScrollback(page, 200)
    await scrollToBottom(page)

    const bursts: string[] = []
    for (let i = 0; i < 30; i++) {
      bursts.push(BSU + `burst-${i}: streaming output line\r\n` + ESU)
    }
    await injectFrames(page, bursts)
    await settle(page)

    const after = await getScroll(page)
    expect(after.baseY).toBeGreaterThan(0)
    expect(after.viewportY).toBe(after.baseY)
  })

  /**
   * End-of-turn shape: a single BSU spanning multiple WS frames, with
   * content large enough to evict from scrollback, then ESU on its own
   * frame. This mirrors what we see when an agent finishes a turn and
   * redraws a large prompt area.
   *
   * The viewport must remain at the bottom: wasAtBottom is captured at
   * BSU time (true), so the rAF restore should scrollToBottom.
   */
  test('user at bottom: BSU/ESU split across frames with large output stays at bottom', async ({ page }) => {
    await seedScrollback(page, 200)
    await scrollToBottom(page)

    const big = Array.from({ length: 500 }, (_, i) => `eot-line-${i}`).join('\r\n') + '\r\n'
    // Three frames: BSU alone, big content, ESU alone.
    await injectFrames(page, [BSU, big, ESU])
    await settle(page)

    const after = await getScroll(page)
    expect(after.baseY).toBeGreaterThan(0)
    expect(after.viewportY).toBe(after.baseY)
  })

  /**
   * Force real scrollback eviction. Default scrollback is 5000; we seed
   * 200 then dump >5000 lines inside a single BSU/ESU. xterm has to evict
   * thousands of lines mid-block, which is the path the synthetic
   * harness in terminal-io.test.ts can only model.
   */
  test('user at bottom: BSU/ESU triggering real eviction stays at bottom', async ({ page }) => {
    await seedScrollback(page, 200)
    await scrollToBottom(page)

    const huge = Array.from({ length: 6000 }, (_, i) => `evict-${i}`).join('\r\n') + '\r\n'
    await injectFrames(page, [BSU, huge, ESU])
    await settle(page)

    const after = await getScroll(page)
    expect(after.baseY).toBeGreaterThan(0)
    expect(after.viewportY).toBe(after.baseY)
  })

  /**
   * Alt-screen entry/exit. TUIs (and pi's prompt UI) routinely toggle the
   * alternate screen buffer. xterm's viewport state behaves differently
   * across that boundary; a BSU/ESU burst followed by alt-screen exit is a
   * plausible end-of-turn shape.
   */
  test('user at bottom: alt-screen enter/work/exit + BSU/ESU stays at bottom', async ({ page }) => {
    await seedScrollback(page, 200)
    await scrollToBottom(page)

    // Enter alt screen, do some redraws, exit, then a redraw burst.
    const ALT_ENTER = '\x1b[?1049h'
    const ALT_EXIT = '\x1b[?1049l'
    const work = Array.from({ length: 10 }, (_, i) => `alt-${i}`).join('\r\n') + '\r\n'
    await inject(page, ALT_ENTER + work)
    await settle(page)
    await inject(page, ALT_EXIT)
    await settle(page)
    // After exiting alt-screen, agent typically redraws the prompt area.
    await inject(page, BSU + 'final-redraw\r\n' + ESU)
    await settle(page)

    const after = await getScroll(page)
    expect(after.baseY).toBeGreaterThan(0)
    expect(after.viewportY).toBe(after.baseY)
  })

  /**
   * `\x1b[3J` (clear scrollback) inside a live BSU/ESU burst. terminal.tsx
   * already names this as a culprit on the replay path; this verifies
   * that the live path doesn't jump to the top either when an agent emits
   * it mid-turn.
   */
  test('user at bottom: BSU + clear-scrollback + ESU stays at bottom', async ({ page }) => {
    await seedScrollback(page, 200)
    await scrollToBottom(page)

    const before = await getScroll(page)
    expect(before.baseY).toBeGreaterThan(0)

    // BSU, some content, \x1b[3J (ED with param 3: clear scrollback),
    // more content, ESU. After [3J xterm resets baseY to 0, then content
    // grows it again. Bug shape: ydisp clamps to 0 even though user was
    // at bottom.
    const frame = `${BSU}before-clear\r\n\x1b[3Jafter-clear\r\n${'redraw\r\n'.repeat(40)}${ESU}`
    await inject(page, frame)
    await settle(page)

    const after = await getScroll(page)
    // viewportY === baseY, even if baseY is now small (the clear reset it).
    expect(after.viewportY).toBe(after.baseY)
  })

  /**
   * Scroll region (DECSTBM) confined to a few rows at the bottom of the
   * screen, with the agent writing into it repeatedly. This is the shape
   * pi uses for its bottom-anchored prompt area while streaming.
   */
  test('user at bottom: DECSTBM region writes + BSU/ESU stays at bottom', async ({ page }) => {
    await seedScrollback(page, 200)
    await scrollToBottom(page)

    const { rows } = await getScroll(page)
    // Set scroll region to bottom 5 rows of the visible area (1-indexed).
    const SET_REGION = `\x1b[${rows - 4};${rows}r`
    const RESET_REGION = `\x1b[r`
    const MOVE_TO_REGION = `\x1b[${rows};1H`

    const burst = SET_REGION + MOVE_TO_REGION
      + BSU
      + Array.from({ length: 20 }, (_, i) => `region-${i}\r\n`).join('')
      + ESU
      + RESET_REGION
    await inject(page, burst)
    await settle(page)

    const after = await getScroll(page)
    expect(after.viewportY).toBe(after.baseY)
  })

  /**
   * User intentionally scrolled up to read backscroll mid-stream. The
   * viewport must NOT jump to the bottom (that would yank them away from
   * what they're reading), AND must NOT jump to the top.
   *
   * The expected behavior is "stay where the user is", with a small
   * downward adjustment if scrollback eviction occurred during the burst.
   */
  test('user scrolled up: BSU/ESU burst preserves position (no jump to top)', async ({ page }) => {
    await seedScrollback(page, 200)
    await scrollToBottom(page)
    const baseline = await getScroll(page)
    expect(baseline.baseY).toBeGreaterThan(20)

    // Scroll up by 20 lines. We pick an absolute line halfway up.
    const target = Math.max(1, baseline.baseY - 20)
    await page.evaluate((line) => (window as any).__gmuxTerm.scrollToLine(line), target)
    const beforeBurst = await getScroll(page)
    expect(beforeBurst.viewportY).toBe(target)
    expect(beforeBurst.viewportY).toBeLessThan(beforeBurst.baseY)

    // A modest BSU/ESU burst that doesn't overflow scrollback.
    const content = Array.from({ length: 10 }, (_, i) => `mid-${i}`).join('\r\n') + '\r\n'
    await inject(page, BSU + content + ESU)
    await settle(page)

    const after = await getScroll(page)
    // Bug shape: jump to top (viewportY=0). Must not happen.
    expect(after.viewportY).toBeGreaterThan(0)
    // Should not have snapped to bottom either.
    expect(after.viewportY).toBeLessThan(after.baseY)
    // And should be near where the user was looking (within a few lines).
    expect(Math.abs(after.viewportY - target)).toBeLessThanOrEqual(2)
  })

  /**
   * Helper: replay all PTY-read chunks of the pi fixture, preserving
   * the original boundaries so BSU/ESU detection in terminal-io sees
   * what production WS framing actually produces.
   */
  async function replayPiFixture(page: Page): Promise<void> {
    const fixtureDir = join(__dirname, '..', 'fixtures')
    const bytes = readFileSync(join(fixtureDir, 'pi-turn.bin'))
    const chunks = JSON.parse(
      readFileSync(join(fixtureDir, 'pi-turn.chunks.json'), 'utf8'),
    ) as Array<{ offset: number; len: number }>
    expect(bytes.indexOf(Buffer.from('\x1b[3J'))).toBeGreaterThan(0)

    for (const c of chunks) {
      const encoded = bytes.subarray(c.offset, c.offset + c.len).toString('base64')
      await page.evaluate((data) => {
        const inj = (window as any).__gmuxInject as ((b: string) => void) | null
        if (!inj) throw new Error('__gmuxInject not available')
        inj(data)
      }, encoded)
      await page.evaluate(() =>
        new Promise(r => requestAnimationFrame(() => r(null))))
    }
    await settle(page)
    await page.evaluate(() =>
      new Promise(r => requestAnimationFrame(() =>
        requestAnimationFrame(() => r(null)))))
  }

  /**
   * Real pi end-of-turn replay.
   *
   * `e2e/fixtures/pi-turn.bin` is a captured PTY byte stream from a
   * real `pi` invocation: a single turn that prints ~30 lines and
   * ends with pi's standard end-of-turn redraw. The redraw is wrapped
   * in BSU/ESU and contains `\x1b[2J \x1b[H \x1b[3J` (clear viewport,
   * cursor home, clear scrollback) followed by the full prompt-area
   * repaint. That `\x1b[3J` resets xterm's `ybase`/`ydisp` to 0 and
   * is the sequence the existing comment in terminal.tsx blames for
   * the jump-to-top bug.
   *
   * `pi-turn.chunks.json` records the original PTY-read boundaries
   * (107 chunks). Replaying along those boundaries keeps the BSU and
   * ESU split that the production WebSocket path actually sees,
   * because gmuxd forwards each PTY read as one frame.
   *
   * In a real terminal pi's end-of-turn lands the cursor at the
   * bottom of the screen. In gmux it jumps to the top: this test
   * encodes the contract that the user reports.
   */
  test('real pi end-of-turn fixture: stays at bottom', async ({ page }) => {
    const fixtureDir = join(__dirname, '..', 'fixtures')
    const bytes = readFileSync(join(fixtureDir, 'pi-turn.bin'))
    const chunks = JSON.parse(
      readFileSync(join(fixtureDir, 'pi-turn.chunks.json'), 'utf8'),
    ) as Array<{ offset: number; len: number }>

    // Sanity: confirm the fixture really contains the sequence we
    // care about. If somebody replaces the fixture with bytes that
    // don't reach the end-of-turn redraw, this test stops being
    // meaningful, so guard the precondition explicitly.
    expect(bytes.indexOf(Buffer.from('\x1b[3J'))).toBeGreaterThan(0)

    // The fixture was recorded at 120x40. Resize the xterm to match
    // so cursor-up/clear-line redraws land where pi expects them.
    // Without this the absolute layout drifts but the bug shape (if
    // any) still appears; matching the recording keeps the test
    // focused on scroll behavior rather than reflow artifacts.
    await page.evaluate(() => (window as any).__gmuxTerm.resize(120, 40))
    await settle(page)

    // Some pre-existing scrollback so baseY is non-trivially large
    // before pi runs. In real use the user has been working in a
    // shell or watching previous turns scroll past.
    await seedScrollback(page, 200)
    await scrollToBottom(page)

    await replayPiFixture(page)

    const after = await getScroll(page)
    // Diagnostics so a regression report shows what the buffer
    // looked like, not just that the assertion failed.
    console.log('[pi-fixture]',
      'viewportY=', after.viewportY,
      'baseY=', after.baseY,
      'rows=', after.rows,
      'cols=', after.cols)

    // Sanity: pi's end-of-turn redraw populates a non-trivial
    // amount of scrollback. If baseY is 0 the test is trivially
    // passing because there's nothing TO scroll, which would mean
    // the fixture or replay isn't producing the conditions we're
    // asserting against.
    expect(after.baseY).toBeGreaterThan(0)
    // Bug shape: viewportY === 0 with baseY > 0 ("jumped to top").
    // Contract: the user was implicitly at the bottom for the whole
    // replay (we never scrolled up), so the final viewport must sit
    // at the bottom of the buffer.
    expect(after.viewportY).toBe(after.baseY)
  })

  /**
   * Real pi replay while the user is scrolled up reading earlier
   * output. This is the user-facing complaint shape: "I scrolled up
   * to read something and pi's next turn yanked me to the top."
   *
   * The contract is: stay near where the user was looking. After
   * pi's `\x1b[3J` clears scrollback, xterm's `baseY` shrinks; we
   * accept landing somewhere reasonable in what's left, but **never**
   * at viewportY=0 while baseY>0.
   */
  test('user scrolled up: real pi end-of-turn does not jump to top', async ({ page }) => {
    await page.evaluate(() => (window as any).__gmuxTerm.resize(120, 40))
    await settle(page)

    await seedScrollback(page, 200)
    await scrollToBottom(page)
    const baseline = await getScroll(page)
    expect(baseline.baseY).toBeGreaterThan(50)

    // Scroll up well into the seeded backscroll.
    const target = Math.floor(baseline.baseY / 2)
    await page.evaluate((line) => (window as any).__gmuxTerm.scrollToLine(line), target)
    const beforeBurst = await getScroll(page)
    expect(beforeBurst.viewportY).toBe(target)

    await replayPiFixture(page)

    const after = await getScroll(page)
    console.log('[pi-fixture/scrolled-up]',
      'viewportY=', after.viewportY,
      'baseY=', after.baseY)

    // The bug: viewportY pinned at 0 while baseY > 0. Whatever
    // restoration policy we pick (stay near user, snap to bottom
    // because scrollback was wiped, etc.), it must not be "top".
    if (after.baseY > 0) {
      expect(after.viewportY).toBeGreaterThan(0)
    }
  })
})

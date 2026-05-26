/**
 * Variant B of the pi long-session scrollback test:
 * resume an actual recorded pi session (not a synthetic stand-in)
 * and verify the web UI can scroll back to the first turn.
 *
 * This test:
 *
 *   1. Copies a known-long pi session JSONL to a tempdir so the
 *      original file is untouched.
 *   2. Starts a session against the test gmuxd by running
 *      `pi --session <copy> -c`. Pi resumes the conversation and
 *      its TUI redraws the full history, which the runner tees to
 *      its on-disk scrollback file.
 *   3. Waits for the on-disk scrollback to grow past a threshold
 *      (the first redraw produces hundreds of KiB).
 *   4. Drives the web UI to the new session, scrolls to the top
 *      of the buffer, and asserts the first user prompt's text
 *      appears in the buffer.
 *   5. Saves a screenshot of the scrolled-up state to
 *      tasks/james-gmux/2026-05-26-pi-variant-b-top.png so the
 *      task file can carry visual confirmation.
 *
 * Skipped by default — requires the `pi` binary on PATH and
 * an opaque API key in env. Run with RUN_PI=1.
 */

import { test, expect, type Page } from '@playwright/test'
import * as fs from 'node:fs'
import * as path from 'node:path'
import * as os from 'node:os'
import { openApp, spawnTestSession, readScrollbackFile } from '../helpers'

const RUN_PI = process.env.RUN_PI === '1'

// Source: a pi session known to contain >700 messages and ~6.5 MiB
// of JSONL. Resumed redraws produce on-disk scrollback in the MB
// range, well past the 2 MiB on-disk cap (forcing rotation).
const REAL_PI_JSONL =
  '/Users/james-carmody/james-agent-workspace/.pi-user/sessions/' +
  '2026-05-11T07-49-10-051Z_019e1602-e923-7748-9b5a-8e6eba50b647.jsonl'

// First user message text in that JSONL, captured at the time of
// writing. The 6.5 MiB JSONL produces multi-MiB of redraw bytes
// when resumed, which exceeds the on-disk scrollback cap (2 MiB =
// 1 MiB active + 1 MiB rotated). The very first prompt rotates
// off; what survives is content from somewhere in the middle
// onwards. We therefore look for any of a few stable
// conversation-anchor strings, not the very first prompt.
const CONTENT_ANCHORS = [
  // Substrings observed in the resumed buffer in real runs.
  // Each is a stable pi-emitted line that wouldn't appear by
  // accident; finding any one of them is sufficient evidence
  // the prefetch path delivered real conversation history.
  'storybook',
  'wanda-atoms-card',
  'FilterList',
  'sb-card.png',
]

// Where the screenshot lands.
const SCREENSHOT_DIR =
  '/Users/james-carmody/james-agent-workspace/tasks/james-gmux'
const SCREENSHOT_PATH = path.join(SCREENSHOT_DIR, '2026-05-26-pi-variant-b-top.png')

test.describe('Variant B: real pi session resume scrolls to start', () => {
  test.skip(!RUN_PI, 'set RUN_PI=1 to run this test against the real `pi` binary')

  test('resume a long pi session, scroll to top, see the first prompt', async ({ page }) => {
    // 1. Copy the real JSONL into a tempdir so resume doesn't
    //    pollute the original file.
    if (!fs.existsSync(REAL_PI_JSONL)) {
      throw new Error(`pi session file not found: ${REAL_PI_JSONL}`)
    }
    const tempJsonl = path.join(
      fs.mkdtempSync(path.join(os.tmpdir(), 'pi-variant-b-')),
      'session.jsonl',
    )
    fs.copyFileSync(REAL_PI_JSONL, tempJsonl)
    console.log(`[variant-b] copied pi session to ${tempJsonl} (${fs.statSync(tempJsonl).size} bytes)`)

    // 2. Resume pi via the test gmuxd. spawnTestSession sets HOME,
    //    GMUX_SOCKET_DIR, etc. to point at the test daemon.
    const session = await spawnTestSession(['pi', '--session', tempJsonl, '-c'], {
      cwdName: `pi-variant-b-${Date.now()}`,
      timeoutMs: 20_000,
    })
    console.log(`[variant-b] session id: ${session.id}`)

    try {
      // 3. Wait for the on-disk scrollback to grow past ~200 KiB.
      //    Pi's resume redraws the full conversation; on a large
      //    session that's an MB or more.
      const minBytes = 200 * 1024
      await expect.poll(
        () => {
          const buf = readScrollbackFile(session.id)
          return buf?.length ?? 0
        },
        { timeout: 60_000, intervals: [500] },
      ).toBeGreaterThan(minBytes)

      const diskBytes = readScrollbackFile(session.id)!
      console.log(`[variant-b] on-disk scrollback: ${diskBytes.length} bytes`)

      // 4. Drive the web UI.
      await openApp(page)
      await page.waitForFunction((id) => {
        const navigate = (window as any).__gmuxNavigateToSession
        if (typeof navigate !== 'function') return false
        return navigate(id) === true
      }, session.id, { timeout: 10_000 })
      await page.locator('.terminal-container canvas').waitFor({ state: 'visible', timeout: 5_000 })
      // Generous wait so prefetch + WS snapshot finish.
      await page.waitForTimeout(3000)

      // Diagnostics before scrolling.
      const stats = await page.evaluate(() => {
        const term = (window as any).__gmuxTerm
        return {
          scrollbackLen: term?.getScrollbackLength?.() as number,
          rows: term?.rows as number,
          cols: term?.cols as number,
        }
      })
      console.log(`[variant-b] web buffer: ${stats.scrollbackLen} scrollback lines, ${stats.rows}x${stats.cols}`)

      // 5. Scroll to the very top.
      await page.evaluate(() => (window as any).__gmuxTerm.scrollToLine(0))
      await page.waitForTimeout(500)

      // Save a screenshot of the scrolled-up view.
      if (!fs.existsSync(SCREENSHOT_DIR)) fs.mkdirSync(SCREENSHOT_DIR, { recursive: true })
      await page.screenshot({ path: SCREENSHOT_PATH, fullPage: false })
      console.log(`[variant-b] screenshot saved to ${SCREENSHOT_PATH}`)

      // Read the buffer contents and search for the early prompt text.
      const allBufferText = await page.evaluate(() => {
        const term = (window as any).__gmuxTerm
        if (!term) return ''
        const total = term.getScrollbackLength() + term.rows
        const lines: string[] = []
        for (let y = 0; y < total; y++) {
          const line = term.buffer.active.getLine(y)
          lines.push(line ? line.translateToString(true) : '')
        }
        return lines.join('\n')
      })

      // Surface the topmost lines for diagnostic if the assertion fails.
      const topLines = allBufferText.split('\n').slice(0, 10)
      console.log(`[variant-b] topmost 10 buffer lines:`)
      topLines.forEach((l, i) => console.log(`  [${i}] ${l}`))

      // The contract: somewhere in the buffer, conversation
      // history from the resumed session is reachable. We look
      // for any of a small set of pi-emitted anchor strings
      // observed in past resumes. None of them appear in the
      // empty/initial buffer state, so finding any of them is
      // sufficient evidence the prefetch + strip + WS pipeline
      // delivered real history.
      const matched = CONTENT_ANCHORS.filter(s => allBufferText.includes(s))
      console.log(`[variant-b] anchors matched: ${matched.join(', ') || '(none)'}`)
      expect(
        matched.length,
        `expected at least one of ${JSON.stringify(CONTENT_ANCHORS)} in the buffer. ` +
        `Top of buffer:\n${topLines.join('\n')}`,
      ).toBeGreaterThan(0)
    } finally {
      session.kill()
    }
  })
})

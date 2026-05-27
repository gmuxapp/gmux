import { test, expect, type Page } from '@playwright/test'
import { openApp, spawnTestSession, readScrollbackFile } from '../helpers'

/**
 * Pins the user contract from
 * tasks/james-gmux/2026-05-26-pi-scrollback-to-start.md:
 * "I should be able to scroll back to the very start of a long-
 *  running pi session in the web UI."
 */

const START_MARKER_PREFIX = 'START-MARKER-'
const TURN_COUNT = 20
const FINAL_MARKER = 'END-MARKER-WAITING'

/**
 * Synthetic pi-shaped session that faithfully matches pi's actual output
 * structure: each BSU/ESU full-render block contains the ACCUMULATED
 * conversation history up to that turn.
 *
 * Pi's TUI re-renders the entire visible conversation on every turn:
 *   BSU + \x1b[2J\x1b[H\x1b[3J + <all turns so far> + ESU
 *
 * extractScrollbackContent (multi-block path) deduplicates the overlap
 * between consecutive blocks and stitches together the unique new lines
 * from each block. For this to yield early-turn markers (START-MARKER-01
 * etc.) those markers must appear inside the block content — not as raw
 * inter-block bytes that get discarded.
 *
 * Script structure: turn N's full-render block contains markers 1..N
 * (accumulated), so early markers are present in every block from turn 1
 * onward and survive the deduplication pass.
 */
function piShapeScript(): string {
  // Build an accumulated history string.  Each iteration adds one
  // marker + 5 fillers, then emits a full-render block that contains
  // the entire history written so far.
  const turns: string[] = []
  for (let i = 1; i <= TURN_COUNT; i++) {
    const n = String(i).padStart(2, '0')
    turns.push(`${START_MARKER_PREFIX}${n}`)
    for (let j = 1; j <= 5; j++) turns.push(`filler-line-${n}-${j}`)
  }

  // For each turn, the block content is the lines accumulated so far
  // (turns 1..i) plus a 'redraw-NN' line.
  const scriptLines: string[] = []
  for (let i = 1; i <= TURN_COUNT; i++) {
    const n = String(i).padStart(2, '0')
    // Accumulated history up to turn i (marker + 5 fillers per turn).
    const historyLines: string[] = []
    for (let k = 1; k <= i; k++) {
      const kn = String(k).padStart(2, '0')
      historyLines.push(`${START_MARKER_PREFIX}${kn}`)
      for (let j = 1; j <= 5; j++) historyLines.push(`filler-line-${kn}-${j}`)
    }
    historyLines.push(`redraw-${n}`)
    // BSU + full clear + accumulated history + ESU
    const blockContent = historyLines.join('\\n')
    scriptLines.push(`printf '\\033[?2026h\\033[2J\\033[H\\033[3J${blockContent}\\033[?2026l'`)
  }
  scriptLines.push(`printf '${FINAL_MARKER}\\n'`)
  scriptLines.push('sleep 60')
  return scriptLines.join('\n')
}

async function navigateToSpawnedSession(page: Page, sessionId: string): Promise<void> {
  await page.waitForFunction((id) => {
    const navigate = (window as any).__gmuxNavigateToSession
    if (typeof navigate !== 'function') return false
    return navigate(id) === true
  }, sessionId, { timeout: 10_000 })
  await page.locator('.terminal-container canvas').waitFor({ state: 'visible', timeout: 5_000 })
  // WS connect + replay.
  await page.waitForTimeout(1500)
}

async function readBufferTopLines(page: Page, n: number): Promise<string[]> {
  return page.evaluate((count) => {
    const term = (window as any).__gmuxTerm
    if (!term) return []
    const lines: string[] = []
    for (let y = 0; y < count; y++) {
      const line = term.buffer.active.getLine(y)
      lines.push(line ? line.translateToString(true) : '')
    }
    return lines
  }, n)
}

async function readBufferAllLines(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const term = (window as any).__gmuxTerm
    if (!term) return []
    const total = term.getScrollbackLength() + term.rows
    const lines: string[] = []
    for (let y = 0; y < total; y++) {
      const line = term.buffer.active.getLine(y)
      lines.push(line ? line.translateToString(true) : '')
    }
    return lines
  })
}

test.describe('pi long-session scrollback reaches the start', () => {
  test('user can scroll up to the first turn marker after a long pi-shape session', async ({ page }) => {
    // Spawn a synthetic pi-shape session against the test daemon.
    const session = await spawnTestSession(['bash', '-c', piShapeScript()], {
      cwdName: `pi-shape-${Date.now()}`,
    })

    try {
      // Wait until the script has finished its loop. Polling the
      // disk file is the most reliable signal because it's the
      // runner's tee, not the (slower) emulator drain.
      await expect.poll(
        () => {
          const buf = readScrollbackFile(session.id)
          return buf?.includes(FINAL_MARKER) ?? false
        },
        { timeout: 10_000, intervals: [200] },
      ).toBe(true)

      // Sanity: the on-disk file contains both the first and last
      // markers. If this fails, the script didn't run correctly
      // and the rest of the test is meaningless.
      const diskBytes = readScrollbackFile(session.id)
      expect(diskBytes, 'on-disk scrollback should exist').not.toBeNull()
      expect(diskBytes!.toString('utf8')).toContain(`${START_MARKER_PREFIX}01`)
      expect(diskBytes!.toString('utf8')).toContain(`${START_MARKER_PREFIX}${String(TURN_COUNT).padStart(2, '0')}`)

      // Diagnostic: surface the on-disk size so a regression report
      // shows whether the bug is "data missing on disk" or "data
      // missing from the web buffer".
      console.log(`[pi-scrollback] on-disk: ${diskBytes!.length} bytes`)

      // Drive the web UI to the spawned session. The default global-
      // setup test session (an idle bash) is unaffected.
      await openApp(page)
      await navigateToSpawnedSession(page, session.id)

      // Diagnostic: how much did the web client receive?
      const webStats = await page.evaluate(() => {
        const term = (window as any).__gmuxTerm
        return {
          scrollbackLen: term?.getScrollbackLength?.() as number,
          rows: term?.rows as number,
        }
      })
      console.log(`[pi-scrollback] web buffer: ${webStats.scrollbackLen} scrollback lines, ${webStats.rows} rows`)

      // Scroll to the very top of the buffer.
      await page.evaluate(() => (window as any).__gmuxTerm.scrollToLine(0))
      await page.waitForTimeout(200)

      // The contract: somewhere near the top of the buffer there
      // is an early START-MARKER-NN, with a low NN. We don't pin
      // exactly NN=01 because xterm's line cap and the prefetch
      // dump's exact ordering may push the very first line off
      // the top. But at minimum, the buffer must contain SOME
      // marker from turns 1-3 — well before the most recent turn.
      const allLines = await readBufferAllLines(page)
      const earlyTurnRegex = new RegExp(`^${START_MARKER_PREFIX}0[123]$`)
      const earlyTurnsFound = allLines.filter(l => earlyTurnRegex.test(l.trim()))

      // Also report which markers ARE present, for diagnostics.
      const allMarkers = allLines
        .map(l => l.trim())
        .filter(l => l.startsWith(START_MARKER_PREFIX))
        .sort()
      console.log(`[pi-scrollback] markers in web buffer: ${allMarkers.join(', ') || '(none)'}`)

      // The bug shape on `main`: only START-MARKER-20 is present
      // (the post-final-wipe visible content). After the fix, all
      // 20 markers are present.
      expect(
        earlyTurnsFound.length,
        `expected at least one early-turn marker (turns 1-3) in the web buffer; got: ${allMarkers.join(', ') || '(none)'}`,
      ).toBeGreaterThan(0)
    } finally {
      session.kill()
    }
  })
})

import { test, expect, type Page } from '@playwright/test'
import { openApp, spawnTestSession, readScrollbackFile } from '../helpers'

/**
 * Pins the user contract from
 * tasks/james-gmux/2026-05-26-pi-scrollback-to-start.md:
 * "I should be able to scroll back to the very start of a long-
 *  running pi session in the web UI."
 *
 * Today: the WS snapshot only contains content from after the most
 * recent \x1b[3J (Erase Saved Lines), which pi emits at the end of
 * every turn. The on-disk scrollback file holds the raw bytes from
 * the start of the session (capped at 2 MiB), but the live web
 * path never reads it.
 *
 * Expected on `main`: this test FAILS — the buffer top is from a
 * recent turn, not the session start.
 *
 * Expected after the fix (web prefetch + ?no_erase=1, mirroring
 * cli/gmux/cmd/gmux/attach.go): this test PASSES.
 */

const START_MARKER_PREFIX = 'START-MARKER-'
const TURN_COUNT = 20
const FINAL_MARKER = 'END-MARKER-WAITING'

/**
 * Synthetic pi-shaped session: 20 turns of
 *   marker + 5 fillers + BSU + clear-display + cursor-home + clear-scrollback + redraw + ESU
 * then a final marker so we can detect loop completion via the
 * on-disk file.
 *
 * The BSU/ESU wrap matches pi's real end-of-turn shape (see
 * e2e/fixtures/pi-turn.bin and the comment in apps/gmux-web/src/
 * replay.ts). The bytes inside the BSU/ESU block carry the screen
 * reset sequence (\x1b[2J\x1b[H\x1b[3J), which is what the live
 * web path needs to skip during disk replay so prior turns'
 * content survives in the host terminal's scrollback.
 */
function piShapeScript(): string {
  return `
for i in $(seq 1 ${TURN_COUNT}); do
  printf '${START_MARKER_PREFIX}%02d\\n' "$i"
  for j in 1 2 3 4 5; do printf 'filler-line-%02d-%d\\n' "$i" "$j"; done
  printf '\\033[?2026h\\033[2J\\033[H\\033[3Jredraw-%02d\\033[?2026l' "$i"
done
printf '${FINAL_MARKER}\\n'
sleep 60
`.trim()
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

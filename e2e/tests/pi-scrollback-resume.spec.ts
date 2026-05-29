/**
 * Real pi session resume: load a fixture session, watch it render in the
 * web UI, and verify that scrolling back reaches the early conversation.
 *
 * What this tests:
 *   - pi can resume a real session JSONL and render its history into the PTY
 *   - gmuxd captures that output and delivers it to the browser via WS
 *   - the browser xterm accumulates scrollback from the live WS replay
 *   - scrolling to line 0 surfaces content from earlier in the session
 *   - the whole flow is captured as a webm artifact
 *
 * Fixture: e2e/fixtures/pi-session.jsonl
 *   48-message session (~68 KB). First message: "clean up the workspace,
 *   grouping changes into commits and linking to tasks".
 *
 * Auto-skips when:
 *   - the pi binary is not on PATH
 *
 * No LLM API calls are made: pi is started without -c so it renders the
 * saved history and then waits for input.
 */

import { test, expect, type Page } from '@playwright/test'
import { execSync } from 'node:child_process'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'
import { openApp, spawnTestSession, readScrollbackFile } from '../helpers'

// ── Paths ─────────────────────────────────────────────────────────────────

const FIXTURE = path.resolve(__dirname, '../fixtures/pi-session.jsonl')
const OUT_DIR  = path.resolve(__dirname, '../../tasks/james-gmux')
const OUT_WEBM = path.join(OUT_DIR, 'pi-scrollback-resume.webm')

// ── Auto-skip when pi is not available ────────────────────────────────────

function findPiBinary(): string | null {
  try {
    return execSync('which pi', { encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'] }).trim()
  } catch {
    return null
  }
}

const PI_BINARY = findPiBinary()

// ── Video config ──────────────────────────────────────────────────────────

// Record the full test as a webm. The recording starts when the page opens
// (START RECORDING) and is saved at the end of the test (FINISH RECORDING).
test.use({
  viewport: { width: 1280, height: 800 },
  video: { mode: 'on', size: { width: 1280, height: 800 } },
})

// ── Helpers ────────────────────────────────────────────────────────────────

/** Navigate the web UI to a session and wait for the terminal canvas. */
async function navigateToSession(page: Page, sessionId: string): Promise<void> {
  await page.waitForFunction((id) => {
    const navigate = (window as any).__gmuxNavigateToSession
    return typeof navigate === 'function' && navigate(id) === true
  }, sessionId, { timeout: 10_000 })
  await page.locator('.terminal-container canvas').waitFor({ state: 'visible', timeout: 5_000 })
  // Give the WS connection time to deliver the initial render.
  await page.waitForTimeout(1500)
}

/** Read all lines from the xterm buffer (scrollback + visible rows). */
async function readAllBufferLines(page: Page): Promise<string[]> {
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

// ── Test ───────────────────────────────────────────────────────────────────

test.describe('pi session resume: scrollback is reachable in the web UI', () => {
  test.skip(PI_BINARY === null, 'pi binary not on PATH')

  test('resume a real pi session and scroll to top', async ({ page }) => {
    test.setTimeout(120_000)

    // Build a merged HOME so pi finds its .pi-user config (models, settings)
    // AND the .aws SSO cache for credential resolution, without needing -c.
    const mergedHome = fs.mkdtempSync(path.join(os.tmpdir(), 'pi-resume-home-'))
    const piUserRoot = (() => {
      // Walk up from __dirname to find a directory that contains .pi-user/
      let dir = path.resolve(__dirname)
      for (let i = 0; i < 10; i++) {
        if (fs.existsSync(path.join(dir, '.pi-user'))) return dir
        const parent = path.dirname(dir)
        if (parent === dir) break
        dir = parent
      }
      return null
    })()
    if (piUserRoot) {
      try { fs.symlinkSync(path.join(piUserRoot, '.pi-user'), path.join(mergedHome, '.pi-user')) } catch { /* ok */ }
    }
    const realAwsDir = path.join(os.homedir(), '.aws')
    if (fs.existsSync(realAwsDir)) {
      try { fs.symlinkSync(realAwsDir, path.join(mergedHome, '.aws')) } catch { /* ok */ }
    }

    // Copy the fixture so pi can read it. pi --session accepts an absolute path.
    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), 'pi-resume-'))
    const sessionFile = path.join(tempDir, 'session.jsonl')
    fs.copyFileSync(FIXTURE, sessionFile)

    // Start pi with --session pointing at the fixture. No -c: pi renders the
    // saved conversation history and waits — no LLM API calls are made.
    const session = await spawnTestSession(
      ['pi', '--session', sessionFile],
      {
        cwdName: `pi-resume-${Date.now()}`,
        timeoutMs: 15_000,
        extraEnv: {
          HOME: mergedHome,
          ...Object.fromEntries(
            Object.entries(process.env).filter(([k]) => k.startsWith('AWS_')) as [string, string][]
          ),
        },
      },
    )
    console.log(`[pi-resume] session id: ${session.id}`)

    try {
      // ── START RECORDING ───────────────────────────────────────────────────
      // Open the app. Video recording started when the page was created;
      // this is when meaningful UI content begins.
      console.log('[pi-resume] opening app')
      await openApp(page)
      console.log('[pi-resume] navigating to session')
      await navigateToSession(page, session.id)
      console.log('[pi-resume] navigation done, waiting for scrollback')

      // Wait for the WS to deliver pi's initial history render into xterm.
      await expect.poll(
        () => page.evaluate(() => (window as any).__gmuxTerm?.getScrollbackLength() ?? 0),
        { timeout: 30_000, intervals: [300], message: 'xterm scrollback did not grow' },
      ).toBeGreaterThan(20)

      const stats = await page.evaluate(() => {
        const term = (window as any).__gmuxTerm
        return { scrollbackLen: term?.getScrollbackLength?.() as number, rows: term?.rows as number }
      })
      console.log(`[pi-resume] web buffer: ${stats.scrollbackLen} scrollback lines, ${stats.rows} rows`)
      const diskBytes = readScrollbackFile(session.id)?.length ?? 0
      console.log(`[pi-resume] on-disk scrollback: ${diskBytes} bytes`)

      // Hold on the live view so the recording shows the current state first.
      await page.waitForTimeout(2000)

      // Scroll smoothly to the top so the recording shows the history.
      const STEPS = 60
      for (let i = 0; i <= STEPS; i++) {
        const line = Math.round(stats.scrollbackLen * (1 - i / STEPS))
        await page.evaluate((l) => (window as any).__gmuxTerm?.scrollToLine(l), line)
        await page.waitForTimeout(30)
      }

      // Hold at the top.
      await page.waitForTimeout(2000)

      const allLines = await readAllBufferLines(page)
      const topLines = allLines.slice(0, 20)
      console.log(`[pi-resume] top 10 buffer lines:`)
      topLines.slice(0, 10).forEach((l, i) => console.log(`  [${i}] ${l.trim()}`))

      // The contract: scrolling to the top of the buffer surfaces real content
      // from the pi session's history render — not an empty prompt, not garbage.
      // At least 5 of the first 20 lines must have meaningful text (> 4 chars).
      const nonEmpty = topLines.filter(l => l.trim().length > 4).length
      console.log(`[pi-resume] non-empty lines in top 20: ${nonEmpty}`)
      expect(
        nonEmpty,
        `expected readable content near top of scrollback (got ${nonEmpty}/20 non-empty).\n` +
        `Top lines:\n${topLines.map(l => l.trim()).join('\n')}`,
      ).toBeGreaterThanOrEqual(5)

      // Scroll back to live state so the recording ends at the current view.
      for (let i = 0; i <= STEPS; i++) {
        const line = Math.round(stats.scrollbackLen * (i / STEPS))
        await page.evaluate((l) => (window as any).__gmuxTerm?.scrollToLine(l), line)
        await page.waitForTimeout(30)
      }
      await page.waitForTimeout(1000)

    } finally {
      // Kill pi before saving the video so the WS closes cleanly.
      session.kill()
      try { fs.rmSync(tempDir, { recursive: true }) } catch { /* ok */ }
      try { fs.rmSync(mergedHome, { recursive: true }) } catch { /* ok */ }
    }  // end try/finally

    // ── FINISH RECORDING ──────────────────────────────────────────────────
    // Grab the Video reference before closing the page (it becomes unavailable
    // after close in some Playwright versions).
    const video = page.video()
    // Closing the page stops the recording and lets Playwright finalize the
    // video file. Without an explicit close here, Playwright's own page fixture
    // teardown does the close — but that happens AFTER the timeout budget is
    // spent waiting for the video encoder, causing a spurious timeout failure.
    await page.close()
    fs.mkdirSync(OUT_DIR, { recursive: true })
    if (video) {
      await video.saveAs(OUT_WEBM)
      console.log(`[pi-resume] webm saved: ${OUT_WEBM}`)
    }
  })
})

/**
 * Record a GIF of loading a long synthetic pi-shaped session.
 *
 *   npm run test:e2e -- --grep "scrollback-perf-demo"
 *
 * Output: tasks/gmux/scrollback-perf-demo.gif
 */

import { test } from '@playwright/test'
import * as fs from 'node:fs'
import * as path from 'node:path'
import { execSync } from 'node:child_process'
import { openApp, spawnTestSession, readScrollbackFile } from '../helpers'

const OUT_DIR  = path.resolve(__dirname, '../../tasks/gmux')
const OUT_WEBM = path.join(OUT_DIR, 'scrollback-perf-demo.webm')
const OUT_GIF  = path.join(OUT_DIR, 'scrollback-perf-demo.gif')

const TURNS = 80   // enough to fill several screenfuls of history

test.use({
  viewport: { width: 1280, height: 800 },
  video: { mode: 'on', size: { width: 1280, height: 800 } },
})

/** Synthetic pi-shaped session: TURNS full-render blocks with accumulated history. */
function longSessionScript(): string {
  const lines: string[] = []
  for (let i = 1; i <= TURNS; i++) {
    const n = String(i).padStart(3, '0')
    // Accumulated history up to turn i — each turn has a marker + 4 content lines.
    const historyLines: string[] = []
    for (let k = 1; k <= i; k++) {
      const kn = String(k).padStart(3, '0')
      historyLines.push(`┌─ Turn ${kn} ─────────────────────────────────────────────┐`)
      historyLines.push(`│  User: do task ${kn}                                       │`)
      historyLines.push(`│  Agent: completed task ${kn} successfully                  │`)
      historyLines.push(`└────────────────────────────────────────────────────────────┘`)
    }
    historyLines.push(`[status: turn ${n} / ${TURNS}]`)
    // Emit BSU + clear + full history + ESU
    const escaped = historyLines.join('\\n').replace(/'/g, "'\\''")
    lines.push(`printf '\\033[?2026h\\033[2J\\033[H\\033[3J${escaped}\\033[?2026l'`)
  }
  lines.push(`printf 'DEMO-COMPLETE\\n'`)
  lines.push('sleep 120')
  return lines.join('\n')
}

test.describe('scrollback perf demo', () => {
  test('record fast load of long session with full history', async ({ page }) => {
    // ── 1. Spawn a second "idle" session to switch from ───────────────────
    const idleSession = await spawnTestSession(
      ['bash', '-c', 'echo idle-ready; sleep 120'],
      { cwdName: `idle-${Date.now()}` },
    )

    // ── 2. Write script to a temp file (avoids E2BIG for long scripts) ───
    const scriptFile = path.join(OUT_DIR, 'scrollback-demo-script.sh')
    fs.mkdirSync(OUT_DIR, { recursive: true })
    fs.writeFileSync(scriptFile, longSessionScript(), { mode: 0o755 })

    // ── 3. Spawn the long synthetic session ──────────────────────────────
    const longSession = await spawnTestSession(
      ['bash', scriptFile],
      { cwdName: `long-session-${Date.now()}` },
    )
    console.log(`[demo] sessions: idle=${idleSession.id} long=${longSession.id}`)

    try {
      // ── 3. Wait until all turns are on disk ──────────────────────────
      await page.waitForFunction(
        () => true, // just need the test to proceed — we poll below
        {},
        { timeout: 1000 },
      ).catch(() => {})

      const t0 = Date.now()
      while (Date.now() - t0 < 30_000) {
        const buf = readScrollbackFile(longSession.id)
        if (buf?.includes('DEMO-COMPLETE')) break
        await new Promise(r => setTimeout(r, 300))
      }
      const diskBytes = readScrollbackFile(longSession.id)!.length
      console.log(`[demo] on-disk scrollback: ${(diskBytes / 1024).toFixed(0)} KB`)

      // ── 4. Open gmux, start on the idle session ───────────────────────
      await openApp(page)
      await page.waitForFunction((id) => {
        const nav = (window as any).__gmuxNavigateToSession
        return typeof nav === 'function' && nav(id) === true
      }, idleSession.id, { timeout: 10_000 })
      await page.locator('.terminal-container canvas').waitFor({ state: 'visible' })
      await page.waitForTimeout(1500)

      // ── 5. Measure + switch to long session (the key moment) ──────────
      const switchStart = Date.now()
      await page.evaluate((id) => (window as any).__gmuxNavigateToSession?.(id), longSession.id)

      // Wait for loading overlay to appear (React processed the switch),
      // then wait for it to disappear (prefetch + WS snapshot written).
      await page.locator('.terminal-loading').waitFor({ state: 'visible',  timeout: 5_000 })
      await page.locator('.terminal-loading').waitFor({ state: 'hidden', timeout: 15_000 })
      const loadMs = Date.now() - switchStart
      console.log(`[demo] session loaded in ${loadMs}ms`)

      // Wait for the terminal write queue to fully drain.
      await page.waitForFunction(
        () => ((window as any).__gmuxTerm?.getScrollbackLength() ?? 0) > 10,
        { timeout: 5_000 },
      ).catch(() => {})

      const scrollbackLines: number = await page.evaluate(
        () => (window as any).__gmuxTerm?.getScrollbackLength() ?? 0,
      )
      console.log(`[demo] ${scrollbackLines} scrollback lines in browser buffer`)

      // ── 6. Hold at live view ──────────────────────────────────────────
      await page.waitForTimeout(2000)

      // ── 7. Scroll smoothly to the top ────────────────────────────────
      const SCROLL_STEPS = 80
      for (let i = 0; i <= SCROLL_STEPS; i++) {
        const line = Math.round(scrollbackLines * (1 - i / SCROLL_STEPS))
        await page.evaluate((l) => (window as any).__gmuxTerm?.scrollToLine(l), line)
        await page.waitForTimeout(30)
      }

      // ── 8. Hold at top — show Turn 001 is present ─────────────────────
      await page.waitForTimeout(2500)

      // ── 9. Scroll back to the bottom ─────────────────────────────────
      for (let i = 0; i <= SCROLL_STEPS; i++) {
        const line = Math.round(scrollbackLines * (i / SCROLL_STEPS))
        await page.evaluate((l) => (window as any).__gmuxTerm?.scrollToLine(l), line)
        await page.waitForTimeout(30)
      }
      await page.waitForTimeout(1000)

      // ── 10. Switch away then back — show cache makes it instant ────────
      await page.evaluate((id) => (window as any).__gmuxNavigateToSession?.(id), idleSession.id)
      await page.waitForTimeout(1500)

      const switchBackStart = Date.now()
      await page.evaluate((id) => (window as any).__gmuxNavigateToSession?.(id), longSession.id)
      await page.locator('.terminal-loading').waitFor({ state: 'visible', timeout: 3_000 })
      await page.locator('.terminal-loading').waitFor({ state: 'hidden',  timeout: 5_000 })
      const cachedLoadMs = Date.now() - switchBackStart
      console.log(`[demo] cached reload in ${cachedLoadMs}ms`)

      await page.waitForTimeout(2000)

    } finally {
      idleSession.kill()
      longSession.kill()
    }

    // ── 11. Save video + convert to GIF ──────────────────────────────────
    // saveAs() waits for the recording to be fully finalized before copying.
    fs.mkdirSync(OUT_DIR, { recursive: true })
    await page.video()!.saveAs(OUT_WEBM)
    console.log(`[demo] webm saved: ${OUT_WEBM}`)

    const filterComplex = [
      '[0:v] fps=10,scale=800:-1:flags=lanczos,split [a][b]',
      '[a] palettegen=max_colors=64 [p]',
      '[b][p] paletteuse=dither=bayer',
    ].join(';')
    execSync(
      `ffmpeg -y -i "${OUT_WEBM}" -filter_complex "${filterComplex}" "${OUT_GIF}"`,
      { stdio: 'inherit' },
    )
    const gifKb = (fs.statSync(OUT_GIF).size / 1024).toFixed(0)
    console.log(`[demo] gif: ${OUT_GIF} (${gifKb} KB)`)
  })
})

/**
 * Record a webm of the scrollback demo, then convert to GIF with ffmpeg.
 *
 * Run with:
 *   RUN_PI=1 npx playwright test --config=e2e/playwright.config.ts scrollback-demo
 */

import { test } from '@playwright/test'
import * as fs from 'node:fs'
import * as path from 'node:path'
import * as os from 'node:os'
import { execSync } from 'node:child_process'
import { openApp, spawnTestSession, readScrollbackFile } from '../helpers'

const RUN_PI = process.env.RUN_PI === '1'

const REAL_PI_JSONL =
  '/Users/james-carmody/james-agent-workspace/.pi-user/sessions/' +
  '2026-05-11T07-49-10-051Z_019e1602-e923-7748-9b5a-8e6eba50b647.jsonl'

const OUT_DIR  = '/Users/james-carmody/james-agent-workspace/tasks/james-gmux'
const OUT_WEBM = path.join(OUT_DIR, '2026-05-26-scrollback-demo.webm')
const OUT_GIF  = path.join(OUT_DIR, '2026-05-26-scrollback-demo.gif')

test.describe('scrollback demo recording', () => {
  test.skip(!RUN_PI, 'set RUN_PI=1 to run')
  test.use({
    viewport: { width: 1280, height: 800 },
    // Playwright writes video alongside the page; we move it after.
    video: { mode: 'on', size: { width: 1280, height: 800 } },
  })

  test('record resume + scroll-to-top demo', async ({ page }) => {
    // ── 1. Copy JSONL ──────────────────────────────────────────────────────
    const tempJsonl = path.join(
      fs.mkdtempSync(path.join(os.tmpdir(), 'pi-demo-')),
      'session.jsonl',
    )
    fs.copyFileSync(REAL_PI_JSONL, tempJsonl)

    // ── 2. Spawn pi session ────────────────────────────────────────────────
    const session = await spawnTestSession(['pi', '--session', tempJsonl, '-c'], {
      cwdName: `pi-demo-${Date.now()}`,
      timeoutMs: 20_000,
    })
    console.log(`[demo] session: ${session.id}`)

    try {
      // ── 3. Wait for scrollback ─────────────────────────────────────────
      const minBytes = 200 * 1024
      const t0 = Date.now()
      while (Date.now() - t0 < 90_000) {
        const n = readScrollbackFile(session.id)?.length ?? 0
        if (n > minBytes) break
        await page.waitForTimeout(500)
      }
      console.log(`[demo] scrollback: ${(readScrollbackFile(session.id)!.length / 1024).toFixed(0)} KB`)

      // ── 4. Open app and navigate to session ───────────────────────────
      await openApp(page)
      await page.waitForFunction((id) => {
        const nav = (window as any).__gmuxNavigateToSession
        return typeof nav === 'function' && nav(id) === true
      }, session.id, { timeout: 10_000 })
      await page.locator('.terminal-container canvas').waitFor({ state: 'visible', timeout: 8_000 })

      // ── 5. Let the prefetch + WS snapshot settle (show live state) ─────
      await page.waitForTimeout(4000)

      // ── 6. Scroll slowly to the top ────────────────────────────────────
      const scrollbackLines: number = await page.evaluate(() => {
        return (window as any).__gmuxTerm?.getScrollbackLength() ?? 0
      })
      console.log(`[demo] ${scrollbackLines} scrollback lines`)

      const steps = 60
      for (let i = 0; i <= steps; i++) {
        const line = Math.round(scrollbackLines * (1 - i / steps))
        await page.evaluate((l) => (window as any).__gmuxTerm?.scrollToLine(l), line)
        await page.waitForTimeout(40)
      }

      // ── 7. Hold at top ─────────────────────────────────────────────────
      await page.waitForTimeout(3000)

      // ── 8. Scroll back down to live state ─────────────────────────────
      for (let i = 0; i <= steps; i++) {
        const line = Math.round(scrollbackLines * (i / steps))
        await page.evaluate((l) => (window as any).__gmuxTerm?.scrollToLine(l), line)
        await page.waitForTimeout(40)
      }
      await page.waitForTimeout(2000)

    } finally {
      session.kill()
    }

    // ── 9. Save video ──────────────────────────────────────────────────────
    const videoPath = await page.video()!.path()
    fs.mkdirSync(OUT_DIR, { recursive: true })
    fs.copyFileSync(videoPath, OUT_WEBM)
    console.log(`[demo] webm: ${OUT_WEBM}`)

    // ── 10. Convert to GIF with ffmpeg ────────────────────────────────────
    const ffmpeg = process.env.FFMPEG_PATH || 'ffmpeg'
    const filter = [
      'fps=20',
      'scale=1280:-1:flags=lanczos',
      'split[s0][s1]',
      '[s0]palettegen=max_colors=128[p]',
      '[s1][p]paletteuse=dither=bayer',
    ].join(',')
    execSync(
      `${ffmpeg} -y -i "${OUT_WEBM}" -vf "${filter}" "${OUT_GIF}"`,
      { stdio: 'inherit' },
    )
    console.log(`[demo] gif: ${OUT_GIF} (${(fs.statSync(OUT_GIF).size / 1024).toFixed(0)} KB)`)
  })
})

#!/usr/bin/env node
/**
 * Capture the landing-page hero screenshots from gmux-web's mock mode.
 *
 * Boots a throwaway vite dev server for apps/gmux-web, opens it in
 * headless Chromium via Playwright with `?mock=clean` (the staged,
 * healthy-world mock fixture — see src/mock-data/), and writes:
 *
 *   apps/website/src/assets/hero-desktop.png  — home dashboard, desktop
 *   apps/website/src/assets/hero-mobile.png   — session terminal + key row, phone
 *
 * Usage:
 *   node apps/website/scripts/capture-hero.mjs
 *
 * Env:
 *   BASE_URL   reuse a running gmux-web dev server instead of spawning one
 *   CHROMIUM   path to a chromium/chrome binary (default: Playwright's own,
 *              falling back to /usr/bin/chromium)
 *
 * Regenerate these whenever the web UI changes visibly; the mock data
 * (apps/gmux-web/src/mock-data/) is the single source of staged content.
 */

import { spawn } from 'node:child_process'
import { existsSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'

const here = dirname(fileURLToPath(import.meta.url))
const repoRoot = join(here, '..', '..', '..')
const webDir = join(repoRoot, 'apps', 'gmux-web')
const outDir = join(here, '..', 'src', 'assets')

const PORT = 5297
const external = process.env.BASE_URL
const base = external ?? `http://localhost:${PORT}`

/** Wait until the dev server responds. */
async function waitFor(url, tries = 60) {
  for (let i = 0; i < tries; i++) {
    try {
      const res = await fetch(url)
      if (res.ok) return
    } catch { /* not up yet */ }
    await new Promise(r => setTimeout(r, 500))
  }
  throw new Error(`dev server at ${url} did not come up`)
}

let server = null
if (!external) {
  server = spawn('npx', ['vite', '--port', String(PORT), '--strictPort'], {
    cwd: webDir,
    stdio: 'ignore',
    detached: false,
  })
}

try {
  await waitFor(base + '/')

  const executablePath = process.env.CHROMIUM
    ?? (existsSync('/usr/bin/chromium') ? '/usr/bin/chromium' : undefined)
  // WebGL canvases composite as black in headless screenshots; xterm
  // falls back to its DOM renderer when WebGL is unavailable, which
  // captures correctly and is pixel-identical for static content.
  const browser = await chromium.launch({
    executablePath,
    args: ['--disable-webgl', '--disable-webgl2'],
  })

  // ── Desktop: home dashboard ────────────────────────────────────────
  {
    const ctx = await browser.newContext({
      viewport: { width: 820, height: 620 },
      deviceScaleFactor: 2,
      colorScheme: 'dark',
    })
    const page = await ctx.newPage()
    await page.goto(`${base}/?mock=clean`)
    await page.waitForSelector('.home-section')
    // The version footer would show the mock fixture's version string;
    // it's noise in a marketing shot.
    await page.addStyleTag({ content: '.home-footer { display: none }' })
    await page.waitForTimeout(1200) // fonts + status dots settle
    await page.screenshot({ path: join(outDir, 'hero-desktop.png') })
    await ctx.close()
  }

  // ── Mobile: session terminal with the key row ─────────────────────
  {
    const ctx = await browser.newContext({
      // 310 CSS px fits exactly 38 xterm columns (13px Fira Code ≈
      // 7.8px cells), matching the mock session's 38-char content so
      // the terminal reads edge-to-edge like a real phone session; 540
      // tall yields 23 rows, so the terminal ends just under the key
      // row's top edge (one row tucked behind the translucent keys,
      // key borders visible) instead of painting under the whole bar.
      viewport: { width: 310, height: 540 },
      deviceScaleFactor: 2,
      isMobile: true,
      hasTouch: true,
      colorScheme: 'dark',
      userAgent:
        'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1',
    })
    const page = await ctx.newPage()
    // The claude "design landing page" session from the mock fixture.
    await page.goto(`${base}/my-project/claude/sess-cla?mock=clean`)
    await page.waitForSelector('.xterm')
    await page.waitForTimeout(1000)
    // Nudge the viewport to force a bar-aware refit: the initial mock fit
    // can run before the mobile control bar has laid out, leaving the
    // terminal sized to the full shell height.
    await page.setViewportSize({ width: 310, height: 541 })
    await page.waitForTimeout(200)
    await page.setViewportSize({ width: 310, height: 540 })
    await page.waitForTimeout(800)
    await page.screenshot({ path: join(outDir, 'hero-mobile.png') })
    await ctx.close()
  }

  await browser.close()
  console.log('wrote', join(outDir, 'hero-desktop.png'))
  console.log('wrote', join(outDir, 'hero-mobile.png'))
} finally {
  if (server) server.kill('SIGTERM')
}

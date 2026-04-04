#!/usr/bin/env node
/**
 * Generate desktop + mobile hero screenshots for the landing page.
 *
 * Usage:
 *   node scripts/gen-hero.cjs           # mock server must be running on :5199
 *   node scripts/gen-hero.cjs --serve   # auto-start vite dev server
 *
 * Output:
 *   apps/website/src/assets/hero-desktop.png
 *   apps/website/src/assets/hero-mobile.png
 */

const { chromium } = require('playwright')
const { spawn } = require('child_process')
const fs = require('fs')
const path = require('path')

const ROOT = path.resolve(__dirname, '..')
const PORT = 5199
const BASE = `http://localhost:${PORT}`

// Desktop: ?host=laptop hides the "laptop" peer pill (you're ON the laptop)
const DESKTOP_URL = `${BASE}/?mock&host=laptop`
// Mobile: shows all peer pills (you're viewing from a different device)
const MOBILE_URL = `${BASE}/?mock`

async function waitForServer(url, timeoutMs = 15000) {
  const start = Date.now()
  while (Date.now() - start < timeoutMs) {
    try {
      const r = await fetch(url)
      if (r.ok) return
    } catch {}
    await new Promise(r => setTimeout(r, 200))
  }
  throw new Error(`Server not ready at ${url} after ${timeoutMs}ms`)
}

const FREEZE_CSS = `
  *, *::before, *::after {
    transition: none !important;
    animation-duration: 0s !important;
    animation-delay: 0s !important;
    animation-iteration-count: 1 !important;
    caret-color: transparent !important;
  }

  .session-dot-indicator.working,
  .terminal-loading-dot,
  .session-dot.working,
  .main-header-status .session-dot {
    opacity: 1 !important;
    animation: none !important;
    transform: none !important;
    filter: saturate(1.15) brightness(1.12) !important;
  }
`

// Widen the terminal so long lines don't visibly wrap. xterm's canvas renderer
// paints cells at fixed font-metric pixel size, so making the canvas wider just
// extends the text past the viewport edge (which is clipped). We also dispatch
// a resize event so MockTerminal's onResize -> fit.fit() recomputes cols at the
// new width, giving the terminal room for ~260 cols with no wrapping.
const NO_WRAP_CSS = `
  .terminal-shell, .terminal-container, .terminal, .xterm-scrollable-element, .xterm-screen {
    width: 2000px !important;
  }
`

async function applyNoWrap(page) {
  await page.addStyleTag({ content: NO_WRAP_CSS })
  await page.evaluate(() => window.dispatchEvent(new Event('resize')))
  await page.waitForTimeout(400)
}

async function preparePage(page, url) {
  await page.goto(url, { timeout: 5000, waitUntil: 'load' })
  await page.waitForSelector('.session-item', { timeout: 5000 })
  // Wait for terminal mock content to finish writing
  await page.waitForTimeout(800)
  await page.addStyleTag({ content: FREEZE_CSS })
}

async function takeDesktop(browser) {
  console.log('Taking desktop screenshot...')
  const page = await browser.newPage({
    viewport: { width: 800, height: 650 },
    deviceScaleFactor: 2,
  })

  await preparePage(page, DESKTOP_URL)
  await applyNoWrap(page)

  const outPath = path.join(ROOT, 'apps/website/src/assets/hero-desktop.png')
  await page.screenshot({ path: outPath })
  await page.close()

  const stat = fs.statSync(outPath)
  console.log(`  → ${path.relative(ROOT, outPath)} (${(stat.size / 1024).toFixed(0)}KB)`)
}

async function takeMobile(browser) {
  console.log('Taking mobile screenshot...')
  // hasTouch triggers the pointer:coarse media query (sidebar overlay + mobile bar).
  // isMobile is intentionally false: it causes xterm's canvas renderer to stretch
  // cells when combined with the width: 2000px override.
  const page = await browser.newPage({
    viewport: { width: 310, height: 560 },
    deviceScaleFactor: 2,
    isMobile: false,
    hasTouch: true,
  })

  await preparePage(page, MOBILE_URL)
  await applyNoWrap(page)
  await page.waitForSelector('.mobile-bottom-bar', { timeout: 5000 })

  // Open the sidebar
  const menuBtn = await page.$('.mobile-bottom-bar button:first-child')
  if (menuBtn) {
    await menuBtn.click()
    await page.waitForTimeout(300)
  }

  // Re-freeze + re-apply width override after sidebar layout settles
  await page.addStyleTag({ content: FREEZE_CSS })
  await page.addStyleTag({ content: NO_WRAP_CSS })
  await page.waitForTimeout(200)

  const outPath = path.join(ROOT, 'apps/website/src/assets/hero-mobile.png')
  await page.screenshot({ path: outPath })
  await page.close()

  const stat = fs.statSync(outPath)
  console.log(`  → ${path.relative(ROOT, outPath)} (${(stat.size / 1024).toFixed(0)}KB)`)
}

;(async () => {
  const shouldServe = process.argv.includes('--serve')
  let server = null

  try {
    if (shouldServe) {
      console.log('Starting vite dev server...')
      server = spawn('npx', ['vite', '--port', String(PORT)], {
        cwd: path.join(ROOT, 'apps/gmux-web'),
        env: { ...process.env, VITE_MOCK: '1' },
        stdio: 'pipe',
      })
    }

    await waitForServer(`${BASE}/?mock`)
    console.log('Server ready.')

    // xterm's fit.fit() reflow behaves differently on the first page load in a
    // browser vs. subsequent loads (browser-level state, not isolated by
    // contexts). Use a fresh browser per shot to guarantee first-load behavior.
    const launchArgs = { args: ['--disable-blink-features=TextAutosizing'] }

    const d = await chromium.launch(launchArgs)
    await takeDesktop(d)
    await d.close()

    const m = await chromium.launch(launchArgs)
    await takeMobile(m)
    await m.close()

    console.log('\n✓ Hero screenshots generated.')
  } finally {
    if (server) server.kill('SIGTERM')
  }
})()

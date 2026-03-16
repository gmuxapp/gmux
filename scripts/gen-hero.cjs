#!/usr/bin/env node
/**
 * Generate hero image for the landing page.
 *
 * Takes desktop + mobile screenshots of the mock UI, composites them
 * into a single image with the phone overlapping the desktop corner.
 *
 * Usage:
 *   node scripts/gen-hero.cjs           # mock server must be running on :5199
 *   node scripts/gen-hero.cjs --serve   # auto-start vite dev server
 *
 * Output:
 *   apps/website/src/assets/hero-desktop.png
 *   apps/website/src/assets/hero-mobile.png
 *   apps/website/public/hero.png  (composite)
 */

const { chromium } = require('playwright')
const { spawn } = require('child_process')
const fs = require('fs')
const path = require('path')

const ROOT = path.resolve(__dirname, '..')
const PORT = 5199
const URL = `http://localhost:${PORT}/?mock`

async function waitForServer(url, timeoutMs = 15000) {
  const start = Date.now()
  while (Date.now() - start < timeoutMs) {
    try { const r = await fetch(url); if (r.ok) return } catch {}
    await new Promise(r => setTimeout(r, 200))
  }
  throw new Error(`Server not ready at ${url} after ${timeoutMs}ms`)
}

async function takeScreenshots(browser) {
  // Desktop — tighter viewport to fill more of the frame
  console.log('Taking desktop screenshot...')
  const dPage = await browser.newPage({
    viewport: { width: 1100, height: 700 },
    deviceScaleFactor: 2,
  })
  await dPage.goto(URL, { timeout: 5000, waitUntil: 'load' })
  await dPage.waitForSelector('.session-item', { timeout: 5000 })
  await dPage.waitForTimeout(500)
  await dPage.click('.session-item')
  await dPage.waitForTimeout(800)

  const desktopPath = path.join(ROOT, 'apps/website/src/assets/hero-desktop.png')
  await dPage.screenshot({ path: desktopPath })
  console.log(`  → ${path.relative(ROOT, desktopPath)}`)

  // Mobile with sidebar open
  console.log('Taking mobile screenshot...')
  const mPage = await browser.newPage({
    viewport: { width: 390, height: 760 },
    deviceScaleFactor: 2,
    isMobile: true,
    hasTouch: true,
  })
  await mPage.goto(URL, { timeout: 5000, waitUntil: 'load' })
  await mPage.waitForSelector('.mobile-bottom-bar', { timeout: 5000 })
  await mPage.waitForTimeout(500)

  // Open sidebar
  const menuBtn = await mPage.$('.mobile-bottom-bar button:first-child')
  if (menuBtn) {
    await menuBtn.click()
    await mPage.waitForTimeout(600)
  }

  const mobilePath = path.join(ROOT, 'apps/website/src/assets/hero-mobile.png')
  await mPage.screenshot({ path: mobilePath })
  console.log(`  → ${path.relative(ROOT, mobilePath)}`)

  return { desktopPath, mobilePath }
}

async function composite(browser, desktopPath, mobilePath) {
  console.log('Compositing...')
  const desktopB64 = fs.readFileSync(desktopPath).toString('base64')
  const mobileB64 = fs.readFileSync(mobilePath).toString('base64')

  // Viewport must be large enough to contain the canvas at CSS size (half pixel size)
  const page = await browser.newPage({
    viewport: { width: 1800, height: 1400 },
    deviceScaleFactor: 2,
  })

  await page.setContent(`<html><body style="margin:0;background:transparent">
    <canvas id="c"></canvas>
    <script>
      async function draw() {
        const desktop = new Image()
        desktop.src = 'data:image/png;base64,${desktopB64}'
        await desktop.decode()
        const mobile = new Image()
        mobile.src = 'data:image/png;base64,${mobileB64}'
        await mobile.decode()

        // Both images are @2x — draw at full pixel size for max sharpness
        const dW = desktop.width   // 2200
        const dH = desktop.height  // 1400

        // Phone drawn large — it's the star of the show
        // ~40% of desktop width so it's prominent and in front
        const phoneScale = 0.40
        const mDrawW = dW * phoneScale
        const mDrawH = (mobile.height / mobile.width) * mDrawW

        const pad = 80
        const canvasW = dW + mDrawW * 0.35 + pad * 2
        const canvasH = Math.max(dH, mDrawH) + pad * 2

        const c = document.getElementById('c')
        c.width = canvasW
        c.height = canvasH
        // CSS size = half canvas for 2x density display
        c.style.width = (canvasW / 2) + 'px'
        c.style.height = (canvasH / 2) + 'px'

        const ctx = c.getContext('2d')

        const dx = pad
        const dy = pad + (canvasH - 2 * pad - dH) * 0.3  // slightly above center

        // Desktop shadow
        ctx.save()
        ctx.shadowColor = 'rgba(0,0,0,0.35)'
        ctx.shadowBlur = 100
        ctx.shadowOffsetY = 30
        roundRect(ctx, dx, dy, dW, dH, 24)
        ctx.fillStyle = '#0f141a'
        ctx.fill()
        ctx.restore()

        // Desktop image — drawn at 1:1 pixel ratio (no scaling)
        ctx.save()
        roundRect(ctx, dx, dy, dW, dH, 24)
        ctx.clip()
        ctx.drawImage(desktop, dx, dy)
        ctx.restore()

        // Desktop border
        ctx.save()
        roundRect(ctx, dx, dy, dW, dH, 24)
        ctx.strokeStyle = 'rgba(255,255,255,0.08)'
        ctx.lineWidth = 2
        ctx.stroke()
        ctx.restore()

        // Phone position — overlapping right side, vertically centered, in front
        const bezel = 14
        const mx = dx + dW - mDrawW * 0.25
        const my = dy + (dH - mDrawH) * 0.5

        // Phone shadow (larger, more dramatic since phone is in front)
        ctx.save()
        ctx.shadowColor = 'rgba(0,0,0,0.5)'
        ctx.shadowBlur = 80
        ctx.shadowOffsetX = -12
        ctx.shadowOffsetY = 20
        roundRect(ctx, mx - bezel, my - bezel, mDrawW + bezel * 2, mDrawH + bezel * 2, 48)
        ctx.fillStyle = '#111'
        ctx.fill()
        ctx.restore()

        // Bezel highlight
        ctx.save()
        roundRect(ctx, mx - bezel, my - bezel, mDrawW + bezel * 2, mDrawH + bezel * 2, 48)
        ctx.strokeStyle = 'rgba(255,255,255,0.12)'
        ctx.lineWidth = 2
        ctx.stroke()
        ctx.restore()

        // Phone screen
        ctx.save()
        roundRect(ctx, mx, my, mDrawW, mDrawH, 34)
        ctx.clip()
        ctx.drawImage(mobile, 0, 0, mobile.width, mobile.height, mx, my, mDrawW, mDrawH)
        ctx.restore()

        document.title = 'done|' + canvasW + '|' + canvasH
      }

      function roundRect(ctx, x, y, w, h, r) {
        ctx.beginPath()
        ctx.moveTo(x + r, y)
        ctx.lineTo(x + w - r, y)
        ctx.arcTo(x + w, y, x + w, y + r, r)
        ctx.lineTo(x + w, y + h - r)
        ctx.arcTo(x + w, y + h, x + w - r, y + h, r)
        ctx.lineTo(x + r, y + h)
        ctx.arcTo(x, y + h, x, y + h - r, r)
        ctx.lineTo(x, y + r)
        ctx.arcTo(x, y, x + r, y, r)
        ctx.closePath()
      }

      draw()
    </script></body></html>`)

  await page.waitForFunction(() => document.title.startsWith('done'), { timeout: 10000 })

  // Read canvas dimensions from title
  const title = await page.title()
  const [, cw, ch] = title.split('|').map(Number)

  const canvas = await page.$('#c')
  const box = await canvas.boundingBox()
  const outPath = path.join(ROOT, 'apps/website/public/hero.png')
  await page.screenshot({
    path: outPath,
    clip: { x: box.x, y: box.y, width: box.width, height: box.height },
    omitBackground: true,
  })
  console.log(`  → ${path.relative(ROOT, outPath)}`)

  // Report sizes
  const stat = fs.statSync(outPath)
  console.log(`  ${(stat.size / 1024).toFixed(0)}KB, canvas ${cw}x${ch}px`)

  return outPath
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
      await waitForServer(URL)
      console.log('Server ready.')
    }

    const browser = await chromium.launch()
    const { desktopPath, mobilePath } = await takeScreenshots(browser)
    await composite(browser, desktopPath, mobilePath)
    await browser.close()

    console.log('\n✓ Hero images generated.')
  } finally {
    if (server) server.kill('SIGTERM')
  }
})()

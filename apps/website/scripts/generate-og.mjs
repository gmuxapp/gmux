#!/usr/bin/env node
/**
 * Generate the Open Graph card (apps/website/public/og.png, 1200×630).
 *
 * 1200×630 (1.91:1) is the size every major platform recommends;
 * previews render at ~500px wide, so 1× is already sharp, and staying
 * small keeps WhatsApp (which drops oversized og:images) happy.
 *
 * Renders a small HTML card that reuses the landing page's design
 * tokens (hero gradient, Instrument Sans, teal accent) and the real
 * hero-desktop screenshot, then captures it with Playwright.
 *
 * Usage:
 *   node apps/website/scripts/generate-og.mjs
 *
 * Regenerate whenever the tagline or hero screenshot changes.
 */

import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'

const here = dirname(fileURLToPath(import.meta.url))
const heroShot = join(here, '..', 'src', 'assets', 'hero-desktop.png')
const outPath = join(here, '..', 'public', 'og.png')
const shotDataUri = `data:image/png;base64,${readFileSync(heroShot).toString('base64')}`

const html = `<!doctype html>
<html>
<head>
<meta charset="utf-8" />
<link rel="preconnect" href="https://fonts.googleapis.com" />
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin />
<link href="https://fonts.googleapis.com/css2?family=Instrument+Sans:wght@400;500;600;700&family=Source+Sans+3:wght@400;500&display=swap" rel="stylesheet" />
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    width: 1200px;
    height: 630px;
    overflow: hidden;
    position: relative;
    background:
      radial-gradient(ellipse 52% 110% at 78% 42%, oklch(49% 0.095 195 / 0.22), transparent 68%),
      radial-gradient(ellipse 36% 70% at 92% 88%, oklch(34% 0.055 225 / 0.14), transparent 72%),
      linear-gradient(180deg, oklch(15.5% 0.018 250), oklch(13% 0.016 250));
    font-family: 'Instrument Sans', sans-serif;
  }
  .wordmark {
    position: absolute;
    top: 64px;
    left: 72px;
    font-size: 40px;
    font-weight: 700;
    letter-spacing: -0.04em;
    color: oklch(88% 0.01 250);
  }
  h1 {
    position: absolute;
    left: 72px;
    top: 190px;
    font-size: 78px;
    font-weight: 700;
    line-height: 1.06;
    letter-spacing: -0.045em;
    color: oklch(90% 0.01 250);
  }
  .sub {
    position: absolute;
    left: 72px;
    top: 396px;
    font-family: 'Source Sans 3', sans-serif;
    font-size: 36px;
    line-height: 1.4;
    color: oklch(68% 0.01 250);
  }
  .shot {
    position: absolute;
    top: 96px;
    left: 700px;
    width: 640px;
    border-radius: 6px;
    border: 1px solid oklch(58% 0.035 220 / 0.28);
    box-shadow:
      0 2px 0 oklch(100% 0 0 / 0.03) inset,
      0 40px 90px oklch(4% 0.01 250 / 0.7);
  }
</style>
</head>
<body>
  <div class="wordmark">gmux</div>
  <h1>The control plane<br />for AI agents.</h1>
  <div class="sub">Subagents · Remote access<br />Any coding agent</div>
  <img class="shot" src="${shotDataUri}" />
</body>
</html>`

const browser = await chromium.launch({ headless: true })
const page = await browser.newPage({
  viewport: { width: 1200, height: 630 },
  deviceScaleFactor: 1,
})
await page.setContent(html, { waitUntil: 'networkidle' })
await page.evaluate(() => document.fonts.ready)
await page.screenshot({ path: outPath })
await browser.close()
console.log('wrote', outPath)

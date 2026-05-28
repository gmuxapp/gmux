/**
 * record-tiptap-demo.ts
 *
 * Records a GIF showing the TipTap v3 markdown editor in gmux-web:
 *   1. Start the Vite dev server (worktree, proxies to running gmuxd)
 *   2. Authenticate via /auth/login?token= URL flow
 *   3. Navigate directly to the markdown editor for demo-tiptap.md
 *   4. Show the loaded content, toolbar, type some text, use bold/italic
 *   5. Capture frames → encode as animated GIF via ffmpeg
 *
 * Prerequisites:
 *   - gmuxd running on localhost:8790
 *   - demo-tiptap.md written to the "home" project (~/demo-tiptap.md)
 *   - pnpm deps installed in the worktree
 *
 * Usage (from worktree root):
 *   npx tsx scripts/record-tiptap-demo.ts
 *
 * Output: tasks/gmux/2026-05-27-tiptap-demo.gif
 */

import { chromium } from 'playwright'
import * as fs from 'node:fs'
import * as path from 'node:path'
import * as net from 'node:net'
import { spawn, execSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

const __filename = fileURLToPath(import.meta.url)
const __dir = path.dirname(__filename)
const ROOT = path.resolve(__dir, '..')

const TOKEN_FILE = path.join(process.env.HOME!, '.local', 'state', 'gmux', 'auth-token')
const GMUXD_PORT = 8790

const OUT_GIF = '/Users/james-carmody/james-agent-workspace/tasks/gmux/2026-05-27-tiptap-demo.gif'
const FRAMES_DIR = '/tmp/tiptap-gif-frames'

// ── helpers ───────────────────────────────────────────────────────────────

function sleep(ms: number) { return new Promise(r => setTimeout(r, ms)) }

async function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer()
    srv.listen(0, '127.0.0.1', () => {
      const port = (srv.address() as net.AddressInfo).port
      srv.close(() => resolve(port))
    })
    srv.on('error', reject)
  })
}

async function waitForServer(port: number, ms = 30_000): Promise<void> {
  const start = Date.now()
  while (Date.now() - start < ms) {
    try {
      const r = await fetch(`http://127.0.0.1:${port}/`)
      if (r.status < 500) return
    } catch { /* retry */ }
    await sleep(300)
  }
  throw new Error(`dev server not up after ${ms}ms`)
}

// ── frame capture ─────────────────────────────────────────────────────────

let frameIdx = 0

function initFramesDir() {
  if (fs.existsSync(FRAMES_DIR)) fs.rmSync(FRAMES_DIR, { recursive: true })
  fs.mkdirSync(FRAMES_DIR, { recursive: true })
  frameIdx = 0
}

// ── GIF encoding via ffmpeg ───────────────────────────────────────────────

async function encodeGif(framesDir: string, outPath: string, fps = 10): Promise<void> {
  console.log(`[gif] encoding ${frameIdx} frames at ${fps}fps → ${outPath}`)
  fs.mkdirSync(path.dirname(outPath), { recursive: true })

  // Two-pass: generate palette, then encode
  const palette = path.join(framesDir, 'palette.png')
  execSync(
    `ffmpeg -y -framerate ${fps} -i "${framesDir}/frame-%05d.png" \
      -vf "fps=${fps},scale=1280:-1:flags=lanczos,palettegen=stats_mode=full" \
      "${palette}"`,
    { stdio: 'pipe' }
  )
  execSync(
    `ffmpeg -y -framerate ${fps} -i "${framesDir}/frame-%05d.png" -i "${palette}" \
      -lavfi "fps=${fps},scale=1280:-1:flags=lanczos [x]; [x][1:v] paletteuse=dither=bayer:bayer_scale=5" \
      "${outPath}"`,
    { stdio: 'pipe' }
  )
  const kb = (fs.statSync(outPath).size / 1024).toFixed(0)
  console.log(`[gif] wrote ${outPath} (${kb} KB)`)
}

// ── main ──────────────────────────────────────────────────────────────────

async function main() {
  const token = fs.readFileSync(TOKEN_FILE, 'utf8').trim()

  // 1. Start Vite dev server
  const devPort = await freePort()
  console.log(`[setup] starting Vite dev server on :${devPort} …`)
  const vite = spawn(
    'node_modules/.bin/vite',
    ['--port', String(devPort), '--host', '127.0.0.1'],
    {
      cwd: path.join(ROOT, 'apps', 'gmux-web'),
      env: {
        ...process.env,
        VITE_DEV_PROXY_PORT: String(GMUXD_PORT),
        VITE_DEV_PROXY_HOST: '127.0.0.1',
        FORCE_COLOR: '0',
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    }
  )
  vite.stdout.on('data', (d: Buffer) => {
    if (process.env.DEBUG) process.stdout.write('[vite] ' + d)
  })
  vite.stderr.on('data', (d: Buffer) => {
    if (process.env.DEBUG) process.stderr.write('[vite] ' + d)
  })

  try {
    await waitForServer(devPort, 40_000)
    console.log(`[setup] Vite ready at :${devPort}`)

    // 2. Launch browser
    const W = 1280, H = 800
    const browser = await chromium.launch({ headless: true })
    const ctx = await browser.newContext({ viewport: { width: W, height: H } })
    const page = await ctx.newPage()

    initFramesDir()

    async function snap(): Promise<void> {
      const png = await page.screenshot()
      const name = `frame-${String(frameIdx).padStart(5, '0')}.png`
      fs.writeFileSync(path.join(FRAMES_DIR, name), png)
      frameIdx++
    }

    async function snapFor(ms: number, intervalMs = 80): Promise<void> {
      const end = Date.now() + ms
      while (Date.now() < end) { await snap(); await sleep(intervalMs) }
    }

    // 3. Authenticate via token URL query param
    console.log('[record] authenticating …')
    await page.goto(
      `http://127.0.0.1:${devPort}/auth/login?token=${encodeURIComponent(token)}`,
      { waitUntil: 'load' }
    )
    // Wait for the SPA to fully initialise: SSE handshake + sessionsLoaded signal.
    // The app sets window.__gmuxReady = true once the initial data arrives.
    // Fall back to a 4 s sleep if the flag isn't present (older builds).
    await page.waitForFunction(() => (window as any).__gmuxReady === true, { timeout: 20_000 })
      .catch(async () => {
        console.log('[record] __gmuxReady not found, using 4 s delay')
        await sleep(4000)
      })
    await sleep(1500)  // extra breathing room
    await snap()

    // 4. Navigate to the markdown editor URL (store is now ready)
    const encodedPath = encodeURIComponent('demo-tiptap.md')
    const editorUrl = `http://127.0.0.1:${devPort}/home/_md/${encodedPath}`
    console.log('[record] opening markdown editor …')
    await page.evaluate((url) => {
      window.history.pushState({}, '', url)
      window.dispatchEvent(new PopStateEvent('popstate'))
    }, editorUrl)
    // Wait for the TipTap ProseMirror editor to appear
    await page.waitForSelector('.ProseMirror', { timeout: 30_000 })
    await sleep(500)
    console.log('[record] editor loaded')

    // ── Act 1: show the loaded document ───────────────────────────────
    console.log('[record] act 1 — show loaded document')
    await snapFor(1500, 100)

    // ── Act 2: scroll down slowly to show content ─────────────────────
    console.log('[record] act 2 — scrolling')
    const scroll = page.locator('.md-editor-scroll')
    for (let i = 0; i < 5; i++) {
      await scroll.evaluate(el => { el.scrollTop += 80 })
      await snapFor(200, 100)
    }
    await snapFor(600, 100)

    // ── Act 3: click into editor, type new text ──────────────────────
    console.log('[record] act 3 — typing')
    // Scroll back to top, then click near the bottom of visible content
    await page.locator('.md-editor-scroll').evaluate(el => { el.scrollTop = 0 })
    await sleep(200)
    // Use keyboard shortcut to move to end of doc rather than positional click
    await page.locator('.ProseMirror').click({ force: true })
    await page.keyboard.press('Control+End')
    await page.keyboard.type('\n\n## What\'s new in TipTap v3\n\n')
    await snapFor(400, 80)

    await page.keyboard.type('Replaced Milkdown with ')
    await snapFor(300, 80)

    // ── Act 4: click Bold toolbar button, type bold text ──────────────
    console.log('[record] act 4 — bold via toolbar')
    await page.locator('.md-toolbar-btn').filter({ hasText: 'B' }).click()
    await snapFor(200, 80)
    await page.keyboard.type('TipTap v3')
    await snapFor(300, 80)
    await page.locator('.md-toolbar-btn').filter({ hasText: 'B' }).click()
    await page.keyboard.type(' for a cleaner, sync editor init.')
    await snapFor(400, 80)

    // ── Act 5: add italic text ─────────────────────────────────────────
    console.log('[record] act 5 — italic')
    await page.keyboard.type('\n\nEditor is ')
    await page.locator('.md-toolbar-btn').filter({ hasText: 'I' }).click()
    await page.keyboard.type('headless')
    await page.locator('.md-toolbar-btn').filter({ hasText: 'I' }).click()
    await page.keyboard.type(' and fully typed.')
    await snapFor(500, 80)

    // ── Act 6: Cmd+S save ──────────────────────────────────────────────
    console.log('[record] act 6 — save')
    await page.keyboard.press('Meta+S')
    await snapFor(1200, 80) // show "Saved" status

    // ── Act 7: show toolbar hover over H1/H2 ──────────────────────────
    console.log('[record] act 7 — toolbar hover')
    const h1btn = page.locator('.md-toolbar-btn').filter({ hasText: 'H1' })
    await h1btn.hover()
    await snapFor(600, 80)

    // ── Act 8: scroll back to top to show full doc ────────────────────
    console.log('[record] act 8 — scroll to top')
    for (let i = 0; i < 8; i++) {
      await scroll.evaluate(el => { el.scrollTop -= 120 })
      await snapFor(150, 80)
    }
    await snapFor(1000, 80)

    await browser.close()
    console.log(`[record] captured ${frameIdx} frames`)

    // 5. Encode GIF
    await encodeGif(FRAMES_DIR, OUT_GIF, 12)

  } finally {
    vite.kill()
  }

  console.log(`\n✓ done: ${OUT_GIF}`)
}

main().catch(e => { console.error(e); process.exit(1) })

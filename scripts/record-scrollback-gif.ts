/**
 * record-scrollback-gif.ts
 *
 * Records a GIF demonstrating the scrollback conversation-recovery fix:
 *   1. Start gmuxd + spawn pi --session <long-jsonl> -c
 *   2. Wait for on-disk scrollback to grow (pi redraws the full history)
 *   3. Open the web UI in a headless Chromium browser
 *   4. Navigate to the session
 *   5. Scroll to the top of the buffer
 *   6. Capture frames throughout; encode as animated GIF
 *
 * Output: tasks/james-gmux/2026-05-26-scrollback-demo.gif
 *
 * Usage:
 *   npx tsx scripts/record-scrollback-gif.ts
 */

import { chromium } from 'playwright'
import * as fs from 'node:fs'
import * as path from 'node:path'
import * as os from 'node:os'
import * as net from 'node:net'
import { spawn, execSync } from 'node:child_process'
import { createRequire } from 'node:module'

// ── paths ──────────────────────────────────────────────────────────────────

import { fileURLToPath } from 'node:url'
const __filename = fileURLToPath(import.meta.url)
const __dirname_ts = path.dirname(__filename)
const ROOT = path.resolve(__dirname_ts, '..')
const GMUXD = path.join(ROOT, 'bin', 'gmuxd')
const GMUX  = path.join(ROOT, 'bin', 'gmux')

const REAL_PI_JSONL =
  '/Users/james-carmody/james-agent-workspace/.pi-user/sessions/' +
  '2026-05-11T07-49-10-051Z_019e1602-e923-7748-9b5a-8e6eba50b647.jsonl'

const OUT_GIF = '/Users/james-carmody/james-agent-workspace/tasks/james-gmux/2026-05-26-scrollback-demo.gif'

// ── helpers ────────────────────────────────────────────────────────────────

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

async function waitForHealth(port: number, token: string, ms = 15_000): Promise<void> {
  const start = Date.now()
  while (Date.now() - start < ms) {
    try {
      const r = await fetch(`http://127.0.0.1:${port}/v1/health`,
        { headers: { Authorization: `Bearer ${token}` } })
      if (r.ok) return
    } catch { /* retry */ }
    await sleep(200)
  }
  throw new Error(`gmuxd not healthy after ${ms}ms`)
}

async function waitForSession(
  port: number, token: string, cwd: string, ms = 20_000
): Promise<string> {
  const start = Date.now()
  while (Date.now() - start < ms) {
    try {
      const r = await fetch(`http://127.0.0.1:${port}/v1/sessions`,
        { headers: { Authorization: `Bearer ${token}` } })
      const body = await r.json() as { data: Array<{ id: string; alive: boolean; cwd?: string }> }
      const m = body.data.find(s => s.alive && s.cwd === cwd)
      if (m) return m.id
    } catch { /* retry */ }
    await sleep(200)
  }
  throw new Error(`no session with cwd=${cwd} after ${ms}ms`)
}

function sleep(ms: number) { return new Promise(r => setTimeout(r, ms)) }

function scrollbackBytes(stateDir: string, sessionId: string): number {
  const dir = path.join(stateDir, 'gmux', 'sessions', sessionId)
  let total = 0
  for (const name of ['scrollback.0', 'scrollback']) {
    try { total += fs.statSync(path.join(dir, name)).size } catch { /* missing */ }
  }
  return total
}

// ── GIF encoder (gifencoder + pngjs from /tmp/node_modules) ───────────────

const req = createRequire(import.meta.url)
const GIFEncoder = req('/tmp/node_modules/gifencoder')
const { PNG }    = req('/tmp/node_modules/pngjs')

async function pngToRgba(pngBuf: Buffer): Promise<{ data: Buffer; width: number; height: number }> {
  return new Promise((resolve, reject) => {
    const png = new PNG()
    png.parse(pngBuf, (err: Error | null, data: typeof png) => {
      if (err) reject(err)
      else resolve({ data: data.data as Buffer, width: data.width, height: data.height })
    })
  })
}

async function encodeGif(
  frames: Buffer[],
  width: number,
  height: number,
  outPath: string,
  delayMs = 120,
): Promise<void> {
  console.log(`[gif] encoding ${frames.length} frames at ${width}×${height}…`)
  const encoder = new GIFEncoder(width, height)
  const writeStream = fs.createWriteStream(outPath)
  encoder.createReadStream().pipe(writeStream)
  encoder.start()
  encoder.setRepeat(0)    // loop forever
  encoder.setDelay(delayMs)
  encoder.setQuality(12)

  for (let i = 0; i < frames.length; i++) {
    const { data } = await pngToRgba(frames[i])
    encoder.addFrame(data)
    if (i % 5 === 0) process.stdout.write(`  frame ${i}/${frames.length}\r`)
  }
  encoder.finish()

  await new Promise<void>((resolve, reject) => {
    writeStream.on('finish', resolve)
    writeStream.on('error', reject)
  })
  console.log(`\n[gif] wrote ${outPath} (${(fs.statSync(outPath).size / 1024).toFixed(0)} KB)`)
}

// ── main ───────────────────────────────────────────────────────────────────

async function main() {
  if (!fs.existsSync(REAL_PI_JSONL)) {
    throw new Error(`pi session not found: ${REAL_PI_JSONL}`)
  }

  // 1. Build if needed
  if (!fs.existsSync(GMUXD) || !fs.existsSync(GMUX)) {
    console.log('[setup] building binaries…')
    execSync('./scripts/build.sh', { cwd: ROOT, stdio: 'inherit' })
  }

  // 2. Temp dirs
  const tmpDir    = fs.mkdtempSync(path.join(os.tmpdir(), 'gmux-gif-'))
  const socketDir = path.join(tmpDir, 'sockets')
  const configDir = path.join(tmpDir, 'config')
  const stateDir  = path.join(tmpDir, 'state')
  const workspace = path.join(tmpDir, 'workspace')
  const fakeHome  = path.join(tmpDir, 'home')
  for (const d of [socketDir, configDir, stateDir, workspace, fakeHome]) {
    fs.mkdirSync(d, { recursive: true })
  }

  const port       = await freePort()
  const token      = 'gif' + '0'.repeat(61)

  // Copy the real session JSONL into fakeHome so pi can resume it
  const piSessionsDir = path.join(fakeHome, '.pi-user', 'sessions')
  fs.mkdirSync(piSessionsDir, { recursive: true })
  const sessionFilename = path.basename(REAL_PI_JSONL)
  const copiedJsonl = path.join(piSessionsDir, sessionFilename)
  fs.copyFileSync(REAL_PI_JSONL, copiedJsonl)
  console.log(`[setup] copied ${(fs.statSync(copiedJsonl).size / 1e6).toFixed(1)} MB JSONL`)

  // Config
  const gmuxStateDir = path.join(stateDir, 'gmux')
  fs.mkdirSync(gmuxStateDir, { recursive: true })
  fs.writeFileSync(path.join(gmuxStateDir, 'projects.json'),
    JSON.stringify({ version: 2, items: [{ slug: 'demo', match: [{ path: workspace }] }] }))
  fs.mkdirSync(path.join(configDir, 'gmux'))
  fs.writeFileSync(path.join(configDir, 'gmux', 'host.toml'),
    `port = ${port}\n[discovery]\ndevcontainers = false\ntailscale = false\n[tailscale]\nenabled = false\n`)

  const env: Record<string, string> = {
    PATH: process.env.PATH || '',
    HOME: fakeHome,
    TERM: 'xterm-256color',
    GMUX_SOCKET_DIR: socketDir,
    GMUXD_TOKEN: token,
    XDG_CONFIG_HOME: configDir,
    XDG_STATE_HOME: stateDir,
    GMUX_CONFIG_DIR: path.join(configDir, 'gmux'),
  }

  // 3. Start gmuxd
  console.log('[setup] starting gmuxd…')
  const gmuxd = spawn(GMUXD, ['run'], { env, stdio: ['ignore', 'pipe', 'pipe'], detached: true })
  gmuxd.stderr?.on('data', (d: Buffer) => { if (process.env.DEBUG) process.stderr.write(d) })
  await waitForHealth(port, token)
  console.log(`[setup] gmuxd healthy on :${port}`)

  // 4. Spawn pi session
  const sessionCwd = path.join(workspace, 'pi-demo')
  fs.mkdirSync(sessionCwd)
  console.log('[setup] spawning pi --session <jsonl> -c …')
  const piProc = spawn(GMUX, ['pi', '--session', copiedJsonl, '-c'], {
    env: { ...env, HOME: fakeHome },
    cwd: sessionCwd,
    stdio: ['ignore', 'pipe', 'pipe'],
    detached: true,
  })
  piProc.stderr?.on('data', (d: Buffer) => { if (process.env.DEBUG) process.stderr.write(d) })

  const sessionId = await waitForSession(port, token, sessionCwd, 30_000)
  console.log(`[setup] session: ${sessionId}`)

  // 5. Wait for scrollback to grow past 200 KB
  console.log('[setup] waiting for scrollback (>200KB)…')
  const t0 = Date.now()
  while (Date.now() - t0 < 90_000) {
    const bytes = scrollbackBytes(stateDir, sessionId)
    process.stdout.write(`  scrollback: ${(bytes / 1024).toFixed(0)} KB\r`)
    if (bytes > 200 * 1024) break
    await sleep(500)
  }
  const finalBytes = scrollbackBytes(stateDir, sessionId)
  console.log(`\n[setup] scrollback ready: ${(finalBytes / 1024).toFixed(0)} KB`)

  // 6. Open browser and record
  const W = 1280, H = 800
  const browser = await chromium.launch({ headless: true })
  const context = await browser.newContext({ viewport: { width: W, height: H } })
  const page    = await context.newPage()

  const frames: Buffer[] = []

  async function snap(label?: string): Promise<void> {
    const png = await page.screenshot()
    frames.push(png)
    if (label) console.log(`  [frame ${frames.length}] ${label}`)
  }

  async function snapFor(ms: number, intervalMs = 80): Promise<void> {
    const end = Date.now() + ms
    while (Date.now() < end) { await snap(); await sleep(intervalMs) }
  }

  // ── Act 1: open the app ──────────────────────────────────────────────────
  console.log('[record] opening app…')
  await page.goto(`http://127.0.0.1:${port}`, { waitUntil: 'networkidle' })
  await snapFor(600)

  // ── Act 2: navigate to the session ──────────────────────────────────────
  console.log('[record] navigating to session…')
  await page.waitForFunction((id) => {
    const nav = (window as any).__gmuxNavigateToSession
    return typeof nav === 'function' && nav(id) === true
  }, sessionId, { timeout: 15_000 })

  await page.locator('.terminal-container canvas').waitFor({ state: 'visible', timeout: 8_000 })
  await snapFor(400) // show "connecting…" or initial state

  // Give prefetch + WS snapshot time to settle
  await sleep(3000)
  await snap('prefetch settled — live screen visible')
  await snapFor(800)

  // ── Act 3: show the live (bottom) state ─────────────────────────────────
  console.log('[record] pausing on live state…')
  await snapFor(1200) // ~1 s of stable live state

  // ── Act 4: scroll to top ────────────────────────────────────────────────
  console.log('[record] scrolling to top…')

  // Smooth scroll: animate scrollToLine over ~60 steps
  const totalScrollbackLines = await page.evaluate(() => {
    const term = (window as any).__gmuxTerm
    return term ? term.getScrollbackLength() : 0
  })
  console.log(`  scrollback lines: ${totalScrollbackLines}`)

  const steps = 40
  for (let i = 0; i <= steps; i++) {
    const line = Math.round(totalScrollbackLines * (1 - i / steps))
    await page.evaluate((l) => (window as any).__gmuxTerm?.scrollToLine(l), line)
    await snap()
    await sleep(50)
  }

  // ── Act 5: hold at top ───────────────────────────────────────────────────
  console.log('[record] holding at top…')
  await snapFor(2000, 100)

  // ── Act 6: scroll back down to live state ────────────────────────────────
  console.log('[record] scrolling back down…')
  for (let i = 0; i <= steps; i++) {
    const line = Math.round(totalScrollbackLines * (i / steps))
    await page.evaluate((l) => (window as any).__gmuxTerm?.scrollToLine(l), line)
    await snap()
    await sleep(50)
  }
  await snapFor(800)

  await browser.close()
  console.log(`[record] captured ${frames.length} frames`)

  // 7. Encode GIF
  await encodeGif(frames, W, H, OUT_GIF, 100)

  // 8. Cleanup
  try { piProc.kill() } catch { /* ok */ }
  try { gmuxd.kill() } catch { /* ok */ }

  console.log(`\n✓ done: ${OUT_GIF}`)
}

main().catch(e => { console.error(e); process.exit(1) })

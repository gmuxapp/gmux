/**
 * record-trees-gif.ts
 *
 * Records an animated GIF demonstrating the @pierre/trees file-tree integration.
 * Shows: load → expand/collapse → context menu → search
 *
 * Output: tasks/gmux-trees-integration/2026-05-27-trees-demo.gif
 * Usage:
 *   npx tsx scripts/record-trees-gif.ts
 *   E2E_SKIP_BUILD=1 npx tsx scripts/record-trees-gif.ts
 */

import { chromium, type Locator } from 'playwright'
import * as fs from 'node:fs'
import * as path from 'node:path'
import * as os from 'node:os'
import * as net from 'node:net'
import { spawn, execSync } from 'node:child_process'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

// ── paths ──────────────────────────────────────────────────────────────────

const __filename = fileURLToPath(import.meta.url)
const __dirname_ts = path.dirname(__filename)
const ROOT      = path.resolve(__dirname_ts, '..')
const PLATFORM  = `${process.platform === 'darwin' ? 'darwin' : 'linux'}-${process.arch === 'arm64' ? 'arm64' : 'amd64'}`
const GMUXD     = path.join(ROOT, 'bin', `gmuxd-${PLATFORM}`)
// workspace root = ROOT/../../.. (projects/james/james-gmux-trees-integration → workspace)
const WORKSPACE = path.resolve(ROOT, '..', '..', '..')
const OUT_GIF   = path.join(WORKSPACE, 'tasks', 'gmux-trees-integration', '2026-05-27-trees-demo.gif')

// ── GIF encoder ────────────────────────────────────────────────────────────

const req = createRequire(import.meta.url)
const GIFEncoder = req('/tmp/node_modules/gifencoder')
const { PNG }    = req('/tmp/node_modules/pngjs')

// ── helpers ────────────────────────────────────────────────────────────────

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

async function waitForHealth(port: number, token: string, ms = 20_000): Promise<void> {
  const start = Date.now()
  while (Date.now() - start < ms) {
    try {
      const r = await fetch(`http://127.0.0.1:${port}/v1/health`,
        { headers: { Authorization: `Bearer ${token}` } })
      if (r.ok) return
    } catch { /* retry */ }
    await sleep(250)
  }
  throw new Error(`gmuxd not healthy on :${port} after ${ms}ms`)
}

async function pngToRgba(buf: Buffer): Promise<{ data: Buffer; width: number; height: number }> {
  return new Promise((resolve, reject) => {
    const png = new PNG()
    png.parse(buf, (err: Error | null, d: typeof png) => {
      if (err) reject(err)
      else resolve({ data: d.data as Buffer, width: d.width, height: d.height })
    })
  })
}

async function encodeGif(
  frames: Buffer[],
  w: number, h: number,
  outPath: string,
  delayMs = 100,
): Promise<void> {
  console.log(`[gif] encoding ${frames.length} frames at ${w}×${h}…`)
  const encoder = new GIFEncoder(w, h)
  const ws = fs.createWriteStream(outPath)
  encoder.createReadStream().pipe(ws)
  encoder.start()
  encoder.setRepeat(0)
  encoder.setDelay(delayMs)
  encoder.setQuality(10)
  for (let i = 0; i < frames.length; i++) {
    const { data } = await pngToRgba(frames[i])
    encoder.addFrame(data)
    if (i % 10 === 0) process.stdout.write(`  frame ${i + 1}/${frames.length}\r`)
  }
  encoder.finish()
  await new Promise<void>((resolve, reject) => {
    ws.on('finish', resolve)
    ws.on('error', reject)
  })
  console.log(`\n[gif] wrote ${outPath} (${(fs.statSync(outPath).size / 1024).toFixed(0)} KB)`)
}

// ── demo project ────────────────────────────────────────────────────────────

function createDemoProject(dir: string): void {
  const mkf = (rel: string, content = '') =>
    (fs.mkdirSync(path.dirname(path.join(dir, rel)), { recursive: true }),
      fs.writeFileSync(path.join(dir, rel), content))

  mkf('README.md', '# my-app\n\nA sample project.\n')
  mkf('package.json', '{\n  "name": "my-app",\n  "version": "1.0.0"\n}\n')
  mkf('tsconfig.json', '{ "compilerOptions": { "strict": true } }\n')
  mkf('src/main.ts', 'import { App } from "./components/App"\nexport { App }\n')
  mkf('src/components/App.tsx', 'export function App() { return <div>Hello</div> }\n')
  mkf('src/components/Button.tsx', 'export function Button() { return <button>Click</button> }\n')
  mkf('src/components/Modal.tsx', 'export function Modal() { return <dialog>…</dialog> }\n')
  mkf('src/utils/helpers.ts', 'export function clsx(...args: string[]) { return args.join(" ") }\n')
  mkf('src/utils/api.ts', 'export async function fetchData(url: string) { return fetch(url) }\n')
  mkf('tests/App.test.ts', 'import { App } from "../src/components/App"\n')
  mkf('tests/helpers.test.ts', 'import { clsx } from "../src/utils/helpers"\n')
}

// ── item helper — uses aria-label (reliable, no doubled-text issue) ─────────

function treeItem(page: ReturnType<typeof chromium.prototype.newPage extends never ? never : any>, ariaLabel: string): Locator {
  return (page as any).locator(`[data-type="item"][aria-label="${ariaLabel}"]`)
}

// ── main ───────────────────────────────────────────────────────────────────

async function main() {
  // 1. Build
  if (!process.env.E2E_SKIP_BUILD) {
    console.log('[setup] building…')
    execSync('./scripts/build.sh', { cwd: ROOT, stdio: 'inherit' })
  } else {
    console.log('[setup] skipping build (E2E_SKIP_BUILD=1)')
    if (!fs.existsSync(GMUXD)) throw new Error(`gmuxd not found: ${GMUXD}`)
  }

  // 2. Temp env
  const tmpDir    = fs.mkdtempSync(path.join(os.tmpdir(), 'gmux-trees-gif-'))
  const socketDir = path.join(tmpDir, 'sockets')
  const configDir = path.join(tmpDir, 'config')
  const stateDir  = path.join(tmpDir, 'state')
  const fakeHome  = path.join(tmpDir, 'home')
  const demoDir   = path.join(tmpDir, 'my-app')
  for (const d of [socketDir, configDir, stateDir, fakeHome, demoDir]) {
    fs.mkdirSync(d, { recursive: true })
  }
  createDemoProject(demoDir)
  console.log(`[setup] demo project at ${demoDir}`)

  const port  = await freePort()
  const token = 'a'.repeat(64)

  const gmuxStateDir = path.join(stateDir, 'gmux')
  fs.mkdirSync(gmuxStateDir, { recursive: true })
  fs.writeFileSync(
    path.join(gmuxStateDir, 'projects.json'),
    JSON.stringify({ version: 2, items: [{ slug: 'my-app', match: [{ path: demoDir }] }] }),
  )
  fs.mkdirSync(path.join(configDir, 'gmux'))
  fs.writeFileSync(
    path.join(configDir, 'gmux', 'host.toml'),
    `port = ${port}\n[discovery]\ndevcontainers = false\ntailscale = false\n[tailscale]\nenabled = false\n`,
  )

  const env: Record<string, string> = {
    PATH: process.env.PATH ?? '',
    HOME: fakeHome,
    TERM: 'xterm-256color',
    GMUX_SOCKET_DIR: socketDir,
    GMUXD_TOKEN: token,
    XDG_CONFIG_HOME: configDir,
    XDG_STATE_HOME: stateDir,
    GMUX_CONFIG_DIR: path.join(configDir, 'gmux'),
  }

  // 3. Start gmuxd
  console.log(`[setup] starting gmuxd on :${port}…`)
  const gmuxd = spawn(GMUXD, ['run'], {
    env, stdio: ['ignore', 'pipe', 'pipe'], detached: true,
  })
  if (process.env.DEBUG) gmuxd.stderr?.on('data', (d: Buffer) => process.stderr.write(d))
  await waitForHealth(port, token)
  console.log('[setup] gmuxd healthy')

  // 4. Record
  const W = 900, H = 640
  const browser = await chromium.launch({ headless: true })
  const context = await browser.newContext({ viewport: { width: W, height: H } })
  const page = await context.newPage()

  // Auth via cookie
  await page.goto(`http://127.0.0.1:${port}/auth/login?token=${token}`, {
    waitUntil: 'load', timeout: 15_000,
  })

  const frames: Buffer[] = []
  let frameN = 0

  async function snap(label?: string): Promise<void> {
    const png = await page.screenshot()
    frames.push(png)
    frameN++
    if (label) console.log(`  [${frameN}] ${label}`)
  }

  async function snapFor(ms: number, intervalMs = 80): Promise<void> {
    const end = Date.now() + ms
    while (Date.now() < end) { await snap(); await sleep(intervalMs) }
  }

  // ── Act 1: home ──────────────────────────────────────────────────────────
  console.log('[record] opening app…')
  await page.goto(`http://127.0.0.1:${port}`, { waitUntil: 'load', timeout: 15_000 })
  await snap('home — my-app project visible')
  await snapFor(600)

  // ── Act 2: click my-app ──────────────────────────────────────────────────
  console.log('[record] clicking my-app…')
  await page.locator('a, button').filter({ hasText: 'my-app' }).first().click()
  await snapFor(400)

  // ── Act 3: wait for tree items ────────────────────────────────────────────
  console.log('[record] waiting for file tree…')
  await page.waitForSelector('[data-type="item"]', { timeout: 12_000 })
  await sleep(500) // let everything settle
  await snap('file tree loaded — directories with icons, expanded by default')
  await snapFor(1000)

  // ── Act 4: collapse src/ ─────────────────────────────────────────────────
  console.log('[record] collapsing src/…')
  const srcItem = page.locator('[data-type="item"][aria-label="src"]')
  await srcItem.click()
  await snapFor(500)
  await snap('src/ collapsed')
  await snapFor(400)

  // re-expand
  console.log('[record] re-expanding src/…')
  await srcItem.click()
  await snapFor(600)

  // ── Act 5: click components/ to expand ──────────────────────────────────
  console.log('[record] expanding components/…')
  const compItem = page.locator('[data-type="item"][aria-label="components"]')
  await compItem.click()
  await snapFor(600)
  await snap('components/ expanded — Button, Modal, App')
  await snapFor(600)

  // ── Act 6: right-click README.md for context menu ────────────────────────
  console.log('[record] right-clicking README.md…')
  const readmeItem = page.locator('[data-type="item"][aria-label="README.md"]')
  await readmeItem.click({ button: 'right' })
  await sleep(300)
  await snap('context menu — Rename / Copy path / Delete')
  await snapFor(1000)

  await page.keyboard.press('Escape')
  await snapFor(400)

  // ── Act 7: hover-reveal context menu button ──────────────────────────────
  console.log('[record] hovering over a file to show action button…')
  const buttonItem = page.locator('[data-type="item"][aria-label="Button.tsx"]')
  if (await buttonItem.isVisible().catch(() => false)) {
    await buttonItem.hover()
    await snapFor(600)
  }

  // ── Act 8: search ────────────────────────────────────────────────────────
  console.log('[record] typing in search…')
  const searchInput = page.locator('input[placeholder*="Search" i], input[type="search"]').first()
  if (await searchInput.isVisible().catch(() => false)) {
    await searchInput.click()
    await sleep(200)

    // Type "utils" char by char for a typing effect
    for (const ch of 'utils') {
      await page.keyboard.type(ch)
      await snap()
      await sleep(80)
    }
    await snapFor(600)
    await snap('search: "utils" — only utils files visible')
    await snapFor(800)

    // clear and type "ts"
    await searchInput.fill('')
    await page.keyboard.type('Button')
    await snapFor(600)
    await snap('search: "Button" — Button.tsx highlighted')
    await snapFor(600)

    await page.keyboard.press('Escape')
    await snapFor(400)
  } else {
    console.log('  [warn] search input not visible')
    await snapFor(800)
  }

  // ── Act 9: drag preview (just hover drag) ────────────────────────────────
  console.log('[record] drag demonstration…')
  const mainTsItem = page.locator('[data-type="item"][aria-label="main.ts"]')
  if (await mainTsItem.isVisible().catch(() => false)) {
    // Simulate drag start (hover near the item)
    const mainBox = await mainTsItem.boundingBox()
    if (mainBox) {
      await page.mouse.move(mainBox.x + mainBox.width / 2, mainBox.y + mainBox.height / 2)
      await page.mouse.down()
      await snapFor(400)
      await page.mouse.move(mainBox.x + mainBox.width / 2, mainBox.y + 60, { steps: 10 })
      await snapFor(400)
      await snap('drag in progress')
      await page.mouse.up()
      await snapFor(300)
    }
  }

  // ── Act 10: hold on final state ──────────────────────────────────────────
  console.log('[record] holding on final state…')
  await snapFor(1000)

  await browser.close()
  console.log(`[record] captured ${frames.length} frames`)

  try { gmuxd.kill() } catch { /* ok */ }

  // 5. Encode GIF
  fs.mkdirSync(path.dirname(OUT_GIF), { recursive: true })
  await encodeGif(frames, W, H, OUT_GIF, 100)

  console.log(`\n✓ done: ${OUT_GIF}`)
}

main().catch(e => { console.error(e); process.exit(1) })

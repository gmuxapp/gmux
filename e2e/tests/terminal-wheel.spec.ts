import { expect, test, type Page } from '@playwright/test'
import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { apiGet, gotoSession, openApp, pollUntil } from '../helpers'

const ROOT = resolve(__dirname, '../..')
const STATE_FILE = join(tmpdir(), 'gmux-e2e-state.json')

type Scroll = { viewportY: number, baseY: number }

async function scroll(page: Page): Promise<Scroll> {
  return page.evaluate(() => {
    const buffer = (window as any).__gmuxTerm.buffer.active
    return { viewportY: buffer.viewportY, baseY: buffer.baseY }
  })
}

async function wheelSamples(page: Page, count: number): Promise<number[]> {
  const box = await page.locator('.xterm').boundingBox()
  if (!box) throw new Error('xterm has no bounding box')
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
  const samples: number[] = [(await scroll(page)).viewportY]
  for (let i = 0; i < count; i++) {
    await page.mouse.wheel(0, -120)
    samples.push((await scroll(page)).viewportY)
    await page.waitForTimeout(60)
  }
  return samples
}

async function startBurstSession(): Promise<{ process: ChildProcess, sessionId: string, trigger: string, stop: string, dir: string, env: NodeJS.ProcessEnv }> {
  const state = JSON.parse(readFileSync(STATE_FILE, 'utf8')) as { tmpDir: string, token: string }
  const before = await apiGet<{ data: Array<{ id: string }> }>('/v1/sessions')
  const existingIds = new Set(before.body.data.map(item => item.id))
  const workspace = process.env.GMUX_TEST_WORKSPACE!
  const dir = join(workspace, `wheel-${Date.now()}-${Math.random().toString(16).slice(2)}`)
  mkdirSync(dir, { recursive: true })
  const trigger = join(dir, 'trigger')
  const stop = join(dir, 'stop')
  const script = join(dir, 'gen.sh')
  writeFileSync(script, `#!/usr/bin/env bash
for i in $(seq 1 400); do printf 'seed-line-%04d\\r\\n' "$i"; done
while [ ! -e "${trigger}" ]; do sleep 0.01; done
i=0
while [ ! -e "${stop}" ]; do
  printf '\\033[?2026h'
  for j in $(seq 1 5); do printf 'burst-%04d-%d\\r\\n' "$i" "$j"; done
  printf '\\033[?2026l'
  i=$((i + 1))
  sleep 0.03
done
while true; do sleep 60; done
`)
  const env = {
    ...process.env,
    GMUX_SOCKET_DIR: join(state.tmpDir, 'sockets'),
    GMUXD_TOKEN: state.token,
    HOME: join(state.tmpDir, 'home'),
    XDG_CONFIG_HOME: join(state.tmpDir, 'config'),
    XDG_STATE_HOME: join(state.tmpDir, 'state'),
  }
  const child = spawn(join(ROOT, 'bin/gmux'), ['--', 'bash', script], {
    cwd: workspace,
    env,
    stdio: ['ignore', 'pipe', 'pipe'],
    detached: true,
  })
  const sessionId = await pollUntil(async () => {
    const response = await apiGet<{ data: Array<{ id: string, alive: boolean }> }>('/v1/sessions')
    return response.body.data.find(item => item.alive && !existingIds.has(item.id))?.id
  }, { timeoutMs: 10_000, description: 'burst session' })
  return { process: child, sessionId, trigger, stop, dir, env }
}

function stopBurstSession(session: { process: ChildProcess, sessionId: string, stop: string, dir: string, env: NodeJS.ProcessEnv }): void {
  writeFileSync(session.stop, '')
  try { execFileSync(join(ROOT, 'bin/gmux'), ['kill', session.sessionId], { env: session.env }) } catch { /* already exited */ }
  if (session.process.pid) {
    try { process.kill(-session.process.pid, 'SIGTERM') } catch { /* already exited */ }
  }
  rmSync(session.dir, { recursive: true, force: true })
}

test.describe.serial('terminal wheel under live synchronized output', () => {
  let burst: Awaited<ReturnType<typeof startBurstSession>>

  test.beforeEach(async ({ page }) => {
    burst = await startBurstSession()
    await openApp(page)
    await gotoSession(page, burst.sessionId)
    await page.waitForFunction(() => {
      const buffer = (window as any).__gmuxTerm?.buffer.active
      return buffer && buffer.baseY > 300
    }, undefined, { timeout: 15_000 }).catch(async error => {
      throw new Error(`${error.message}; cli=${burst.sessionId}; state=${JSON.stringify(await scroll(page))}; url=${page.url()}`)
    })
    await page.evaluate(() => (window as any).__gmuxTerm.scrollToBottom())
  })

  test.afterEach(() => { if (burst) stopBurstSession(burst) })

  test('wheel-up escapes bottom during 30 Hz BSU/ESU bursts', async ({ page }) => {
    const before = await scroll(page)
    writeFileSync(burst.trigger, '')
    const samples = await wheelSamples(page, 25)
    const after = await scroll(page)
    expect(after.baseY).toBeGreaterThan(before.baseY)
    expect(samples[0] - samples.at(-1)!).toBeGreaterThan(40)
  })

  test('an anchored viewport is not dragged and further wheel input moves it', async ({ page }) => {
    const initial = await wheelSamples(page, 12)
    expect(initial[0] - initial.at(-1)!).toBeGreaterThan(20)
    const beforeLoad = await scroll(page)
    writeFileSync(burst.trigger, '')
    await page.waitForTimeout(300)
    const beforeMore = await scroll(page)
    expect(beforeMore.baseY).toBeGreaterThan(beforeLoad.baseY)
    expect(Math.abs(beforeMore.viewportY - beforeLoad.viewportY)).toBeLessThanOrEqual(3)
    const more = await wheelSamples(page, 10)
    expect(more[0] - more.at(-1)!).toBeGreaterThan(15)
  })

  test('bare ED3 restores an anchored viewport after redraw', async ({ page }) => {
    await wheelSamples(page, 10)
    const redraw = '\x1b[2J\x1b[H\x1b[3J' + Array.from({ length: 100 }, (_, i) => `redraw-${i}\r\n`).join('')
    await page.evaluate((encoded) => (window as any).__gmuxInject(encoded), Buffer.from(redraw).toString('base64'))
    await page.waitForTimeout(200)
    const after = await scroll(page)
    expect(after.baseY).toBeGreaterThan(0)
    expect(after.viewportY).toBeGreaterThan(0)
  })
})

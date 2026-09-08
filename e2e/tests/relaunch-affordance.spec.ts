import { test, expect, type Page } from '@playwright/test'
import { spawn, type ChildProcess } from 'child_process'
import * as fs from 'fs'
import * as net from 'net'
import * as os from 'os'
import * as path from 'path'
import { apiFetch, gotoSession, openApp, pollUntil } from '../helpers'

/**
 * The affordance must be truthful: if the UI offers a relaunch, the daemon
 * must perform it. This is the regression test for the reported bug, where a
 * dead shell session was advertised as resumable and the click came back
 * "sessioncoord: resume spawn for …: runner spawn: session … is not
 * resumable" — because presentation derived resumability from "dead + has a
 * command" while the spawner only ever resolved an agent *resume* command.
 *
 * Both halves of the bug are covered, because the peer path is the one the
 * user actually hit:
 *
 *  1. a dead shell owned by this daemon, and
 *  2. a dead shell owned by a peer daemon this spec starts, where the click
 *     must be forwarded to the owner and executed there.
 *
 * The assertion in both cases is the biconditional: the button is shown, it
 * says "Rerun" (not "Resume" — there is no conversation to resume), and
 * clicking it brings the session back alive on the daemon that owns it.
 */

const ROOT = path.resolve(__dirname, '..', '..')
const GMUX = path.join(ROOT, 'bin', 'gmux')
const GMUXD = path.join(ROOT, 'bin', 'gmuxd')
const STATE_FILE = path.join(os.tmpdir(), 'gmux-e2e-state.json')

type WireSession = {
  id: string
  alive: boolean
  cwd?: string
  peer?: string
  adapter?: string
  resumable?: boolean
  relaunch?: string
}

async function sessions(port?: string | number, token?: string): Promise<WireSession[]> {
  const { body } = await apiFetch<{ data: WireSession[] }>('GET', '/v1/sessions', { port, token })
  return body?.data ?? []
}

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

/** Fail loudly, with the daemon's own words, the moment a relaunch is
 * refused — rather than letting the liveness poll time out anonymously. */
async function expectNoFailureToast(page: Page): Promise<void> {
  const toast = page.locator('.toast-error .toast-message', { hasText: 'failed' }).first()
  if (await toast.count() > 0) {
    throw new Error(`relaunch refused: ${await toast.textContent()}`)
  }
}

let env: Record<string, string>
let workspace = ''
let localDeadId = ''
const spawned: ChildProcess[] = []

// The peer daemon: its own state/config/socket dirs and HOME, exactly like
// the harness's isolation contract (e2e/AGENTS.md) — a second daemon that
// leaked into the operator's environment would be no better than the first.
let peerProc: ChildProcess | undefined
let peerPort = 0
let peerToken = ''
let peerName = ''
let peerWorkspace = ''
let peerDeadId = ''
let peerTmp = ''

test.describe.configure({ mode: 'serial' })

test.beforeAll(async () => {
  test.setTimeout(90_000)
  const state = JSON.parse(fs.readFileSync(STATE_FILE, 'utf-8')) as { tmpDir: string }
  workspace = process.env.GMUX_TEST_WORKSPACE!
  env = {
    PATH: process.env.PATH || '',
    HOME: process.env.GMUX_TEST_HOME!,
    TERM: 'xterm-256color',
    GMUX_SOCKET_DIR: path.join(state.tmpDir, 'sockets'),
    GMUXD_TOKEN: process.env.GMUX_TEST_TOKEN || '',
    XDG_CONFIG_HOME: path.join(state.tmpDir, 'config'),
    XDG_STATE_HOME: path.join(state.tmpDir, 'state'),
  }

  // ── a dead shell owned by the harness daemon ──
  const marker = 'local-relaunch-probe'
  // Sleeps briefly so both the initial exit and the relaunch are observable:
  // a command that exits instantly would relaunch and die between polls.
  const proc = spawn(GMUX, ['--', 'bash', '-c', `echo ${marker}; sleep 5`], {
    env, cwd: workspace, stdio: ['ignore', 'pipe', 'pipe'], detached: true,
  })
  spawned.push(proc)
  const dead = await pollUntil(async () =>
    (await sessions()).find(s => !s.alive && s.cwd === workspace && s.adapter === 'shell'),
  { timeoutMs: 20_000, intervalMs: 200, description: 'local shell session exited' })
  localDeadId = dead.id

  // ── a dead shell owned by a peer daemon ──
  peerTmp = fs.mkdtempSync(path.join(os.tmpdir(), 'gmux-e2e-peer-'))
  peerWorkspace = path.join(peerTmp, 'workspace')
  const peerHome = path.join(peerTmp, 'home')
  for (const d of ['sockets', 'config/gmux', 'state', 'home', 'workspace']) {
    fs.mkdirSync(path.join(peerTmp, d), { recursive: true })
  }
  peerPort = await freePort()
  // Must be 64 hex chars (authtoken.validateFormat), and distinct from the
  // harness token so a mis-addressed request fails loudly.
  peerToken = 'beef'.padEnd(64, '0')
  fs.writeFileSync(
    path.join(peerTmp, 'config', 'gmux', 'host.toml'),
    [`port = ${peerPort}`, '', '[discovery]', 'devcontainers = false', ''].join('\n'),
  )
  peerProc = spawn(GMUXD, ['run'], {
    env: {
      PATH: process.env.PATH || '',
      HOME: peerHome,
      TERM: 'xterm-256color',
      GMUX_SOCKET_DIR: path.join(peerTmp, 'sockets'),
      GMUXD_TOKEN: peerToken,
      XDG_CONFIG_HOME: path.join(peerTmp, 'config'),
      XDG_STATE_HOME: path.join(peerTmp, 'state'),
    },
    stdio: ['ignore', 'pipe', 'pipe'],
    detached: true,
  })
  let peerLog = ''
  peerProc.stderr?.on('data', (d: Buffer) => { peerLog += String(d) })
  peerProc.stdout?.on('data', (d: Buffer) => { peerLog += String(d) })
  await pollUntil(async () => {
    try {
      return (await apiFetch('GET', '/v1/health', { port: peerPort, token: peerToken })).status === 200
    } catch {
      return false // daemon still binding
    }
  }, { timeoutMs: 20_000, intervalMs: 200, description: `peer daemon healthy (log: ${peerLog})` })

  // The peer's session needs a project on the peer for the UI to route to it.
  await apiFetch('PUT', '/v1/projects', {
    port: peerPort, token: peerToken,
    body: { items: [{ slug: 'peer-project', match: [{ path: peerWorkspace }] }] },
  })
  await apiFetch('POST', '/v1/launch', {
    port: peerPort, token: peerToken,
    body: { cwd: peerWorkspace, command: ['bash', '-c', 'echo peer-relaunch-probe; sleep 5'] },
  })
  const peerDead = await pollUntil(async () =>
    (await sessions(peerPort, peerToken)).find(s => !s.alive && s.cwd === peerWorkspace),
  { timeoutMs: 20_000, intervalMs: 200, description: 'peer shell session exited' })
  peerDeadId = peerDead.id

  const registered = await apiFetch<{ peer?: { Name: string } }>('POST', '/v1/peers', {
    body: { URL: `http://127.0.0.1:${peerPort}`, Token: peerToken },
  })
  expect(registered.status, 'peer registration').toBe(200)
  peerName = registered.body?.peer?.Name ?? ''
  expect(peerName).not.toBe('')
})

test.afterAll(async () => {
  // Restore the shared daemon: no peer, no extra sessions. Later specs
  // assume the harness's single-session world.
  for (const id of [localDeadId, peerDeadId ? `${peerDeadId}@${peerName}` : '']) {
    if (!id) continue
    await apiFetch('POST', `/v1/sessions/${id}/kill`).catch(() => {})
  }
  await new Promise(r => setTimeout(r, 300))
  if (localDeadId) await apiFetch('POST', `/v1/sessions/${localDeadId}/dismiss`).catch(() => {})
  if (peerName) await apiFetch('DELETE', `/v1/peers/${peerName}`).catch(() => {})
  for (const proc of spawned) {
    if (proc.pid) { try { process.kill(-proc.pid, 'SIGKILL') } catch { /* gone */ } }
  }
  if (peerProc?.pid) { try { process.kill(-peerProc.pid, 'SIGKILL') } catch { /* gone */ } }
  if (peerTmp) fs.rmSync(peerTmp, { recursive: true, force: true })
})

test('a dead local shell is offered Rerun, and Rerun works', async ({ page }) => {
  const row = (await sessions()).find(s => s.id === localDeadId)!
  expect(row.resumable, 'dead shell stays visible in the sidebar').toBe(true)
  // The daemon names the verb: a shell has no conversation to resume.
  expect(row.relaunch).toBe('rerun')

  await openApp(page)
  await gotoSession(page, localDeadId)

  const button = page.locator('.replay-actions .btn-primary')
  await expect(button).toBeVisible()
  await expect(button).toHaveText('Rerun')

  await button.click()
  // The failure mode this replaces is a toast carrying the daemon's refusal.
  // Watch for it *while* waiting for liveness, so a regression reports the
  // actual message instead of an anonymous poll timeout.
  await pollUntil(async () => {
    await expectNoFailureToast(page)
    return (await sessions()).find(s => s.id === localDeadId)?.alive
  }, { timeoutMs: 15_000, intervalMs: 200, description: 'local shell relaunched' })
})

test('a dead peer shell is offered Rerun, and Rerun runs on the owning daemon', async ({ page }) => {
  const ref = `${peerDeadId}@${peerName}`
  const row = await pollUntil(async () => (await sessions()).find(s => s.id === ref),
    { timeoutMs: 20_000, intervalMs: 200, description: 'peer session projected locally' })
  expect(row.peer).toBe(peerName)
  // The verdict is the owner's, carried on the wire — not recomputed here.
  expect(row.relaunch).toBe('rerun')

  await openApp(page)
  await gotoSession(page, ref)

  const button = page.locator('.replay-actions .btn-primary')
  await expect(button).toBeVisible()
  await expect(button).toHaveText('Rerun')

  await button.click()
  // Assert on the *owning* daemon: the request must be forwarded, not run
  // locally against a store row that doesn't exist here.
  await pollUntil(async () => {
    await expectNoFailureToast(page)
    return (await sessions(peerPort, peerToken)).find(s => s.id === peerDeadId)?.alive
  }, { timeoutMs: 15_000, intervalMs: 200, description: 'peer shell relaunched on its owner' })
})

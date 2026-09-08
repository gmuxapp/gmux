import { test, expect } from '@playwright/test'
import { spawn, spawnSync, type ChildProcess } from 'child_process'
import * as fs from 'fs'
import * as net from 'net'
import * as os from 'os'
import * as path from 'path'
import { gotoSession, openApp, pollUntil } from '../helpers'

/**
 * Promote to root for a **peer** session, end to end through two real
 * daemons.
 *
 * Promote is reparent-to-null (single-axis families), and for a peer session
 * that is a same-peer mutation: the session never leaves its own daemon, only
 * its parent pointer changes. The viewer must therefore offer the verb and
 * forward it to the owning daemon — never apply it to the local projection,
 * and never refuse it as if it were cross-peer.
 *
 * The spec runs a second, disposable daemon (the "spoke") in its own tmpdir,
 * state dir, HOME and port, peers the e2e daemon (the "hub") to it, and builds
 * the family on the spoke: stub `claude` parent + shell child inheriting
 * GMUX_SESSION_ID. Every assertion about the mutation is read from the
 * SPOKE's own API, which is the authority for its sessions' parentage.
 */

const ROOT = path.resolve(__dirname, '..', '..')
const GMUX = path.join(ROOT, 'bin', 'gmux')
const GMUXD = path.join(ROOT, 'bin', 'gmuxd')
const SPOKE_TOKEN = 'ab'.padEnd(64, '0')

type WireSession = {
  id: string
  alive: boolean
  peer?: string
  cwd?: string
  title?: string
  adapter?: string
  parent_session_id?: string
  launched_from_session_id?: string
  semantic_agent?: boolean
}

function hub(): { base: string; headers: Record<string, string> } {
  const port = process.env.GMUXD_TEST_PORT
  const token = process.env.GMUX_TEST_TOKEN
  if (!port || !token) throw new Error('global-setup did not run')
  return { base: `http://127.0.0.1:${port}`, headers: { Authorization: `Bearer ${token}` } }
}

async function request(
  target: { base: string; headers: Record<string, string> },
  method: string,
  pathname: string,
  body?: unknown,
): Promise<{ status: number; body: any }> {
  const resp = await fetch(`${target.base}${pathname}`, {
    method,
    headers: body === undefined ? target.headers : { ...target.headers, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  let parsed: any
  try { parsed = await resp.json() } catch { /* non-JSON */ }
  return { status: resp.status, body: parsed }
}

let spokePort = 0
let spokeProc: ChildProcess | undefined
let spokeTmp = ''
let spokeEnv: Record<string, string> = {}
let hubEnv: Record<string, string> = {}
const spokeProcs: ChildProcess[] = []

let peerName = ''
let parentId = ''
let childId = ''
let childTitle = ''

function spoke(): { base: string; headers: Record<string, string> } {
  return { base: `http://127.0.0.1:${spokePort}`, headers: { Authorization: `Bearer ${SPOKE_TOKEN}` } }
}

async function spokeSessions(): Promise<WireSession[]> {
  const { body } = await request(spoke(), 'GET', '/v1/sessions')
  return (body?.data ?? []) as WireSession[]
}

async function hubSessions(): Promise<WireSession[]> {
  const { body } = await request(hub(), 'GET', '/v1/sessions')
  return (body?.data ?? []) as WireSession[]
}

/** The authority for the family edge: the daemon that owns the session. */
async function spokeParentOf(id: string): Promise<string | undefined> {
  return (await spokeSessions()).find(s => s.id === id)?.parent_session_id
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

test.describe.configure({ mode: 'serial' })

test.beforeAll(async () => {
  test.setTimeout(120_000)
  spokeTmp = fs.mkdtempSync(path.join(os.tmpdir(), 'gmux-e2e-spoke-'))
  spokePort = await freePort()
  const dirs = {
    sockets: path.join(spokeTmp, 'sockets'),
    config: path.join(spokeTmp, 'config', 'gmux'),
    state: path.join(spokeTmp, 'state'),
    home: path.join(spokeTmp, 'home'),
    workspace: path.join(spokeTmp, 'workspace'),
    stub: path.join(spokeTmp, 'stub-bin'),
  }
  for (const dir of Object.values(dirs)) fs.mkdirSync(dir, { recursive: true })
  fs.writeFileSync(path.join(dirs.config, 'host.toml'),
    `port = ${spokePort}\n\n[discovery]\ndevcontainers = false\n\n[tailscale]\nenabled = false\n`)
  // Same trick as the local promote spec: the adapter registry resolves on
  // the command basename, so a `claude` stub is a semantic agent.
  fs.writeFileSync(path.join(dirs.stub, 'claude'), '#!/bin/sh\necho stub agent ready\nexec sleep 600\n')
  fs.chmodSync(path.join(dirs.stub, 'claude'), 0o755)

  spokeEnv = {
    PATH: `${dirs.stub}:${process.env.PATH || ''}`,
    HOME: dirs.home,
    TERM: 'xterm-256color',
    GMUX_SOCKET_DIR: dirs.sockets,
    GMUXD_TOKEN: SPOKE_TOKEN,
    XDG_CONFIG_HOME: path.join(spokeTmp, 'config'),
    XDG_STATE_HOME: dirs.state,
  }
  // The hub's env, for driving the CLI against the e2e daemon.
  hubEnv = {
    PATH: process.env.PATH || '',
    HOME: process.env.GMUX_TEST_HOME!,
    TERM: 'xterm-256color',
    GMUX_SOCKET_DIR: path.join(JSON.parse(
      fs.readFileSync(path.join(os.tmpdir(), 'gmux-e2e-state.json'), 'utf-8')).tmpDir, 'sockets'),
    GMUXD_TOKEN: process.env.GMUX_TEST_TOKEN!,
    XDG_CONFIG_HOME: path.join(JSON.parse(
      fs.readFileSync(path.join(os.tmpdir(), 'gmux-e2e-state.json'), 'utf-8')).tmpDir, 'config'),
    XDG_STATE_HOME: path.join(JSON.parse(
      fs.readFileSync(path.join(os.tmpdir(), 'gmux-e2e-state.json'), 'utf-8')).tmpDir, 'state'),
  }

  spokeProc = spawn(GMUXD, ['run'], { env: spokeEnv, stdio: ['ignore', 'pipe', 'pipe'], detached: true })
  spokeProc.stderr?.on('data', (d: Buffer) => { if (process.env.DEBUG) process.stderr.write(`[spoke] ${d}`) })
  await pollUntil(async () => {
    try { return (await fetch(`http://127.0.0.1:${spokePort}/v1/health`, { headers: spoke().headers })).ok }
    catch { return false }
  }, { timeoutMs: 20_000, description: 'spoke daemon healthy' })

  // The spoke owns its project catalog; the hub will reference it by slug.
  expect((await request(spoke(), 'PUT', '/v1/projects',
    { items: [{ slug: 'spoke-project', match: [{ path: dirs.workspace }] }] })).status).toBe(200)

  spokeProcs.push(spawn(GMUX, ['--', 'claude'],
    { env: spokeEnv, cwd: dirs.workspace, stdio: ['ignore', 'pipe', 'pipe'], detached: true }))
  const parent = await pollUntil(async () =>
    (await spokeSessions()).find(s => s.alive && s.adapter === 'claude'),
  { timeoutMs: 20_000, description: 'spoke stub claude registered' })
  parentId = parent.id
  expect(parent.semantic_agent, 'stub claude parent must be a semantic agent').toBe(true)

  spokeProcs.push(spawn(GMUX, ['--', 'sh', '-c', 'echo peer child ready; sleep 600'],
    { env: { ...spokeEnv, GMUX_SESSION_ID: parentId }, cwd: dirs.workspace, stdio: ['ignore', 'pipe', 'pipe'], detached: true }))
  const child = await pollUntil(async () =>
    (await spokeSessions()).find(s => s.alive && s.parent_session_id === parentId),
  { timeoutMs: 20_000, description: 'spoke family child registered' })
  childId = child.id
  childTitle = child.title || 'sh'

  // Peer the hub to the spoke. The daemon assigns the peer name.
  const added = await request(hub(), 'POST', '/v1/peers',
    { url: `http://127.0.0.1:${spokePort}`, token: SPOKE_TOKEN })
  expect(added.status).toBe(200)
  peerName = added.body?.peer?.Name
  expect(peerName, 'peer name assigned by the hub').toBeTruthy()

  // Reference the spoke's project so its rows have a hub-side folder — the
  // same thing the Settings "From other hosts" list does. Without it the
  // promoted row would have nowhere to live and the menu would (correctly)
  // block the verb.
  expect((await request(hub(), 'PUT', '/v1/projects', {
    items: [
      { slug: 'test-project', match: [{ path: process.env.GMUX_TEST_WORKSPACE! }] },
      { slug: 'spoke-project', peer: peerName },
    ],
  })).status).toBe(200)

  // Wait for the hub's projection: namespaced ids, namespaced family edges.
  await pollUntil(async () => {
    const row = (await hubSessions()).find(s => s.id === `${childId}@${peerName}`)
    return row?.parent_session_id === `${parentId}@${peerName}`
      && row?.launched_from_session_id === `${parentId}@${peerName}`
  }, { timeoutMs: 20_000, description: 'hub projects the peer family with namespaced edges' })
})

test.afterAll(async () => {
  // Restore the hub's seeded catalog and drop the peer, so sibling specs see
  // the daemon they expect.
  await request(hub(), 'PUT', '/v1/projects',
    { items: [{ slug: 'test-project', match: [{ path: process.env.GMUX_TEST_WORKSPACE! }] }] }).catch(() => {})
  if (peerName) await request(hub(), 'DELETE', `/v1/peers/${peerName}`).catch(() => {})
  for (const proc of spokeProcs) {
    if (proc.pid) { try { process.kill(-proc.pid, 'SIGKILL') } catch { /* gone */ } }
  }
  if (spokeProc?.pid) { try { process.kill(-spokeProc.pid, 'SIGKILL') } catch { /* gone */ } }
  await new Promise(r => setTimeout(r, 300))
  if (spokeTmp) fs.rmSync(spokeTmp, { recursive: true, force: true })
})

test('the ⋮ menu promotes a peer child and returns it to its family', async ({ page }) => {
  test.setTimeout(90_000)
  await openApp(page)
  await gotoSession(page, `${childId}@${peerName}`)

  const childRow = page.locator('.session-item')
    .filter({ has: page.locator(`.session-title:text-is("${childTitle}")`) })
  await expect(page.locator('.family-slot.selected')).toHaveCount(1)
  await expect(childRow).toHaveCount(0)

  await page.locator('.session-menu-trigger').click()
  const item = page.locator('.session-menu-promotion')
  await expect(item).toContainText('Promote to root')
  await expect(item).not.toHaveAttribute('aria-disabled', 'true')
  await item.click()

  // Authoritative check on the OWNING daemon: the edge was cleared there,
  // not faked in the viewer's projection.
  await expect.poll(() => spokeParentOf(childId), { timeout: 15_000 }).toBeUndefined()
  await expect(page.locator('[data-promotion-status]')).toHaveText('Promoted to root.')
  await expect(childRow).toHaveCount(1)
  await expect(page.locator('.xterm')).toBeVisible()

  // And back: Return to family resolves the peer's launch provenance, which
  // only works because the projection namespaces it with the parent edge.
  await page.locator('.session-menu-trigger').click()
  const back = page.locator('.session-menu-promotion')
  await expect(back).toContainText('Return to family')
  await back.click()
  await expect.poll(() => spokeParentOf(childId), { timeout: 15_000 }).toBe(parentId)
  await expect(page.locator('[data-promotion-status]')).toHaveText('Returned to family.')
})

test('the CLI promotes and reparents a peer child through the local daemon', async () => {
  test.setTimeout(60_000)
  const gmux = (...args: string[]) => spawnSync(GMUX, args, { env: hubEnv, encoding: 'utf-8' })

  const promote = gmux('promote', `${childId}@${peerName}`)
  expect(promote.stderr + promote.stdout).toContain('promoted')
  expect(promote.status).toBe(0)
  await expect.poll(() => spokeParentOf(childId), { timeout: 15_000 }).toBeUndefined()

  // Same-peer reparent is legitimate under one-daemon families: the parent
  // reference is translated into the owner's namespace by the daemon.
  const reparent = gmux('reparent', `${childId}@${peerName}`, `${parentId}@${peerName}`)
  expect(reparent.stderr + reparent.stdout).toContain('reparented')
  expect(reparent.status).toBe(0)
  await expect.poll(() => spokeParentOf(childId), { timeout: 15_000 }).toBe(parentId)
})

test('a family that would span two daemons is still refused, in both directions', async () => {
  test.setTimeout(60_000)
  const localId = process.env.GMUX_TEST_SESSION_ID!

  // Peer child under a local parent.
  const inbound = await request(hub(), 'POST', `/v1/sessions/${childId}@${peerName}/reparent`,
    { parent_session_id: localId })
  expect(inbound.status).toBe(400)
  expect(inbound.body?.error?.code).toBe('cross_peer')

  // Local child under a peer parent.
  const outbound = await request(hub(), 'POST', `/v1/sessions/${localId}/reparent`,
    { parent_session_id: `${parentId}@${peerName}` })
  expect(outbound.status).toBe(400)
  expect(outbound.body?.error?.code).toBe('cross_peer')

  // Neither daemon moved anything.
  expect(await spokeParentOf(childId)).toBe(parentId)
  expect((await hubSessions()).find(s => s.id === localId)?.parent_session_id).toBeUndefined()

  // The CLI refuses before any request leaves.
  const cli = spawnSync(GMUX, ['reparent', `${childId}@${peerName}`, localId], { env: hubEnv, encoding: 'utf-8' })
  expect(cli.status).toBe(1)
  expect(cli.stderr).toContain('cross-peer reparenting is not supported')
})

import { spawn, execSync, type ChildProcess } from 'child_process'
import * as fs from 'fs'
import * as net from 'net'
import * as os from 'os'
import * as path from 'path'
import type { FullConfig } from '@playwright/test'

const ROOT = path.resolve(__dirname, '..')
const GMUXD = path.join(ROOT, 'bin', 'gmuxd')
const GMUX = path.join(ROOT, 'bin', 'gmux')

// Shared state file so teardown can find the PIDs and tmpDir.
const STATE_FILE = path.join(os.tmpdir(), 'gmux-e2e-state.json')

/** Find a free port by briefly binding to :0. */
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

async function waitForHealth(port: number, token: string, timeoutMs = 15_000): Promise<void> {
  const start = Date.now()
  const headers = { Authorization: `Bearer ${token}` }
  while (Date.now() - start < timeoutMs) {
    try {
      const resp = await fetch(`http://127.0.0.1:${port}/v1/health`, { headers })
      if (resp.ok) return
    } catch { /* retry */ }
    await new Promise(r => setTimeout(r, 200))
  }
  throw new Error(`gmuxd did not become healthy on port ${port} within ${timeoutMs}ms`)
}

async function waitForSession(port: number, token: string, timeoutMs = 15_000): Promise<string> {
  const start = Date.now()
  const headers = { Authorization: `Bearer ${token}` }
  while (Date.now() - start < timeoutMs) {
    try {
      const resp = await fetch(`http://127.0.0.1:${port}/v1/sessions`, { headers })
      const body = await resp.json() as { data: Array<{ id: string; alive: boolean }> }
      const alive = body.data.filter(s => s.alive)
      if (alive.length > 0) return alive[0].id
    } catch { /* retry */ }
    await new Promise(r => setTimeout(r, 200))
  }
  throw new Error(`No alive session found on port ${port} within ${timeoutMs}ms`)
}

export default async function globalSetup(config: FullConfig) {
  // Build frontend + Go binaries so the embedded assets are always in sync
  // with the current source.  Runs unconditionally — both Vite and `go build`
  // are incremental, so a no-op rebuild finishes in seconds.
  //
  // Set E2E_SKIP_BUILD=1 to skip (e.g. in CI where builds are a separate job).
  if (!process.env.E2E_SKIP_BUILD) {
    console.log('\n[e2e] building gmux (E2E_SKIP_BUILD=1 to skip)…')
    execSync('./scripts/build.sh', { cwd: ROOT, stdio: 'inherit' })
  }

  const port = await freePort()
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'gmux-e2e-'))
  const socketDir = path.join(tmpDir, 'sockets')
  const configDir = path.join(tmpDir, 'config')
  const stateDir = path.join(tmpDir, 'state')
  fs.mkdirSync(socketDir)
  fs.mkdirSync(configDir)
  fs.mkdirSync(stateDir)

  // Write host.toml with the allocated port so gmuxd binds there instead of
  // its default 8790 (which may be in use by a dev instance on the host).
  fs.mkdirSync(path.join(configDir, 'gmux'))
  fs.writeFileSync(
    path.join(configDir, 'gmux', 'host.toml'),
    `port = ${port}\n`,
  )

  // Seed a known auth token so tests can authenticate without scraping the
  // generated token file. Must be >= 64 hex chars (authtoken.validateFormat).
  const testToken = 'e2e'.padEnd(64, '0')

  const env: Record<string, string> = {
    PATH: process.env.PATH || '',
    HOME: process.env.HOME || '',
    TERM: 'xterm-256color',
    GMUX_SOCKET_DIR: socketDir,
    GMUXD_TOKEN: testToken,
    XDG_CONFIG_HOME: configDir,
    XDG_STATE_HOME: stateDir,
  }

  const pids: number[] = []

  // Start gmuxd
  const gmuxd = spawn(GMUXD, ['start'], {
    env,
    stdio: ['ignore', 'pipe', 'pipe'],
    detached: true,
  })
  if (gmuxd.pid) pids.push(gmuxd.pid)

  gmuxd.stderr?.on('data', (d: Buffer) => {
    if (process.env.DEBUG) process.stderr.write(`[gmuxd] ${d}`)
  })
  gmuxd.on('exit', (code) => {
    if (process.env.DEBUG) console.error(`[gmuxd] exited with code ${code}`)
  })

  await waitForHealth(port, testToken)

  // Start a test shell session (non-interactive — no local terminal attach)
  const gmux = spawn(GMUX, ['bash', '-c', 'echo READY; while true; do sleep 60; done'], {
    env,
    stdio: ['ignore', 'pipe', 'pipe'],
    detached: true,
  })
  if (gmux.pid) pids.push(gmux.pid)

  gmux.stderr?.on('data', (d: Buffer) => {
    if (process.env.DEBUG) process.stderr.write(`[gmux] ${d}`)
  })

  // Wait for the session to appear in gmuxd
  const sessionId = await waitForSession(port, testToken)

  // Save state for teardown and for test config
  fs.writeFileSync(STATE_FILE, JSON.stringify({
    tmpDir,
    pids,
    sessionId,
    port,
    token: testToken,
  }))

  // Playwright reads baseURL from config, but config is evaluated before
  // globalSetup. So env vars from here propagate to the worker process that
  // runs the tests.
  process.env.GMUXD_TEST_PORT = String(port)
  process.env.GMUX_TEST_SESSION_ID = sessionId
  process.env.GMUX_TEST_TOKEN = testToken
}

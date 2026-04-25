import { spawn, execSync } from 'child_process'
import * as fs from 'fs'
import * as net from 'net'
import * as os from 'os'
import * as path from 'path'
import type { FullConfig } from '@playwright/test'
import { SMOKE_FIXTURES, writeFakeSession } from './fixtures'

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

/**
 * Wait for *our* test session to appear, matching by cwd.
 *
 * The daemon-isolation work in this same setup means there should
 * only ever be one alive session here. Matching by cwd is still the
 * right contract though: it asserts we got the session we spawned,
 * not whatever else might surface, so an isolation regression fails
 * loudly here instead of one of the spec files.
 */
async function waitForSession(port: number, token: string, expectCwd: string, timeoutMs = 15_000): Promise<string> {
  const start = Date.now()
  const headers = { Authorization: `Bearer ${token}` }
  while (Date.now() - start < timeoutMs) {
    try {
      const resp = await fetch(`http://127.0.0.1:${port}/v1/sessions`, { headers })
      const body = await resp.json() as { data: Array<{ id: string; alive: boolean; cwd?: string }> }
      const match = body.data.find(s => s.alive && s.cwd === expectCwd)
      if (match) return match.id
    } catch { /* retry */ }
    await new Promise(r => setTimeout(r, 200))
  }
  throw new Error(`No alive session with cwd=${expectCwd} found on port ${port} within ${timeoutMs}ms`)
}

export default async function globalSetup(_config: FullConfig) {
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
  // Workspace dir for the test session's cwd, so it matches the seeded
  // project rule below. Without a matching project, the test session
  // can't be reached via URL.
  const workspaceDir = path.join(tmpDir, 'workspace')
  fs.mkdirSync(socketDir)
  fs.mkdirSync(configDir)
  fs.mkdirSync(stateDir)
  fs.mkdirSync(workspaceDir)

  // Seed projects.json with a single project rule matching workspaceDir.
  // The test session is spawned with cwd=workspaceDir below, so it'll
  // be matched into this project and reachable at /test-project/shell/...
  // navigateToSession (called from the test helper) needs the session
  // to belong to *some* project to compute a URL.
  //
  // Path layout matches paths.StateDir(): $XDG_STATE_HOME/gmux/.
  const gmuxStateDir = path.join(stateDir, 'gmux')
  fs.mkdirSync(gmuxStateDir, { recursive: true })
  fs.writeFileSync(
    path.join(gmuxStateDir, 'projects.json'),
    JSON.stringify({
      version: 2,
      items: [
        { slug: 'test-project', match: [{ path: workspaceDir }] },
      ],
    }),
  )

  // Write host.toml with:
  //  - the allocated port (so gmuxd doesn't fight a dev instance on 8790)
  //  - devcontainer discovery disabled (so the test daemon doesn't
  //    enumerate the operator's running docker containers via the
  //    host's docker socket; that's the last non-HOME, non-socketdir
  //    path that can leak the operator's environment into the test).
  //  - tailscale stays off by default; mention it explicitly so a
  //    future default change doesn't silently re-enable it for tests.
  fs.mkdirSync(path.join(configDir, 'gmux'))
  fs.writeFileSync(
    path.join(configDir, 'gmux', 'host.toml'),
    [
      `port = ${port}`,
      ``,
      `[discovery]`,
      `devcontainers = false`,
      `tailscale = false`,
      ``,
      `[tailscale]`,
      `enabled = false`,
      ``,
    ].join('\n'),
  )

  // Seed a known auth token so tests can authenticate without scraping the
  // generated token file. Must be >= 64 hex chars (authtoken.validateFormat).
  const testToken = 'e2e'.padEnd(64, '0')

  // Isolation: a fake HOME under tmpDir prevents gmuxd's adapters
  // (pi, claude, codex, shell) from discovering the operator's real
  // session files via os.UserHomeDir(). Without this, the test
  // gmuxd's session list contains every pi/claude/codex conversation
  // the operator has on their machine. With it, gmuxd starts blank.
  const fakeHome = path.join(tmpDir, 'home')
  fs.mkdirSync(fakeHome)

  // Pre-seed smoke fixtures (one valid JSONL per adapter) before
  // gmuxd starts. The bootstrap scan picks them up and the smoke spec
  // asserts they're reachable at /v1/conversations/{kind}/{slug}. If a
  // fixture is invalid for its parser, the smoke spec fails first with
  // a clear "fixture didn't reach the index" signal, before the
  // discovery spec's tests run.
  for (const fixture of SMOKE_FIXTURES) {
    writeFakeSession(fakeHome, fixture)
  }

  const env: Record<string, string> = {
    PATH: process.env.PATH || '',
    HOME: fakeHome,
    TERM: 'xterm-256color',
    GMUX_SOCKET_DIR: socketDir,
    GMUXD_TOKEN: testToken,
    XDG_CONFIG_HOME: configDir,
    XDG_STATE_HOME: stateDir,
  }

  const pids: number[] = []

  // Start gmuxd in foreground mode (`run`, not `start`).
  //
  // `gmuxd start` daemonizes by re-execing itself, and on the way
  // strips every GMUX_* env var ("so the daemon doesn't inherit
  // session identity"). That's correct for production, but it means
  // GMUX_SOCKET_DIR and GMUXD_TOKEN never reach the actual daemon
  // process; the test gmuxd would then read /tmp/gmux-sessions and
  // surface the operator's real sessions as its own.
  //
  // `gmuxd run` doesn't strip the env, and we already detach the
  // process ourselves, so we get the foreground process tree we need
  // for both isolation and clean teardown by PID.
  const gmuxd = spawn(GMUXD, ['run'], {
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

  // Start a test shell session (non-interactive — no local terminal attach).
  // cwd=workspaceDir so the session is matched into the seeded project.
  const gmux = spawn(GMUX, ['bash', '-c', 'echo READY; while true; do sleep 60; done'], {
    env,
    cwd: workspaceDir,
    stdio: ['ignore', 'pipe', 'pipe'],
    detached: true,
  })
  if (gmux.pid) pids.push(gmux.pid)

  gmux.stderr?.on('data', (d: Buffer) => {
    if (process.env.DEBUG) process.stderr.write(`[gmux] ${d}`)
  })

  // Wait for the session to appear in gmuxd. Matched by cwd so we get
  // exactly the session we just spawned (see comment on waitForSession
  // for why this assertion still earns its keep after isolation).
  const sessionId = await waitForSession(port, testToken, workspaceDir)

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
  // Tests that write into adapter session roots (conversation
  // discovery, etc.) need to know where the daemon is looking for
  // session files. fakeHome is the daemon's HOME, so adapters resolve
  // their roots under it.
  process.env.GMUX_TEST_HOME = fakeHome
}

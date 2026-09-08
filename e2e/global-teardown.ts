import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'
import type { FullConfig } from '@playwright/test'

const STATE_FILE = path.join(os.tmpdir(), 'gmux-e2e-state.json')

interface E2EState {
  tmpDir: string
  pids: number[]
  port: string
  token: string
  socketDir?: string
}

/**
 * Ask the daemon to stop every session it still knows about.
 *
 * Runners are deliberately not children of gmuxd (ADR 0011: runner-owned
 * session state), so killing the process groups global-setup spawned reaches
 * the daemon and its one setup session only. Sessions a *test* launched
 * through `/v1/launch` — ticker scripts that never exit — outlive teardown as
 * orphaned `gmux __run` processes, one per launching test per suite run.
 *
 * Stopping them is the daemon's job, so ask it first; `sweepRunners` is what
 * guarantees the outcome.
 */
async function killTestSessions(state: E2EState): Promise<void> {
  let sessions: Array<{ id: string, alive: boolean }>
  try {
    const resp = await fetch(`http://127.0.0.1:${state.port}/v1/sessions`, {
      headers: { Authorization: `Bearer ${state.token}` },
    })
    if (!resp.ok) return
    const body = await resp.json() as { data?: Array<{ id: string, alive: boolean }> }
    sessions = (body.data ?? []).filter(s => s.alive)
  } catch {
    return // daemon already gone; nothing it can stop for us
  }
  for (const session of sessions) {
    try {
      await fetch(`http://127.0.0.1:${state.port}/v1/sessions/${session.id}/kill`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${state.token}` },
      })
    } catch { /* best effort: the sweep below is the backstop */ }
  }
}

/**
 * PIDs of every process holding this run's socket dir in its environment.
 *
 * Runners inherit `GMUX_SOCKET_DIR` from the test daemon and the directory is
 * a fresh mkdtemp per run, so this matches exactly the processes this suite
 * started (runners and their session children) and can never reach a runner
 * of the developer's own daemon.
 */
function runnersBySocketDir(socketDir: string): number[] {
  const found: number[] = []
  for (const entry of fs.readdirSync('/proc')) {
    const pid = Number(entry)
    if (!Number.isInteger(pid) || pid <= 0) continue
    let environ: string
    try {
      environ = fs.readFileSync(`/proc/${pid}/environ`, 'utf-8')
    } catch {
      continue // gone, or not ours to read
    }
    if (environ.split('\0').includes(`GMUX_SOCKET_DIR=${socketDir}`)) found.push(pid)
  }
  return found
}

/**
 * Guarantee that nothing this run started is still alive.
 *
 * The graceful path needs a live daemon, and one spec deliberately SIGTERMs
 * it (`z-terminal-disconnect`) — after which the daemon can no longer stop
 * anything, which is how a full-suite run used to orphan every
 * test-launched runner for the rest of the machine's uptime. This step
 * depends on nothing but /proc, so it holds in that case too.
 *
 * Linux-only (`/proc/<pid>/environ`); elsewhere the graceful path above is
 * all there is, and a suite run that kills the daemon can still leak.
 */
function sweepRunners(state: E2EState): void {
  if (!state.socketDir || process.platform !== 'linux') return
  const pids = runnersBySocketDir(state.socketDir)
  if (pids.length === 0) return
  console.log(`[e2e] sweeping ${pids.length} runner process(es) the daemon could no longer stop`)
  for (const pid of pids) {
    try {
      process.kill(pid, 'SIGKILL')
    } catch { /* already dead */ }
  }
}

export default async function globalTeardown(_config: FullConfig) {
  let state: E2EState
  try {
    state = JSON.parse(fs.readFileSync(STATE_FILE, 'utf-8'))
  } catch {
    return // no state file — nothing to clean up
  }

  // Stop test-launched sessions while the daemon is still up to do it.
  await killTestSessions(state)

  // Ask gmuxd to shut down gracefully
  try {
    await fetch(`http://127.0.0.1:${state.port}/v1/shutdown`, { method: 'POST' })
  } catch { /* already gone */ }

  // Kill all spawned processes
  for (const pid of state.pids) {
    try {
      // Kill the process group (negative PID) since we used detached: true
      process.kill(-pid, 'SIGTERM')
    } catch { /* already dead */ }
  }

  // Give them a moment to exit
  await new Promise(r => setTimeout(r, 500))

  // Force kill anything remaining
  for (const pid of state.pids) {
    try {
      process.kill(-pid, 'SIGKILL')
    } catch { /* already dead */ }
  }

  // Anything still holding this run's socket dir is a leak by definition.
  sweepRunners(state)

  // Clean up temp dir
  try {
    fs.rmSync(state.tmpDir, { recursive: true, force: true })
  } catch { /* best effort */ }

  // Clean up state file
  try {
    fs.unlinkSync(STATE_FILE)
  } catch { /* best effort */ }
}

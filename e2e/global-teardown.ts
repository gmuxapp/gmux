import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'
import type { FullConfig } from '@playwright/test'

const STATE_FILE = path.join(os.tmpdir(), 'gmux-e2e-state.json')

export default async function globalTeardown(_config: FullConfig) {
  let state: { tmpDir: string; pids: number[]; port: string }
  try {
    state = JSON.parse(fs.readFileSync(STATE_FILE, 'utf-8'))
  } catch {
    return // no state file — nothing to clean up
  }

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

  // Clean up temp dir
  try {
    fs.rmSync(state.tmpDir, { recursive: true, force: true })
  } catch { /* best effort */ }

  // Clean up state file
  try {
    fs.unlinkSync(STATE_FILE)
  } catch { /* best effort */ }
}

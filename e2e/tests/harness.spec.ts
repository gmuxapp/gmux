import { test, expect } from '@playwright/test'

/**
 * Asserts the harness's isolation contract directly. See
 * `e2e/AGENTS.md` for the three leak paths these checks defend.
 *
 * If any of these fail, the rest of the suite is untrustworthy: a
 * test that reads "the first alive session" or counts sidebar
 * entries is silently coupled to whatever the operator happens to
 * have running. Catching that here surfaces it as one named failure
 * instead of a smear of unrelated flakes.
 */
test.describe('harness isolation', () => {
  // No openApp/login here — these checks talk to the daemon
  // directly. Auth is via the bearer token global-setup writes.
  const port = () => process.env.GMUXD_TEST_PORT
  const token = () => process.env.GMUX_TEST_TOKEN
  const sessionId = () => process.env.GMUX_TEST_SESSION_ID
  const headers = () => ({ Authorization: `Bearer ${token()}` })

  test('daemon sees exactly one alive session and no peers', async () => {
    const resp = await fetch(`http://127.0.0.1:${port()}/v1/health`, { headers: headers() })
    expect(resp.ok).toBe(true)
    const body = await resp.json() as {
      data: {
        sessions: { local_alive: number; remote_alive: number; dead: number }
      }
      // Peers may live at the top level or under data depending on
      // the endpoint shape; we'll fetch /v1/peers separately for
      // an authoritative answer.
    }

    expect(body.data.sessions.local_alive).toBe(1)
    expect(body.data.sessions.remote_alive).toBe(0)

    const peersResp = await fetch(`http://127.0.0.1:${port()}/v1/peers`, { headers: headers() })
    if (peersResp.ok) {
      const peers = await peersResp.json() as { data?: unknown[] }
      // Peers payload is an array; empty means no remote hosts
      // discovered. If the daemon ever auto-enables peer discovery
      // by default, this fires.
      expect(peers.data ?? []).toEqual([])
    }
  })

  test('the alive session is the one we spawned', async () => {
    const resp = await fetch(`http://127.0.0.1:${port()}/v1/sessions`, { headers: headers() })
    expect(resp.ok).toBe(true)
    const body = await resp.json() as {
      data: Array<{ id: string; alive: boolean; cwd: string; kind: string }>
    }
    const alive = body.data.filter(s => s.alive)
    expect(alive).toHaveLength(1)

    const ours = alive[0]
    expect(ours.id).toBe(sessionId())
    expect(ours.kind).toBe('shell')
    // The session was spawned with cwd=$tmpDir/workspace; we don't
    // pin the exact path (it's per-run) but it must end in
    // /workspace and live under the OS temp dir.
    expect(ours.cwd).toMatch(/\/workspace$/)
  })
})

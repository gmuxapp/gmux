import { test, expect } from '@playwright/test'
import { openApp, spawnTestSession, readScrollbackFile } from '../helpers'

/**
 * Regression test for: "switching gmux sessions causes WS connection to
 * take ages to re-establish."
 *
 * Root cause: `openWs()` was called only after `fetchScrollback()` resolved,
 * serialising the TCP handshake behind a potentially large HTTP download and
 * `extractScrollbackContent` processing.
 *
 * Fix (a774dc5): prefetch and WS open in parallel. The WS must be in OPEN
 * state well before a slow scrollback fetch completes.
 */

const SLOW_PREFETCH_DELAY_MS = 3_000
const WS_OPEN_DEADLINE_MS    = 1_500   // WS must open faster than the slow delay

test.describe('session switch — WS connects in parallel with scrollback prefetch', () => {
  test('WS opens before a slow scrollback fetch completes', async ({ page }) => {
    // Spawn two sessions: "from" (the one we start on) and "to" (the one we
    // switch to). The "to" session gets some output so an on-disk scrollback
    // file exists (a real HTTP request will be made to /v1/sessions/<id>/scrollback).
    const fromSession = await spawnTestSession(
      ['bash', '-c', 'echo FROM-SESSION-READY; sleep 60'],
      { cwdName: `switch-from-${Date.now()}` },
    )
    const toSession = await spawnTestSession(
      ['bash', '-c', 'printf "line-%d\\n" $(seq 1 50); sleep 60'],
      { cwdName: `switch-to-${Date.now()}` },
    )

    try {
      // Wait until the "to" session has on-disk scrollback we can intercept.
      await expect.poll(
        () => readScrollbackFile(toSession.id) !== null,
        { timeout: 10_000, intervals: [200] },
      ).toBe(true)

      await openApp(page)

      // Navigate to the "from" session and wait for it to be fully connected.
      await page.waitForFunction((id) => {
        const navigate = (window as any).__gmuxNavigateToSession
        if (typeof navigate !== 'function') return false
        return navigate(id) === true
      }, fromSession.id, { timeout: 10_000 })
      await page.locator('.terminal-container canvas').waitFor({ state: 'visible', timeout: 5_000 })
      await page.waitForTimeout(1_500) // let the WS settle

      // Intercept the scrollback endpoint for the "to" session to add an
      // artificial delay, simulating a large on-disk file.
      const scrollbackUrl = `/v1/sessions/${encodeURIComponent(toSession.id)}/scrollback`
      let interceptResolve: (() => void) | null = null
      const interceptFired = new Promise<void>(r => { interceptResolve = r })

      await page.route(`**${scrollbackUrl}`, async (route) => {
        interceptResolve?.()
        interceptResolve = null
        // Hold the response for the full slow delay.
        await new Promise<void>(r => setTimeout(r, SLOW_PREFETCH_DELAY_MS))
        await route.continue()
      })

      // Install a WebSocket open-time trap before the navigation so we can
      // measure how long the browser takes to get an OPEN event.
      await page.evaluate(() => {
        ;(window as any).__gmuxWsOpenAt = null
        const OrigWS = window.WebSocket
        ;(window as any).__gmuxWsProxy = class extends OrigWS {
          constructor(...args: ConstructorParameters<typeof WebSocket>) {
            super(...args)
            this.addEventListener('open', () => {
              if ((window as any).__gmuxWsOpenAt === null) {
                ;(window as any).__gmuxWsOpenAt = Date.now()
              }
            })
          }
        }
        window.WebSocket = (window as any).__gmuxWsProxy
      })

      // Record the wall-clock start then switch to the "to" session.
      const switchStart = Date.now()

      await page.waitForFunction((id) => {
        const navigate = (window as any).__gmuxNavigateToSession
        if (typeof navigate !== 'function') return false
        return navigate(id) === true
      }, toSession.id, { timeout: 10_000 })

      // Wait for the intercept to confirm the browser requested the scrollback
      // (this means the WS + prefetch logic has started).
      await interceptFired

      // Poll until the WS open event fires, with a deadline tighter than the
      // slow prefetch delay. Failure here means the WS is blocked on the fetch.
      const wsOpenAt = await page.waitForFunction(() => {
        return (window as any).__gmuxWsOpenAt as number | null
      }, null, { timeout: WS_OPEN_DEADLINE_MS + 500 })

      const elapsed = await wsOpenAt.evaluate(v => v as number) - switchStart

      console.log(`[session-switch] WS open in ${elapsed}ms (slow prefetch delay: ${SLOW_PREFETCH_DELAY_MS}ms)`)

      expect(
        elapsed,
        `WS should open in <${WS_OPEN_DEADLINE_MS}ms even with a ${SLOW_PREFETCH_DELAY_MS}ms slow prefetch`,
      ).toBeLessThan(WS_OPEN_DEADLINE_MS)

      // After the slow prefetch completes, the terminal-loading overlay must
      // disappear and the terminal canvas must be visible.
      await expect(page.locator('.terminal-loading')).not.toBeVisible({ timeout: SLOW_PREFETCH_DELAY_MS + 3_000 })
      await expect(page.locator('.terminal-container canvas')).toBeVisible()
    } finally {
      fromSession.kill()
      toSession.kill()
    }
  })
})

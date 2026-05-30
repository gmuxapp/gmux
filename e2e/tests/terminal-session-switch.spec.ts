import { test, expect } from '@playwright/test'
import { openApp, spawnTestSession } from '../helpers'

/**
 * Regression test for: "switching gmux sessions causes WS connection to
 * take ages to re-establish."
 *
 * Original root cause (a774dc5): prefetch serialised the WS handshake behind
 * a large HTTP download. Fix: run prefetch and WS in parallel.
 *
 * Current architecture: live sessions skip the scrollback prefetch entirely —
 * they get full scrollback from the WS snapshot (renderScreen). The WS must
 * open immediately, and no scrollback HTTP request should be made.
 */

const WS_OPEN_DEADLINE_MS = 1_500

test.describe('session switch — WS connects immediately for live sessions', () => {
  test('WS opens quickly and no scrollback prefetch is requested', async ({ page }) => {
    const fromSession = await spawnTestSession(
      ['bash', '-c', 'echo FROM-SESSION-READY; sleep 60'],
      { cwdName: `switch-from-${Date.now()}` },
    )
    const toSession = await spawnTestSession(
      ['bash', '-c', 'printf "line-%d\\n" $(seq 1 50); sleep 60'],
      { cwdName: `switch-to-${Date.now()}` },
    )

    try {
      await openApp(page)

      // Navigate to the "from" session and let it settle.
      await page.waitForFunction((id) => {
        const navigate = (window as any).__gmuxNavigateToSession
        if (typeof navigate !== 'function') return false
        return navigate(id) === true
      }, fromSession.id, { timeout: 10_000 })
      await page.locator('.terminal-shell .terminal-container.wterm.focused').waitFor({ state: 'visible', timeout: 5_000 })
      await page.waitForTimeout(1_500)

      // Verify that the scrollback endpoint is NOT called for live sessions.
      const scrollbackUrl = `/v1/sessions/${encodeURIComponent(toSession.id)}/scrollback`
      let prefetchRequested = false
      await page.route(`**${scrollbackUrl}*`, async (route) => {
        prefetchRequested = true
        await route.continue()
      })

      // Install a WS open-time trap.
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

      const switchStart = Date.now()

      await page.waitForFunction((id) => {
        const navigate = (window as any).__gmuxNavigateToSession
        if (typeof navigate !== 'function') return false
        return navigate(id) === true
      }, toSession.id, { timeout: 10_000 })

      // WS must open quickly — no prefetch blocking it.
      const wsOpenAt = await page.waitForFunction(
        () => (window as any).__gmuxWsOpenAt as number | null,
        null,
        { timeout: WS_OPEN_DEADLINE_MS + 500 },
      )
      const elapsed = await wsOpenAt.evaluate(v => v as number) - switchStart
      console.log(`[session-switch] WS open in ${elapsed}ms`)

      expect(
        elapsed,
        `WS should open in <${WS_OPEN_DEADLINE_MS}ms`,
      ).toBeLessThan(WS_OPEN_DEADLINE_MS)

      // Terminal must finish loading.
      await expect(page.locator('.terminal-loading')).not.toBeVisible({ timeout: 5_000 })
      await expect(page.locator('.terminal-shell .terminal-container.wterm.focused')).toBeVisible()

      // No scrollback prefetch should have been made for a live session.
      expect(prefetchRequested, 'live sessions must not request the scrollback prefetch').toBe(false)
    } finally {
      fromSession.kill()
      toSession.kill()
    }
  })
})

import { test, expect } from '@playwright/test'
import { getTermState, isPillVisible, openApp, selectFirstSession } from '../helpers'

test.describe('terminal resize', () => {
  test.beforeEach(async ({ page }) => {
    await openApp(page)
    await selectFirstSession(page)
  })

  // NOTE: tests that need addInitScript before page load go outside this
  // describe block (below) so they can control navigation order.

  test('selecting a session claims: terminal fits viewport, no pill', async ({ page }) => {
    // After selectFirstSession the browser has claimed ownership. Terminal
    // should have a sensible size and the pill should be hidden.
    const state = await getTermState(page)
    expect(state.termCols).toBeGreaterThan(0)
    expect(state.termRows).toBeGreaterThan(0)
    expect(await isPillVisible(page)).toBe(false)
  })

  test('window resize updates terminal dimensions', async ({ page }) => {
    await page.setViewportSize({ width: 900, height: 600 })
    await page.waitForTimeout(1000)
    const small = await getTermState(page)

    await page.setViewportSize({ width: 1400, height: 900 })
    await page.waitForTimeout(1000)
    const large = await getTermState(page)

    // Larger viewport → more columns and rows.
    expect(large.termCols!).toBeGreaterThan(small.termCols!)
    expect(large.termRows!).toBeGreaterThan(small.termRows!)
    // Driving the whole time → no pill.
    expect(await isPillVisible(page)).toBe(false)
  })

  test('fresh connection claims at current viewport, ignoring server\'s prior PTY size', async ({ page }) => {
    // Shrink the viewport. Since this page is driving, the server's PTY
    // size follows us down to ~800x500.
    await page.setViewportSize({ width: 800, height: 500 })
    await page.waitForTimeout(1000)
    const small = await getTermState(page)

    // Now reconnect fresh with a LARGER viewport. The server's PTY is still
    // the smaller size. If claim-on-connect works, the new page's first WS
    // open will resize the PTY up to fit this viewport (not start passive).
    await page.goto('about:blank')
    await page.setViewportSize({ width: 1400, height: 900 })
    await openApp(page)
    await selectFirstSession(page)

    const large = await getTermState(page)
    expect(large.termCols!).toBeGreaterThan(small.termCols!)
    expect(large.termRows!).toBeGreaterThan(small.termRows!)
    // No pill: we claimed at the current viewport, server's PTY now matches.
    expect(await isPillVisible(page)).toBe(false)
  })
})

test.describe('terminal resize — reconnect', () => {
  test('reconnect after network blip does not re-claim', async ({ page, context }) => {
    // Instrument WS.send and expose a helper to force-close WebSockets,
    // since context.setOffline doesn't immediately sever WS connections.
    await page.addInitScript(() => {
      const origSend = WebSocket.prototype.send
      ;(window as any).__wsResizes = [] as string[]
      ;(window as any).__allWs = [] as WebSocket[]
      const origCtor = window.WebSocket
      ;(window as any).WebSocket = function (...args: ConstructorParameters<typeof WebSocket>) {
        const ws = new origCtor(...args)
        ;(window as any).__allWs.push(ws)
        return ws
      } as unknown as typeof WebSocket
      Object.assign((window as any).WebSocket, origCtor)
      ;(window as any).WebSocket.prototype = origCtor.prototype

      WebSocket.prototype.send = function (data: unknown) {
        if (typeof data === 'string' && data.includes('"type":"resize"')) {
          ;(window as any).__wsResizes.push(data)
        }
        return origSend.apply(this, [data as any])
      }
    })

    await openApp(page)
    await selectFirstSession(page)

    // Initial claim should have sent at least one resize.
    const initialCount = await page.evaluate(
      () => ((window as any).__wsResizes as string[]).length,
    )
    expect(initialCount).toBeGreaterThan(0)

    // Reset capture.
    await page.evaluate(() => { (window as any).__wsResizes = [] })

    // Force-close all WebSockets to trigger the disconnect path.
    await page.evaluate(() => {
      for (const ws of (window as any).__allWs as WebSocket[]) {
        if (ws.readyState === WebSocket.OPEN) ws.close()
      }
    })

    // Wait for the "Connection lost" pill to appear.
    await expect(page.locator('.terminal-disconnected-pill')).toBeVisible({ timeout: 10_000 })

    // WS auto-reconnect should fire and re-establish the connection.
    await expect(page.locator('.terminal-disconnected-pill')).not.toBeVisible({ timeout: 10_000 })
    // Extra settle time.
    await page.waitForTimeout(1000)

    // No resize messages should have been sent during reconnect.
    const reconnectCount = await page.evaluate(
      () => ((window as any).__wsResizes as string[]).length,
    )
    expect(reconnectCount).toBe(0)
  })
})

import { test, expect } from '@playwright/test'
import { getTermState, isPillVisible, openApp, gotoTestSession } from '../helpers'

test.describe('terminal resize', () => {
  test.beforeEach(async ({ page }) => {
    await openApp(page)
    await gotoTestSession(page)
  })

  // NOTE: tests that need addInitScript before page load go outside this
  // describe block (below) so they can control navigation order.

  test('selecting a session claims: terminal fits viewport, no pill', async ({ page }) => {
    // After gotoTestSession the browser has claimed ownership. Terminal
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
    await gotoTestSession(page)

    const large = await getTermState(page)
    expect(large.termCols!).toBeGreaterThan(small.termCols!)
    expect(large.termRows!).toBeGreaterThan(small.termRows!)
    // No pill: we claimed at the current viewport, server's PTY now matches.
    expect(await isPillVisible(page)).toBe(false)
  })
})

test.describe('terminal resize — reconnect', () => {
  test('does not yank a scrolled-up reader until reconnect replay completes', async ({ page }) => {
    await page.addInitScript(() => {
      ;(window as any).__allWs = [] as WebSocket[]
      ;(window as any).__blockTerminalReconnect = false
      const OriginalWebSocket = window.WebSocket
      ;(window as any).WebSocket = function (...args: ConstructorParameters<typeof WebSocket>) {
        const original = String(args[0])
        const url = (window as any).__blockTerminalReconnect && original.includes('/ws/')
          ? 'ws://127.0.0.1:1/unavailable'
          : original
        const ws = new OriginalWebSocket(url, ...(args.slice(1) as any))
        ;(window as any).__allWs.push(ws)
        return ws
      } as unknown as typeof WebSocket
      Object.assign((window as any).WebSocket, OriginalWebSocket)
      ;(window as any).WebSocket.prototype = OriginalWebSocket.prototype
    })

    await openApp(page)
    await gotoTestSession(page)
    const payload = Array.from({ length: 200 }, (_, i) => `reconnect-seed-${i}\r\n`).join('')
    await page.evaluate((encoded) => (window as any).__gmuxInject(encoded), Buffer.from(payload).toString('base64'))
    await page.waitForTimeout(200)
    const box = await page.locator('.xterm').boundingBox()
    if (!box) throw new Error('xterm has no bounding box')
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
    for (let i = 0; i < 5; i++) await page.mouse.wheel(0, -120)
    const before = await page.evaluate(() => {
      const buffer = (window as any).__gmuxTerm.buffer.active
      return { viewportY: buffer.viewportY, baseY: buffer.baseY }
    })
    expect(before.viewportY).toBeLessThan(before.baseY)

    await page.evaluate(() => {
      ;(window as any).__blockTerminalReconnect = true
      for (const ws of (window as any).__allWs as WebSocket[]) {
        if (ws.readyState === WebSocket.OPEN) ws.close()
      }
    })
    await expect(page.locator('.terminal-disconnected-pill')).toBeVisible({ timeout: 10_000 })
    await page.waitForTimeout(800)
    const during = await page.evaluate(() => {
      const buffer = (window as any).__gmuxTerm.buffer.active
      return { viewportY: buffer.viewportY, baseY: buffer.baseY }
    })
    expect(during.viewportY).toBeLessThan(during.baseY)
    expect(during.viewportY).toBe(before.viewportY)

    await page.evaluate(() => { (window as any).__blockTerminalReconnect = false })
    await expect(page.locator('.terminal-disconnected-pill')).not.toBeVisible({ timeout: 10_000 })
    await page.waitForTimeout(1000)
    const after = await page.evaluate(() => {
      const buffer = (window as any).__gmuxTerm.buffer.active
      return { viewportY: buffer.viewportY, baseY: buffer.baseY }
    })
    expect(after.viewportY).toBe(after.baseY)
  })

  test('reconnect after network blip does not re-claim', async ({ page }) => {
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
    await gotoTestSession(page)

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

import { test, expect, type Page, type Response } from '@playwright/test'
import { apiFetch, gotoTestSession, openApp } from '../helpers'

/**
 * gzip on the phone path, observed from a real browser.
 *
 * The daemon compresses its network responses (host.toml `[http]
 * compression`, default on). What must hold from the browser's side:
 *
 *  - the SSE stream and the JS bundle arrive `Content-Encoding: gzip`;
 *  - `EventSource` still receives events one at a time, promptly — the
 *    compressor is sync-flushed per event, so a launch shows up on the
 *    stream within the daemon's coalescing window, not "whenever the
 *    deflate buffer fills";
 *  - the stream stays OPEN (readyState 1) with no error events, which is
 *    what the SSE supervisor's readyState/staleness checks observe;
 *  - the terminal WebSocket (an Upgrade, bypassing the middleware) still
 *    attaches and streams frames.
 */

type ProbeSource = { readyState: number; __events: Array<{ type: string; at: number }>; __errors: number[] }

async function installEventSourceProbe(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const Native = window.EventSource
    const sources: ProbeSource[] = []
    ;(window as any).__gmuxEventSources = sources
    ;(window as any).EventSource = class extends Native {
      __events: Array<{ type: string; at: number }> = []
      __errors: number[] = []
      constructor(url: string | URL, config?: EventSourceInitDict) {
        super(url, config)
        sources.push(this as unknown as ProbeSource)
        this.addEventListener('error', () => this.__errors.push(this.readyState))
        for (const type of ['snapshot.sessions.begin', 'snapshot.sessions.batch', 'snapshot.sessions.ready', 'snapshot.world', 'session-activity']) {
          this.addEventListener(type, () => this.__events.push({ type, at: performance.now() }))
        }
      }
    }
  })
}

test.describe('wire compression', () => {
  test('SSE and bundle are gzip-encoded; events still arrive individually and promptly', async ({ page }) => {
    await installEventSourceProbe(page)
    const responses: Response[] = []
    page.on('response', r => { responses.push(r) })

    await openApp(page)
    await page.waitForFunction(() => (window as any).__gmuxEventSources?.[0]?.readyState === 1)
    await page.waitForFunction(() => (window as any).__gmuxEventSources[0].__events.some((e: any) => e.type === 'snapshot.sessions.ready'))

    const sse = responses.find(r => r.url().includes('/v1/events'))
    expect(sse, 'SSE response observed').toBeTruthy()
    const sseHeaders = await sse!.allHeaders()
    expect(sseHeaders['content-encoding']).toBe('gzip')
    expect(sseHeaders['content-type']).toBe('text/event-stream')
    expect(sseHeaders['vary']).toContain('Accept-Encoding')
    expect(sseHeaders['content-length']).toBeUndefined()

    const bundle = responses.find(r => /\/assets\/index-.*\.js$/.test(r.url()))
    expect(bundle, 'JS bundle response observed').toBeTruthy()
    const bundleHeaders = await bundle!.allHeaders()
    expect(bundleHeaders['content-encoding']).toBe('gzip')
    expect(bundleHeaders['etag']).toMatch(/-gz"$/)
    expect(bundleHeaders['vary']).toContain('Accept-Encoding')
    expect(bundleHeaders['cache-control']).toContain('immutable')
    // The precompressed sibling declares its length; the browser reports
    // the encoded transfer size, which must be a fraction of the source.
    const encoded = Number(bundleHeaders['content-length'])
    const decoded = (await bundle!.body()).length
    expect(encoded).toBeGreaterThan(0)
    expect(encoded * 2).toBeLessThan(decoded)

    // Per-event delivery: a launch outside the browser must reach the open
    // stream well inside the 10 s stall bound and the supervisor's staleness
    // window. The daemon coalesces broadcasts for ~250 ms; anything beyond a
    // couple of seconds would mean the compressor held the event.
    const before = await page.evaluate(() => (window as any).__gmuxEventSources[0].__events.length)
    const t0 = Date.now()
    const launched = await apiFetch<{ ok: boolean }>('POST', '/v1/launch', {
      body: { cwd: process.env.GMUX_TEST_WORKSPACE, command: ['bash', '-c', 'echo gzip-probe; sleep 30'] },
    })
    expect(launched.status).toBe(200)
    await page.waitForFunction(
      n => (window as any).__gmuxEventSources[0].__events.length > n,
      before,
      { timeout: 3_000 },
    )
    const latencyMs = Date.now() - t0
    console.log(`launch → SSE event on a gzip stream: ${latencyMs} ms`)
    expect(latencyMs).toBeLessThan(3_000)

    // The stream stayed OPEN throughout, with no error events (#522 supervisor view).
    for (let i = 0; i < 6; i++) {
      expect(await page.evaluate(() => (window as any).__gmuxEventSources[0].readyState)).toBe(1)
      await page.waitForTimeout(250)
    }
    const probe = await page.evaluate(() => {
      const s = (window as any).__gmuxEventSources
      return { sources: s.length, errors: s[0].__errors, events: s[0].__events.length }
    })
    expect(probe.sources).toBe(1)
    expect(probe.errors).toEqual([])
    expect(probe.events).toBeGreaterThan(before)
  })

  test('terminal WebSocket attach is unaffected by the middleware', async ({ page }) => {
    const frames: number[] = []
    let wsUrl = ''
    page.on('websocket', ws => {
      wsUrl = ws.url()
      ws.on('framereceived', f => { frames.push(typeof f.payload === 'string' ? f.payload.length : f.payload.byteLength) })
    })
    await openApp(page)
    await gotoTestSession(page)
    expect(wsUrl).toContain('/ws/')
    expect(frames.length, 'received frames on the PTY WebSocket').toBeGreaterThan(0)
    // The terminal rendered replayed output, i.e. frames decoded as plain
    // WebSocket data (no compression extension involved).
    await expect(page.locator('.xterm')).toBeVisible()
  })
})

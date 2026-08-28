import { expect, test, type Page } from '@playwright/test'
import { apiGet, gotoSession, openApp, pollUntil } from '../helpers'

async function installGeometryProbe(page: Page) {
  await page.addInitScript(() => {
    ;(window as any).__checkpointGeometryEvents = []
    ;(window as any).__geometrySockets = []
    const Native = window.WebSocket
    const Wrapped = function (...args: ConstructorParameters<typeof WebSocket>) {
      const ws = new Native(...args)
      ;(window as any).__geometrySockets.push(ws)
      const nativeSend = ws.send.bind(ws)
      ws.send = ((data: string | ArrayBufferLike | Blob | ArrayBufferView) => {
        if (typeof data === 'string') {
          try {
            const msg = JSON.parse(data)
            if (msg.type === 'resize') (window as any).__checkpointGeometryEvents.push({ kind: 'server-resize', cols: msg.cols, rows: msg.rows })
          } catch {}
        }
        nativeSend(data as never)
      }) as typeof ws.send
      let productionHandler: ((this: WebSocket, ev: MessageEvent) => unknown) | null = null
      Object.defineProperty(ws, 'onmessage', {
        configurable: true,
        get: () => productionHandler,
        set: handler => { productionHandler = handler },
      })
      ws.addEventListener('message', event => {
        if (typeof event.data === 'string') {
          try {
            const msg = JSON.parse(event.data)
            if (msg.type === 'terminal_checkpoint') {
              ;(window as any).__checkpointGeometryEvents.push({ kind: 'metadata', cols: msg.cols, rows: msg.rows })
            }
          } catch {}
        }
        setTimeout(() => productionHandler?.call(ws, event), typeof event.data === 'string' ? 0 : 200)
      })
      return ws
    } as unknown as typeof WebSocket
    Object.assign(Wrapped, Native)
    Wrapped.prototype = Native.prototype
    ;(window as any).WebSocket = Wrapped

    const seen = new WeakSet<object>()
    setInterval(() => {
      const term = (window as any).__gmuxTerm
      if (!term || seen.has(term)) return
      seen.add(term)
      const resize = term.resize.bind(term)
      term.resize = (cols: number, rows: number) => {
        ;(window as any).__checkpointGeometryEvents.push({ kind: 'resize', cols, rows })
        resize(cols, rows)
      }
      const write = term.write.bind(term)
      term.write = (data: string | Uint8Array, callback?: () => void) => {
        ;(window as any).__checkpointGeometryEvents.push({ kind: 'write', cols: term.cols, rows: term.rows })
        write(data, callback)
      }
    }, 1)
  })
}

async function launchIdle(page: Page): Promise<string> {
  const before = await apiGet<{ data: Array<{ id: string }> }>('/v1/sessions')
  const known = new Set(before.body.data.map(s => s.id))
  const cwd = process.env.GMUX_TEST_WORKSPACE!
  const launched = await page.evaluate(async ({ cwd }) => {
    const r = await fetch('/v1/launch', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ command: ['bash', '-c', "printf 'REATTACH-GEOMETRY\\r\\n'; while :; do sleep 1; done"], cwd }),
    })
    return { ok: r.ok, text: await r.text() }
  }, { cwd })
  expect(launched.ok, launched.text).toBe(true)
  return pollUntil(async () => {
    const r = await apiGet<{ data: Array<{ id: string, alive: boolean }> }>('/v1/sessions')
    return r.body.data.find(s => s.alive && !known.has(s.id))?.id
  }, { timeoutMs: 10_000, description: 'reattach geometry session' })
}

test('reattach replays at the runner-declared shrunken geometry', async ({ page }) => {
  await installGeometryProbe(page)
  await openApp(page)
  const id = await launchIdle(page)
  await gotoSession(page, id)
  await expect.poll(() => page.evaluate(() => (window as any).__checkpointGeometryEvents
    .find((event: any) => event.kind === 'metadata')?.cols)).toBeGreaterThan(1)
  await expect(page.locator('.terminal-loading')).not.toBeVisible({ timeout: 5_000 })
  await page.waitForTimeout(500)
  const before = 100
  await page.evaluate(({ sessionId, cols }) => {
    const ws = ((window as any).__geometrySockets as WebSocket[]).find(socket =>
      socket.url.includes(`/ws/${sessionId}`) && socket.readyState === WebSocket.OPEN)
    ws?.send(JSON.stringify({ type: 'resize', cols, rows: 25 }))
  }, { sessionId: id, cols: before })
  await page.waitForTimeout(200)
  await page.evaluate((sessionId) => {
    for (const ws of (window as any).__geometrySockets as WebSocket[]) {
      if (ws.url.includes(`/ws/${sessionId}`) && ws.readyState === WebSocket.OPEN) ws.close()
    }
  }, id)
  await page.waitForTimeout(50)
  await page.goto('about:blank')
  await page.waitForTimeout(1500)
  await openApp(page)
  await page.evaluate(() => { (window as any).__checkpointGeometryEvents = [] })
  await gotoSession(page, id)

  await expect.poll(() => page.evaluate(() => (window as any).__checkpointGeometryEvents
    .some((event: any) => event.kind === 'write'))).toBe(true)
  const events = await page.evaluate(() => (window as any).__checkpointGeometryEvents) as Array<any>
  const metadataIndex = events.findIndex(event => event.kind === 'metadata')
  const writeIndex = events.findIndex(event => event.kind === 'write')
  const declared = events[metadataIndex]
  const barrierIndex = events.findIndex((event, index) => index > metadataIndex && index < writeIndex
    && event.kind === 'resize' && event.cols === declared.cols && event.rows === declared.rows)

  // The runner metadata is the source of truth for the replay barrier.
  expect(declared.cols).toBeGreaterThan(1)
  expect(barrierIndex, JSON.stringify(events)).toBeGreaterThan(metadataIndex)
  expect(events[writeIndex]).toMatchObject({ cols: declared.cols, rows: declared.rows })
})

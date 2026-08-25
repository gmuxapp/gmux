import { test, expect, type Page } from '@playwright/test'
import { openApp, gotoSession } from '../helpers'

async function installAttachProbe(page: Page, checkpointDelay = 200) {
  await page.addInitScript((delay) => {
    ;(window as any).__checkpointRows = null
    ;(window as any).__attachEvents = [] as Array<Record<string, unknown>>
    ;(window as any).__inputSends = [] as string[]
    const Native = window.WebSocket
    const Wrapped = function (...args: ConstructorParameters<typeof WebSocket>) {
      const ws = new Native(...args)
      let productionHandler: ((this: WebSocket, ev: MessageEvent) => unknown) | null = null
      Object.defineProperty(ws, 'onmessage', {
        configurable: true,
        get: () => productionHandler,
        set: handler => { productionHandler = handler },
      })
      ws.addEventListener('message', event => {
        if (typeof event.data === 'string') {
          try {
            const message = JSON.parse(event.data)
            if (message.type === 'terminal_checkpoint') (window as any).__checkpointRows = message.rows
          } catch {}
        }
        // Hold the binary checkpoint long enough to install xterm probes and
        // deterministically attempt input during the replay phase.
        setTimeout(() => productionHandler?.call(ws, event), typeof event.data === 'string' ? 0 : delay)
      })
      const nativeSend = ws.send.bind(ws)
      ws.send = ((data: string | ArrayBufferLike | Blob | ArrayBufferView) => {
        if (ArrayBuffer.isView(data)) {
          ;(window as any).__inputSends.push(new TextDecoder().decode(data as ArrayBufferView<ArrayBuffer>).toString())
        }
        nativeSend(data as never)
      }) as typeof ws.send
      return ws
    } as unknown as typeof WebSocket
    Object.assign(Wrapped, Native)
    Wrapped.prototype = Native.prototype
    ;(window as any).WebSocket = Wrapped

    const probe = setInterval(() => {
      const term = (window as any).__gmuxTerm
      if (!term || (term as any).__attachProbeInstalled) return
      ;(term as any).__attachProbeInstalled = true
      const resize = term.resize.bind(term)
      term.resize = (cols: number, rows: number) => {
        ;(window as any).__attachEvents.push({ kind: 'resize', cols, rows, at: performance.now() })
        resize(cols, rows)
      }
      const write = term.write.bind(term)
      term.write = (data: string | Uint8Array, callback?: () => void) => {
        const text = typeof data === 'string' ? data : new TextDecoder().decode(data)
        const status = /STATUS ROWS=(\d+) COLS=(\d+)/.exec(text)
        const liveRedraw = status !== null && Number(status[1]) > 25
          && Number(status[1]) === term.rows && Number(status[2]) === term.cols
        ;(window as any).__attachEvents.push({ kind: 'write', cols: term.cols, rows: term.rows,
          liveRedraw, at: performance.now() })
        write(data, callback)
      }
      clearInterval(probe)
    }, 1)
  }, checkpointDelay)
}

async function launchRowsProgram(page: Page, suppressWinch: boolean) {
  await openApp(page)
  const cwd = process.env.GMUX_TEST_WORKSPACE!
  const marker = suppressWinch ? 'CASE-SUPPRESSED' : 'CASE-CONVERGES'
  const draw = `draw() { read -r rows cols < <(stty size); frame=$( { printf '\\033[2J\\033[H'; for i in $(seq 1 $((rows-1))); do printf '\\033[%d;1HROW-%03d' "$i" "$i"; done; printf '\\033[1;10H${marker}'; printf '\\033[%d;1HSTATUS ROWS=%d COLS=%d' "$rows" "$rows" "$cols"; } ); printf '%s' "$frame"; }`
  const command = suppressWinch
    ? `stty -echo; ${draw}; draw; trap '' WINCH; while :; do sleep 1; done`
    : `stty -echo; ${draw}; draw; last_rows=$rows; last_cols=$cols; while :; do read -r now_rows now_cols < <(stty size); if [ "$now_rows:$now_cols" != "$last_rows:$last_cols" ]; then draw; break; fi; sleep 0.05; done; while :; do sleep 1; done`
  const launched = await page.evaluate(async ({ command, cwd }) => {
    const r = await fetch('/v1/launch', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ command: ['bash', '-c', command], cwd }) })
    return { ok: r.ok, text: await r.text() }
  }, { command, cwd })
  expect(launched.ok, launched.text).toBe(true)
  const link = page.locator('a').filter({ hasText: marker }).first()
  await link.waitFor({ state: 'visible', timeout: 10_000 })
  const id = (await link.getAttribute('href'))!.split('~').pop()!
  await gotoSession(page, id)
}

function terminalState() {
  const t = (window as any).__gmuxTerm
  const lines = Array.from({ length: t.rows }, (_, i) => t.buffer.active.getLine(i)?.translateToString(true) ?? '') as string[]
  const statusRow = lines.findIndex(line => line.includes('STATUS ROWS='))
  return { checkpointRows: (window as any).__checkpointRows as number, termRows: t.rows as number, termCols: t.cols as number,
    statusRow, blankTail: lines.slice(statusRow + 1).filter(line => line === '').length, bottom: lines.at(-1),
    loading: document.querySelector('.terminal-loading') !== null }
}

test('checkpoint geometry and input are fenced before viewport claim', async ({ page }) => {
  await installAttachProbe(page, 3000)
  await launchRowsProgram(page, true)
  await page.locator('.xterm-helper-textarea').focus()
  // Prove the delayed binary has not reached production before exercising
  // the input fence; otherwise these assertions would be post-claim vacuous.
  expect(await page.evaluate(() => (window as any).__attachEvents)).toEqual([])
  await page.keyboard.type('X')
  await expect.poll(() => page.evaluate(() => (window as any).__attachEvents.length)).toBeGreaterThan(1)
  await expect.poll(() => page.evaluate(terminalState)).toMatchObject({ checkpointRows: 25, statusRow: 24 })
  await page.waitForTimeout(100)

  const evidence = await page.evaluate(() => ({ events: (window as any).__attachEvents, sends: (window as any).__inputSends }))
  const checkpointWrite = evidence.events.find((event: any) => event.kind === 'write') as any
  const checkpointResize = evidence.events.findIndex((event: any) => event.kind === 'resize' && event.cols === 80 && event.rows === 25)
  const writeIndex = evidence.events.indexOf(checkpointWrite)
  const claimResize = evidence.events.findIndex((event: any, i: number) => i > writeIndex && event.kind === 'resize' && event.rows > 25)
  expect(checkpointResize, JSON.stringify(evidence.events)).toBeGreaterThanOrEqual(0)
  expect(checkpointResize).toBeLessThan(writeIndex)
  expect(checkpointWrite).toMatchObject({ cols: 80, rows: 25 })
  expect(claimResize).toBeGreaterThan(writeIndex)
  expect(evidence.sends).not.toContain('X')

  // A non-redrawing child is released by the bounded fallback, rather than
  // leaving an input-fenced loading screen forever. Input is accepted only
  // after the local claim has committed.
  await expect(page.locator('.terminal-loading')).not.toBeVisible({ timeout: 2_000 })
  await page.keyboard.type('Y')
  await expect.poll(() => page.evaluate(() => (window as any).__inputSends)).toContain('Y')
})

test('real post-claim redraw parses only after the claim resize', async ({ page }) => {
  await installAttachProbe(page)
  await launchRowsProgram(page, false)
  await expect.poll(() => page.evaluate(terminalState), { timeout: 10_000 }).toMatchObject({ loading: false, blankTail: 0 })
  const observed = await page.evaluate(terminalState)
  expect(observed.bottom).toBe(`STATUS ROWS=${observed.termRows} COLS=${observed.termCols}`)
  expect(observed.statusRow).toBe(observed.termRows - 1)

  const events = await page.evaluate(() => (window as any).__attachEvents) as Array<any>
  const checkpointResize = events.findIndex(event => event.kind === 'resize' && event.cols === 80 && event.rows === 25)
  const checkpointWrite = events.findIndex(event => event.kind === 'write' && event.cols === 80 && event.rows === 25)
  const claimResize = events.findIndex(event => event.kind === 'resize' && event.cols === observed.termCols && event.rows === observed.termRows)
  const liveWrite = events.findIndex(event => event.kind === 'write' && event.liveRedraw === true)
  expect(checkpointResize).toBeGreaterThanOrEqual(0)
  expect(checkpointResize).toBeLessThan(checkpointWrite)
  expect(claimResize).toBeGreaterThan(checkpointWrite)
  expect(liveWrite).toBeGreaterThan(claimResize)
  // Busy-fence claim ordering itself is pinned deterministically by the
  // enqueueResizeThenMany unit test; this E2E uses only the real child's
  // WINCH redraw and proves production checkpoint/claim/live sequencing.
})

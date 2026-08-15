import { test, expect } from '@playwright/test'
import { gotoSession, openApp } from '../helpers'

const A_COMMAND = 'stty -echo; printf "\\033[44mA-BLUE\\033[0m\\r\\n"; for i in $(seq 1 40); do printf "A-SCROLL-%02d\\r\\n" "$i"; done; sleep 5; printf "\\033[?1h\\033[?1000h\\033[?1006h\\033[?2004h\\033[?1004h\\033[2;4r\\033[3;6H"; while true; do read -r line || exit; eval "$line"; done'
const B_COMMAND = 'printf "B-NORMAL\\r\\n"; while true; do read -r line || exit; eval "$line"; done'
const C_COMMAND = 'stty -echo; printf "C-NORMAL\\r\\n"; while true; do read -r line || exit; if [ "$line" = go ]; then printf "\\033[2;4r\\033[3;6H"; for i in $(seq 1 40); do printf "C-SCROLL-%02d\\r\\n" "$i"; done; printf "\\033[9;20H"; elif [ "$line" = marker ]; then printf "C-PARK-MARK"; elif [ "$line" = after ]; then printf "\\033[3;6H"; for i in $(seq 1 10); do printf "C-AFTER-%02d\\r\\n" "$i"; done; fi; done'

test('real A→B→A isolation and reconnect checkpoint', async ({ page }) => {
  test.setTimeout(90_000)
  const mark = (s: string) => console.log(`[switch] ${s}`)
  await page.addInitScript(() => {
    ;(window as any).__allWs = [] as WebSocket[]
    ;(window as any).__checkpointMetadata = [] as string[]
    ;(window as any).__blockTerminalReconnect = false
    ;(window as any).__stripBrowserQuery = false
    ;(window as any).__wsSent = [] as string[]
    const OriginalWebSocket = window.WebSocket
    ;(window as any).WebSocket = function (...args: ConstructorParameters<typeof WebSocket>) {
      const original = String(args[0])
      const url = (window as any).__blockTerminalReconnect && original.includes('/ws/')
        ? 'ws://127.0.0.1:1/unavailable'
        : (window as any).__stripBrowserQuery ? original.replace('?client=browser', '') : original
      const ws = new OriginalWebSocket(url, ...(args.slice(1) as any))
      const send = ws.send.bind(ws)
      ws.send = ((data: any) => {
        let bytes: Uint8Array
        if (typeof data === 'string') bytes = new TextEncoder().encode(data)
        else if (data instanceof ArrayBuffer) bytes = new Uint8Array(data)
        else if (ArrayBuffer.isView(data)) bytes = new Uint8Array(data.buffer, data.byteOffset, data.byteLength)
        else bytes = new Uint8Array()
        ;(window as any).__wsSent.push(Array.from(bytes).map(b => String.fromCharCode(b)).join(''))
        return send(data)
      }) as typeof ws.send
      ws.addEventListener('message', (event) => {
        if (typeof event.data === 'string' && event.data.includes('terminal_checkpoint')) {
          ;(window as any).__checkpointMetadata.push(event.data)
        }
      })
      ;(window as any).__allWs.push(ws)
      return ws
    } as unknown as typeof WebSocket
    Object.assign((window as any).WebSocket, OriginalWebSocket)
    ;(window as any).WebSocket.prototype = OriginalWebSocket.prototype
  })

  await openApp(page)
  const cwd = process.env.GMUX_TEST_WORKSPACE!
  const launch = async (command: string) => {
    await page.evaluate(async ({ command, cwd }) => {
      const response = await fetch('/v1/launch', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ command: ['bash', '-c', command], cwd }),
      })
      if (!response.ok) throw new Error(`launch failed: ${response.status}`)
    }, { command, cwd })
    const marker = command.includes('A-BLUE') ? 'A-BLUE' : command.includes('C-NORMAL') ? 'C-NORMAL' : 'B-NORMAL'
    const link = page.locator('a').filter({ hasText: marker }).first()
    await link.waitFor({ state: 'visible', timeout: 10_000 })
    const href = await link.getAttribute('href')
    return href!.split('~').pop()!
  }

  const aId = await launch(A_COMMAND)
  const bId = await launch(B_COMMAND)
  const cId = await launch(C_COMMAND)
  // Let the production SSE snapshot publish the newly launched sessions
  // before routing through the same navigation hook used by the suite.
  await page.waitForTimeout(1500)
  const gotoLocalSession = async (id: string) => {
    await gotoSession(page, id)
  }
  try {
    mark('A attach')
    await gotoLocalSession(aId)
    await page.waitForFunction(() => {
      const term = (window as any).__gmuxTerm
      const buffer = term?.buffer.active
      const text = Array.from({ length: (buffer?.baseY ?? 0) + (term?.rows ?? 0) }, (_, y) => buffer?.getLine(y)?.translateToString(true) ?? '').join('\n')
      return buffer?.type === 'normal' && text.includes('A-BLUE') && text.includes('A-SCROLL-40')
    })
    mark('A text')
    await page.waitForTimeout(2000)
    console.log(await page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      return { modes: term.modes, active: term.buffer.active.type, text: term.buffer.active.getLine(0)?.translateToString(true) }
    }))
    await page.screenshot({ path: '.memory/screenshots/terminal-switch-state-A-before.png' })
    mark('A modes')

    const aGeometry = await page.evaluate(() => ({ cols: (window as any).__gmuxTerm.cols, rows: (window as any).__gmuxTerm.rows }))
    await page.setViewportSize({ width: 900, height: 600 })
    mark('B attach')
    await gotoLocalSession(bId)
    const bGeometry = await page.evaluate(() => ({ cols: (window as any).__gmuxTerm.cols, rows: (window as any).__gmuxTerm.rows }))
    expect(bGeometry.cols).toBeLessThan(aGeometry.cols)
    expect(bGeometry.rows).toBeLessThan(aGeometry.rows)
    await page.waitForFunction(() => {
      const term = (window as any).__gmuxTerm
      const buffer = term.buffer.active
      const text = Array.from({ length: buffer.baseY + term.rows }, (_, y) => buffer.getLine(y)?.translateToString(true) ?? '').join('\n')
      return buffer.type === 'normal' && text.includes('B-NORMAL')
    })
    await expect.poll(() => page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      return {
        appCursor: term.modes.applicationCursorKeysMode,
        bracketedPaste: term.modes.bracketedPasteMode,
        mouse: term.modes.mouseTrackingMode,
        focus: term.modes.sendFocusMode,
      }
    })).toMatchObject({ appCursor: false, bracketedPaste: false, mouse: 'none', focus: false })
    // Establish B's alternate state only after the normal B checkpoint has
    // been rendered. The same-session reconnect and the later session switch
    // both use the browser metadata to select the target buffer.
    await page.locator('.xterm-helper-textarea').focus()
    await page.keyboard.type("printf '\\033[?1049h\\033[2J\\033[H\\033[44mB-ALT\\033[0m\\033[3;5H'")
    await page.keyboard.press('Enter')
    await page.waitForFunction(() => (window as any).__gmuxTerm.buffer.active.type === 'alternate'
      && (window as any).__gmuxTerm.buffer.active.getLine(0)?.translateToString(true).includes('B-ALT'))
    await expect.poll(() => page.evaluate(() => (window as any).__checkpointMetadata.length)).toBeGreaterThan(0)
    await page.screenshot({ path: '.memory/screenshots/terminal-switch-state-B.png' })
    mark('B active')

    // Reconnect while the TUI owns the alternate buffer, then exit it. The
    // saved normal screen must be B-NORMAL, not the previous A screen.
    mark('B reconnect start')
    await page.evaluate(() => {
      for (const ws of (window as any).__allWs as WebSocket[]) {
        if (ws.readyState === WebSocket.OPEN && ws.url.includes('/ws/')) ws.close()
      }
    })
    await expect(page.locator('.terminal-disconnected-pill')).toBeVisible({ timeout: 10_000 })
    await expect(page.locator('.terminal-disconnected-pill')).not.toBeVisible({ timeout: 10_000 })
    mark('B reconnect socket')
    await expect.poll(() => page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      const active = term.buffer.active
      return {
        active: active.type,
        activeText: active.getLine(0)?.translateToString(true),
        normalText: Array.from({ length: term.buffer.normal.baseY + term.rows }, (_, y) => term.buffer.normal.getLine(y)?.translateToString(true) ?? '').join('\n'),
      }
    }), { timeout: 10_000 }).toMatchObject({ active: 'alternate', activeText: expect.stringContaining('B-ALT') })
    expect(await page.evaluate(() => (window as any).__gmuxTerm.buffer.normal.getLine(0)?.translateToString(true))).not.toContain('A-BLUE')
    mark('B reconnect')

    // Same-session reconnect with legacy metadata absence is non-destructive:
    // the already-active alternate buffer is retained rather than guessed.
    await page.evaluate(() => {
      ;(window as any).__stripBrowserQuery = true
      for (const ws of (window as any).__allWs as WebSocket[]) {
        if (ws.readyState === WebSocket.OPEN && ws.url.includes('/ws/')) ws.close()
      }
    })
    await expect(page.locator('.terminal-disconnected-pill')).toBeVisible({ timeout: 10_000 })
    await expect(page.locator('.terminal-disconnected-pill')).not.toBeVisible({ timeout: 10_000 })
    await page.waitForFunction(() => (window as any).__gmuxTerm.buffer.active.type === 'alternate'
      && (window as any).__gmuxTerm.buffer.active.getLine(0)?.translateToString(true).includes('B-ALT'))
    mark('B legacy reconnect')
    await page.evaluate(() => { ;(window as any).__stripBrowserQuery = false })
    await page.locator('.xterm-helper-textarea').focus()
    await page.keyboard.type("printf '\\033[?1049l'")
    await page.keyboard.press('Enter')
    await page.waitForFunction(() => (window as any).__gmuxTerm.buffer.active.type === 'normal')
    expect(await page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      return Array.from({ length: term.buffer.normal.baseY + term.rows }, (_, y) => term.buffer.normal.getLine(y)?.translateToString(true) ?? '').join('\n')
    })).not.toContain('A-BLUE')
    await page.keyboard.type("printf '\\033[?1049h\\033[2J\\033[H\\033[44mB-ALT-SWITCH\\033[0m'")
    await page.keyboard.press('Enter')
    await page.waitForFunction(() => (window as any).__gmuxTerm.buffer.active.type === 'alternate'
      && (window as any).__gmuxTerm.buffer.active.getLine(0)?.translateToString(true).includes('B-ALT-SWITCH'))

    // Switch into an already-alternate session as a distinct transition. The
    // session reset must prevent A's normal modes, background, and scrollback
    // from becoming B's active screen; metadata selects B's alternate buffer.
    await gotoLocalSession(aId)
    await page.waitForFunction(() => {
      const term = (window as any).__gmuxTerm
      const buffer = term.buffer.active
      const text = Array.from({ length: buffer.baseY + term.rows }, (_, y) => buffer.getLine(y)?.translateToString(true) ?? '').join('\n')
      return buffer.type === 'normal' && text.includes('A-BLUE') && !text.includes('B-ALT')
    })
    await gotoLocalSession(bId)
    await page.waitForFunction(() => {
      const term = (window as any).__gmuxTerm
      const buffer = term.buffer.active
      const text = Array.from({ length: buffer.baseY + term.rows }, (_, y) => buffer.getLine(y)?.translateToString(true) ?? '').join('\n')
      return buffer.type === 'alternate' && text.includes('B-ALT-SWITCH') && !text.includes('A-BLUE')
    })
    await expect.poll(() => page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      return { appCursor: term.modes.applicationCursorKeysMode, bracketedPaste: term.modes.bracketedPasteMode, mouse: term.modes.mouseTrackingMode, focus: term.modes.sendFocusMode }
    })).toMatchObject({ appCursor: false, bracketedPaste: false, mouse: 'none', focus: false })
    await page.evaluate(() => { ;(window as any).__wsSent = [] })
    await page.locator('.xterm-helper-textarea').focus()
    await page.keyboard.press('ArrowUp')
    await expect.poll(() => page.evaluate(() => (window as any).__wsSent.join(''))).toContain('\x1b[A')
    await expect.poll(() => page.evaluate(() => (window as any).__wsSent.join(''))).not.toMatch(/\x1bOA|\x1b\[</)

    await page.setViewportSize({ width: 1200, height: 800 })
    mark('A final start')
    await gotoLocalSession(aId)
    mark('A final connected')
    await page.waitForFunction(() => {
      const term = (window as any).__gmuxTerm
      const buffer = term.buffer.active
      const text = Array.from({ length: buffer.baseY + term.rows }, (_, y) => buffer.getLine(y)?.translateToString(true) ?? '').join('\n')
      return buffer.type === 'normal' && text.includes('A-BLUE')
        && !text.includes('B-ALT')
    })
    await page.screenshot({ path: '.memory/screenshots/terminal-switch-state-A-again.png' })
    await page.waitForFunction(() => {
      const term = (window as any).__gmuxTerm
      return !term.modes.applicationCursorKeys && !term.modes.bracketedPasteMode
        && term.modes.mouseTrackingMode === 'none'
    })

    await page.screenshot({ path: '.memory/screenshots/terminal-switch-state-after-reconnect.png' })

    // A real-daemon regression: C fills 40 distinguishable lines, then sets
    // a non-full region after its initial geometry has settled.
    mark('C attach')
    await gotoLocalSession(cId)
    mark('C connected')
    await page.locator('.xterm-helper-textarea').focus()
    await page.keyboard.type('go')
    await page.keyboard.press('Enter')
    await page.waitForFunction(() => {
      const term = (window as any).__gmuxTerm
      const text = Array.from({ length: term.buffer.active.baseY + term.rows }, (_, y) => term.buffer.active.getLine(y)?.translateToString(true) ?? '').join('\n')
      return text.includes('C-SCROLL-40')
    })
    mark('C margins')
    await expect.poll(() => page.evaluate(() => {
      const buffer = (window as any).__gmuxTerm._core?.buffers?.active
      return { top: buffer?.scrollTop, bottom: buffer?.scrollBottom }
    }), { timeout: 10_000 }).toMatchObject({ top: 1, bottom: 3 })
    mark('C reconnect start')
    await page.evaluate(() => {
      for (const ws of (window as any).__allWs as WebSocket[]) {
        if (ws.readyState === WebSocket.OPEN && ws.url.includes('/ws/')) ws.close()
      }
    })
    await expect(page.locator('.terminal-disconnected-pill')).toBeVisible({ timeout: 10_000 })
    await expect(page.locator('.terminal-disconnected-pill')).not.toBeVisible({ timeout: 10_000 })
    mark('C reconnect socket')
    await expect.poll(() => page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      const active = term.buffer.active
      const buffer = term._core?.buffers?.active
      const text = Array.from({ length: active.baseY + term.rows }, (_, y) => active.getLine(y)?.translateToString(true) ?? '').join('\n')
      return { top: buffer?.scrollTop, bottom: buffer?.scrollBottom, cursorX: active.cursorX, cursorY: active.cursorY, text }
    }), { timeout: 10_000 }).toMatchObject({ top: 1, bottom: 3, cursorX: 19, cursorY: 8 })
    mark('C reconnect margins and cursor restored')
    await expect.poll(() => page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      const text = Array.from({ length: term.buffer.active.baseY + term.rows }, (_, y) => term.buffer.active.getLine(y)?.translateToString(true) ?? '').join('\n')
      return Array.from({ length: 40 }, (_, i) => `C-SCROLL-${String(i + 1).padStart(2, '0')}`).filter(line => !text.includes(line))
    })).toEqual([])
    mark('C content restored')
    const beforeMarker = await page.evaluate(() => {
      const buffer = (window as any).__gmuxTerm.buffer.active
      return { row0: buffer.getLine(buffer.baseY)?.translateToString(true) ?? '' }
    })
    await page.locator('.xterm-helper-textarea').focus()
    await page.keyboard.type('marker')
    await page.keyboard.press('Enter')
    await expect.poll(() => page.evaluate(() => {
      const buffer = (window as any).__gmuxTerm.buffer.active
      const parkedRow = buffer.getLine(buffer.baseY + 8)?.translateToString(true) ?? ''
      return {
        row0: buffer.getLine(buffer.baseY)?.translateToString(true) ?? '',
        markerAtParkedColumn: parkedRow.slice(19).startsWith('C-PARK-MARK'),
      }
    })).toEqual({ row0: beforeMarker.row0, markerAtParkedColumn: true })
    mark('C marker landed at parked cursor')
    await page.keyboard.type('after')
    await page.keyboard.press('Enter')
    await page.waitForFunction(() => {
      const term = (window as any).__gmuxTerm
      const text = Array.from({ length: term.buffer.active.baseY + term.rows }, (_, y) => term.buffer.active.getLine(y)?.translateToString(true) ?? '').join('\n')
      return text.includes('C-AFTER-10')
    })
    mark('C after output')
    const afterRegion = await page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      const buffer = term.buffer.active
      return { top: buffer.getLine(buffer.baseY)?.translateToString(true), bottom: buffer.getLine(buffer.baseY + 4)?.translateToString(true), scrollTop: term._core?.buffers?.active.scrollTop, scrollBottom: term._core?.buffers?.active.scrollBottom }
    })
    expect(afterRegion).toMatchObject({ scrollTop: 1, scrollBottom: 3 })
    mark('C region verified')

    // Return to A before the failed-handshake check.
    await gotoLocalSession(aId)
    await page.waitForFunction(() => {
      const term = (window as any).__gmuxTerm
      const text = Array.from({ length: term.buffer.active.baseY + term.rows }, (_, y) => term.buffer.active.getLine(y)?.translateToString(true) ?? '').join('\n')
      return text.includes('A-BLUE')
    })

    // Failed replacement handshakes preserve the last committed screen.
    await page.evaluate(() => {
      ;(window as any).__blockTerminalReconnect = true
      for (const ws of (window as any).__allWs as WebSocket[]) {
        if (ws.readyState === WebSocket.OPEN && ws.url.includes('/ws/')) ws.close()
      }
    })
    await expect(page.locator('.terminal-disconnected-pill')).toBeVisible({ timeout: 10_000 })
    await page.waitForTimeout(1200)
    const preservedDuringOutage = await page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      const buffer = term.buffer.active
      return Array.from({ length: buffer.baseY + term.rows }, (_, y) => buffer.getLine(y)?.translateToString(true) ?? '').join('\\n').includes('A-BLUE')
    })
    expect(preservedDuringOutage).toBe(true)
    await page.evaluate(() => { ;(window as any).__blockTerminalReconnect = false })
    await expect(page.locator('.terminal-disconnected-pill')).not.toBeVisible({ timeout: 10_000 })

    const state = await page.evaluate(() => {
      const term = (window as any).__gmuxTerm
      const buffer = term.buffer.active
      const line = Array.from({ length: buffer.baseY + term.rows }, (_, y) => buffer.getLine(y)).find(line => line?.translateToString(true).includes('A-BLUE'))!
      return { buffer: buffer.type, baseY: buffer.baseY, viewportY: buffer.viewportY, cursorX: buffer.cursorX, cursorY: buffer.cursorY, cols: term.cols, rows: term.rows, text: line.translateToString(true), bg: line.getCell(0).bg }
    })
    expect(state).toMatchObject({ buffer: 'normal', text: expect.stringContaining('A-BLUE') })
    expect(state.cursorX).toBe(5)
    expect(state.cursorY).toBe(2)
    expect(state.viewportY).toBe(state.baseY)
    expect(state.bg).not.toBe(0)
  } finally {
    for (const id of [aId, bId, cId]) {
      await page.evaluate(async id => { await fetch(`/v1/sessions/${id}/kill`, { method: 'POST' }) }, id)
    }
  }
})

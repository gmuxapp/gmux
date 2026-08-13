import { test, expect } from '@playwright/test'
import { gotoSession, openApp } from '../helpers'

const A_COMMAND = 'printf "\\033[44mA-BLUE\\033[0m\\r\\n"; for i in $(seq 1 40); do printf "A-SCROLL-%02d\\r\\n" "$i"; done; printf "\\033[2;4r\\033[3;6H"; while true; do sleep 60; done'
const B_COMMAND = 'printf "B-NORMAL\\r\\n"; while true; do read -r line || exit; eval "$line"; done'

test('real A→B→A isolation and reconnect checkpoint', async ({ page }) => {
  await page.addInitScript(() => {
    ;(window as any).__allWs = [] as WebSocket[]
    ;(window as any).__blockTerminalReconnect = false
    const OriginalWebSocket = window.WebSocket
    ;(window as any).WebSocket = function (...args: ConstructorParameters<typeof WebSocket>) {
      const url = (window as any).__blockTerminalReconnect && String(args[0]).includes('/ws/')
        ? 'ws://127.0.0.1:1/unavailable'
        : args[0]
      const ws = new OriginalWebSocket(url, ...(args.slice(1) as any))
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
    const marker = command.includes('A-BLUE') ? 'A-BLUE' : 'B-NORMAL'
    const link = page.locator('a').filter({ hasText: marker }).first()
    await link.waitFor({ state: 'visible', timeout: 10_000 })
    const href = await link.getAttribute('href')
    return href!.split('~').pop()!
  }

  const aId = await launch(A_COMMAND)
  const bId = await launch(B_COMMAND)
  // Let the production SSE snapshot publish the newly launched sessions
  // before routing through the same navigation hook used by the suite.
  await page.waitForTimeout(1500)
  const gotoLocalSession = async (id: string) => {
    await gotoSession(page, id)
  }
  try {
    await gotoLocalSession(aId)
    await page.waitForFunction(() => {
      const term = (window as any).__gmuxTerm
      const buffer = term?.buffer.active
      const text = Array.from({ length: (buffer?.baseY ?? 0) + (term?.rows ?? 0) }, (_, y) => buffer?.getLine(y)?.translateToString(true) ?? '').join('\n')
      return buffer?.type === 'normal' && text.includes('A-BLUE') && text.includes('A-SCROLL-40')
    })
    await page.screenshot({ path: '.memory/screenshots/terminal-switch-state-A-before.png' })

    const aGeometry = await page.evaluate(() => ({ cols: (window as any).__gmuxTerm.cols, rows: (window as any).__gmuxTerm.rows }))
    await page.setViewportSize({ width: 900, height: 600 })
    await gotoLocalSession(bId)
    const bGeometry = await page.evaluate(() => ({ cols: (window as any).__gmuxTerm.cols, rows: (window as any).__gmuxTerm.rows }))
    expect(bGeometry.cols).toBeLessThan(aGeometry.cols)
    expect(bGeometry.rows).toBeLessThan(aGeometry.rows)
    await page.waitForFunction(() => (window as any).__gmuxTerm.buffer.active.getLine(0)?.translateToString(true).includes('B-NORMAL'))

    // Enter the alternate TUI only after its normal shell screen is present;
    // this makes the DEC 1049 saved-buffer contract observable on reconnect.
    await page.locator('.xterm-helper-textarea').focus()
    await page.keyboard.type("printf '\\033[?1049h\\033[2J\\033[H\\033[44mB-ALT\\033[0m\\033[3;5H'")
    await page.keyboard.press('Enter')
    await page.waitForFunction(() => (window as any).__gmuxTerm.buffer.active.type === 'alternate'
      && (window as any).__gmuxTerm.buffer.active.getLine(0)?.translateToString(true).includes('B-ALT'))
    await page.screenshot({ path: '.memory/screenshots/terminal-switch-state-B.png' })

    // Reconnect while the TUI owns the alternate buffer, then exit it. The
    // saved normal screen must be B-NORMAL, not the previous A screen.
    await page.evaluate(() => {
      for (const ws of (window as any).__allWs as WebSocket[]) {
        if (ws.readyState === WebSocket.OPEN && ws.url.includes('/ws/')) ws.close()
      }
    })
    await expect(page.locator('.terminal-disconnected-pill')).toBeVisible({ timeout: 10_000 })
    await expect(page.locator('.terminal-disconnected-pill')).not.toBeVisible({ timeout: 10_000 })
    await page.waitForFunction(() => (window as any).__gmuxTerm.buffer.active.type === 'alternate'
      && (window as any).__gmuxTerm.buffer.active.getLine(0)?.translateToString(true).includes('B-ALT')
      && (window as any).__gmuxTerm.buffer.normal.getLine(0)?.translateToString(true).includes('B-NORMAL'))
    await page.locator('.xterm-helper-textarea').focus()
    await page.keyboard.type("printf '\\033[?1049l'")
    await page.keyboard.press('Enter')
    await page.waitForFunction(() => (window as any).__gmuxTerm.buffer.active.type === 'normal'
      && (window as any).__gmuxTerm.buffer.active.getLine(0)?.translateToString(true).includes('B-NORMAL'))

    await page.setViewportSize({ width: 1200, height: 800 })
    await gotoLocalSession(aId)
    await page.waitForFunction(() => {
      const term = (window as any).__gmuxTerm
      const buffer = term.buffer.active
      const text = Array.from({ length: buffer.baseY + term.rows }, (_, y) => buffer.getLine(y)?.translateToString(true) ?? '').join('\n')
      return buffer.type === 'normal' && text.includes('A-BLUE')
        && !text.includes('B-ALT')
        && term.buffer.active.cursorX === 5 && term.buffer.active.cursorY === 2
    })
    await page.screenshot({ path: '.memory/screenshots/terminal-switch-state-A-again.png' })
    await page.evaluate(() => {
      for (const ws of (window as any).__allWs as WebSocket[] | undefined ?? []) {
        if (ws.readyState === WebSocket.OPEN && ws.url.includes('/ws/')) ws.close()
      }
    })
    await expect(page.locator('.terminal-disconnected-pill')).toBeVisible({ timeout: 10_000 })
    await expect(page.locator('.terminal-disconnected-pill')).not.toBeVisible({ timeout: 10_000 })
    await page.waitForFunction(() => {
      const term = (window as any).__gmuxTerm
      const buffer = term.buffer.active
      const aLine = Array.from({ length: buffer.baseY + term.rows }, (_, y) => buffer.getLine(y)).find(line => line?.translateToString(true).includes('A-BLUE'))
      return buffer.type === 'normal' && aLine != null
        && buffer.cursorX === 5 && buffer.cursorY === 2
        && aLine.getCell(0)?.bg !== 0
    })
    await page.screenshot({ path: '.memory/screenshots/terminal-switch-state-after-reconnect.png' })

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
    expect(state).toMatchObject({ buffer: 'normal', cursorX: 5, cursorY: 2, text: expect.stringContaining('A-BLUE') })
    expect(state.bg).not.toBe(0)
  } finally {
    for (const id of [aId, bId]) {
      await page.evaluate(async id => { await fetch(`/v1/sessions/${id}/kill`, { method: 'POST' }) }, id)
    }
  }
})

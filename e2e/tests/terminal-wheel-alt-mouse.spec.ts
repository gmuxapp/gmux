import { expect, test, type Page } from '@playwright/test'
import { gotoSession, openApp } from '../helpers'

// Regression: pi >= 1.0 runs fullscreen — it enters the alternate screen and
// enables SGR mouse tracking (?1049h ?1000h ?1002h ?1003h ?1006h) at startup,
// normally long before a browser attaches. The checkpoint selected the
// alternate buffer but did not restore mouse tracking, so xterm.js (alt
// buffer, no mouse tracking) translated the wheel into Up/Down arrows, which
// pi treats as prompt-history navigation. The runner now reports input modes
// in `terminal_checkpoint` and the browser restores them.

// Fullscreen TUI with mouse tracking, set before any browser attaches.
const MOUSE_TUI = 'stty -echo; printf "\\033[?1049h\\033[2J\\033[HMOUSE-TUI\\033[?1000h\\033[?1002h\\033[?1003h\\033[?1004h\\033[?1006h\\033[?2004h"; while true; do sleep 60; done'
// Fullscreen TUI without mouse tracking: wheel → arrows is xterm.js's
// alternate-scroll behaviour and must stay that way (less, vim w/o mouse).
const PLAIN_ALT = 'stty -echo; printf "\\033[?1049h\\033[2J\\033[HPLAIN-ALT"; while true; do sleep 60; done'

async function installWsRecorder(page: Page) {
  await page.addInitScript(() => {
    ;(window as any).__allWs = [] as WebSocket[]
    ;(window as any).__wsSent = [] as string[]
    const OriginalWebSocket = window.WebSocket
    ;(window as any).WebSocket = function (...args: ConstructorParameters<typeof WebSocket>) {
      const ws = new OriginalWebSocket(...args)
      const send = ws.send.bind(ws)
      ws.send = ((data: any) => {
        let bytes: Uint8Array
        if (typeof data === 'string') bytes = new TextEncoder().encode(data)
        else if (data instanceof ArrayBuffer) bytes = new Uint8Array(data)
        else if (ArrayBuffer.isView(data)) bytes = new Uint8Array(data.buffer, data.byteOffset, data.byteLength)
        else bytes = new Uint8Array()
        if (String(args[0]).includes('/ws/')) (window as any).__wsSent.push(new TextDecoder().decode(bytes))
        return send(data)
      }) as typeof ws.send
      ;(window as any).__allWs.push(ws)
      return ws
    } as unknown as typeof WebSocket
    Object.assign((window as any).WebSocket, OriginalWebSocket)
    ;(window as any).WebSocket.prototype = OriginalWebSocket.prototype
  })
}

async function launch(page: Page, command: string, marker: string): Promise<string> {
  const cwd = process.env.GMUX_TEST_WORKSPACE!
  await page.evaluate(async ({ command, cwd }) => {
    const response = await fetch('/v1/launch', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ command: ['bash', '-c', command], cwd }),
    })
    if (!response.ok) throw new Error(`launch failed: ${response.status}`)
  }, { command, cwd })
  const link = page.locator('a').filter({ hasText: marker }).first()
  await link.waitFor({ state: 'visible', timeout: 10_000 })
  return (await link.getAttribute('href'))!.split('~').pop()!
}

async function attached(page: Page, id: string, marker: string) {
  await gotoSession(page, id)
  await page.waitForFunction((marker) => {
    const buffer = (window as any).__gmuxTerm?.buffer.active
    return buffer?.type === 'alternate' && buffer.getLine(0)?.translateToString(true).includes(marker)
  }, marker, { timeout: 10_000 })
}

/** Wheel up three notches over the terminal; return the bytes sent to the PTY. */
async function wheelBytes(page: Page): Promise<string> {
  await page.evaluate(() => { ;(window as any).__wsSent = [] })
  const box = await page.locator('.xterm').boundingBox()
  if (!box) throw new Error('xterm has no bounding box')
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
  for (let i = 0; i < 3; i++) {
    await page.mouse.wheel(0, -120)
    await page.waitForTimeout(50)
  }
  await page.waitForTimeout(300)
  return page.evaluate(() => (window as any).__wsSent.join(''))
}

const SGR_WHEEL_UP = /\x1b\[<64;\d+;\d+M/
const ARROW_UP = /\x1b(\[|O)A/

async function expectMouseWheel(page: Page, when: string) {
  const sent = await wheelBytes(page)
  expect(sent, `${when}: wheel must be reported as SGR mouse`).toMatch(SGR_WHEEL_UP)
  expect(sent, `${when}: wheel must not become arrow keys`).not.toMatch(ARROW_UP)
}

test('wheel in an alternate-screen TUI follows its mouse modes across attach, switch, reconnect and reload', async ({ page }) => {
  test.setTimeout(90_000)
  await installWsRecorder(page)
  await openApp(page)
  const mouseId = await launch(page, MOUSE_TUI, 'MOUSE-TUI')
  const plainId = await launch(page, PLAIN_ALT, 'PLAIN-ALT')
  await page.waitForTimeout(1500)

  await attached(page, mouseId, 'MOUSE-TUI')
  expect(await page.evaluate(() => {
    const term = (window as any).__gmuxTerm
    return { mouse: term.modes.mouseTrackingMode, paste: term.modes.bracketedPasteMode, focus: term.modes.sendFocusMode }
  })).toEqual({ mouse: 'any', paste: true, focus: true })
  await expectMouseWheel(page, 'first attach')

  // Session isolation: the plain alt-screen session must not inherit mouse
  // tracking, and its wheel stays xterm's alternate-scroll arrows.
  await attached(page, plainId, 'PLAIN-ALT')
  expect(await page.evaluate(() => (window as any).__gmuxTerm.modes.mouseTrackingMode)).toBe('none')
  const plain = await wheelBytes(page)
  expect(plain).toMatch(ARROW_UP)
  expect(plain).not.toMatch(SGR_WHEEL_UP)

  await attached(page, mouseId, 'MOUSE-TUI')
  await expectMouseWheel(page, 'after session switch')

  await page.evaluate(() => {
    for (const ws of (window as any).__allWs as WebSocket[]) {
      if (ws.readyState === WebSocket.OPEN && ws.url.includes('/ws/')) ws.close()
    }
  })
  await expect(page.locator('.terminal-disconnected-pill')).toBeVisible({ timeout: 10_000 })
  await expect(page.locator('.terminal-disconnected-pill')).not.toBeVisible({ timeout: 10_000 })
  await expectMouseWheel(page, 'after reconnect')

  await page.reload()
  await page.waitForFunction(() => (window as any).__gmuxTerm?.buffer.active.type === 'alternate', undefined, { timeout: 10_000 })
  await page.waitForTimeout(500)
  await expectMouseWheel(page, 'after reload')
})

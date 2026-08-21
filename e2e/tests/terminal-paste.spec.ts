/**
 * E2E tests for terminal paste handling (attachPasteHandler).
 *
 * Strategy: patch WebSocket.prototype.send via addInitScript so every binary
 * send (Uint8Array) is recorded in window.__wsCaptured.  Then dispatch
 * synthetic ClipboardEvents on the .terminal-container element and assert
 * what arrived in the capture log.
 *
 * The paste handler intercepts in the capture phase, so dispatching on the
 * container element is sufficient — xterm's listeners on .xterm and textarea
 * are inside the container and are never reached when stopPropagation() fires.
 */
import { test, expect, type Page } from '@playwright/test'
import { openApp, gotoTestSession } from '../helpers'

// ── Helpers ──────────────────────────────────────────────────────────────────

/** Dispatch a synthetic paste event carrying `text` on the terminal container. */
async function dispatchPaste(page: Page, text: string): Promise<void> {
  await page.evaluate((t) => {
    const container = document.querySelector('.terminal-container') as HTMLElement | null
    if (!container) throw new Error('.terminal-container not found')
    const dt = new DataTransfer()
    dt.setData('text/plain', t)
    container.dispatchEvent(
      new ClipboardEvent('paste', { bubbles: true, cancelable: true, clipboardData: dt }),
    )
  }, text)
}

/** Dispatch a synthetic binary paste through the production DOM handler. */
async function dispatchBinaryPaste(page: Page, size: number): Promise<void> {
  await page.evaluate((bytes) => {
    const container = document.querySelector('.terminal-container') as HTMLElement | null
    if (!container) throw new Error('.terminal-container not found')
    const dt = new DataTransfer()
    dt.items.add(new File([new Uint8Array(bytes)], 'clipboard.png', { type: 'image/png' }))
    container.dispatchEvent(
      new ClipboardEvent('paste', { bubbles: true, cancelable: true, clipboardData: dt }),
    )
  }, size)
}

/** Drain and return all binary WS sends recorded since the last drain. */
async function drainCaptured(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const all = [...((window as any).__wsCaptured as string[])]
    ;(window as any).__wsCaptured.length = 0
    return all
  })
}

/** Override term.modes on the xterm instance to force a specific bracketedPasteMode value. */
async function setBracketedPasteMode(page: Page, enabled: boolean): Promise<void> {
  await page.evaluate((val) => {
    const term = (window as any).__gmuxTerm
    Object.defineProperty(term, 'modes', {
      get: () => ({ bracketedPasteMode: val }),
      configurable: true,
    })
  }, enabled)
}

/** Remove any instance-level override of term.modes (restores prototype getter). */
async function resetBracketedPasteMode(page: Page): Promise<void> {
  await page.evaluate(() => {
    try { delete (window as any).__gmuxTerm.modes } catch { /* noop */ }
  })
}

// ── Test suite ────────────────────────────────────────────────────────────────

test.describe('terminal paste', () => {
  test.beforeEach(async ({ page }) => {
    // Instrument WebSocket.prototype.send BEFORE the page loads so we capture
    // every binary send from the very first connection.
    await page.addInitScript(() => {
      const NativeWebSocket = WebSocket
      const origSend = NativeWebSocket.prototype.send
      ;(window as any).__wsCaptured = [] as string[]
      ;(window as any).__wsInstances = [] as WebSocket[]
      NativeWebSocket.prototype.send = function (data: unknown) {
        if (data instanceof Uint8Array) {
          ;(window as any).__wsCaptured.push(new TextDecoder().decode(data))
        }
        return origSend.apply(this, [data as any])
      }
      window.WebSocket = new Proxy(NativeWebSocket, {
        construct(Target, args) {
          const ws = Reflect.construct(Target, args) as WebSocket
          ;(window as any).__wsInstances.push(ws)
          return ws
        },
      })
    })

    await openApp(page)
    await gotoTestSession(page)

    // Discard any data sent during session setup (resize etc. are strings and
    // filtered out, but binary data like initial input isn't expected here).
    await drainCaptured(page)
  })

  // ── Basic delivery ──────────────────────────────────────────────────────────

  test('single-line paste is sent verbatim', async ({ page }) => {
    await setBracketedPasteMode(page, false)

    await dispatchPaste(page, 'hello world')
    const captured = await drainCaptured(page)

    expect(captured).toContain('hello world')
  })

  // ── Newline normalisation (non-bracketed mode) ──────────────────────────────
  // In non-bracketed mode, newlines are kept as \n so that applications in raw
  // mode can distinguish pasted newlines from Enter (\r).

  test('\\n is kept as \\n in non-bracketed mode', async ({ page }) => {
    await setBracketedPasteMode(page, false)

    await dispatchPaste(page, 'line1\nline2\nline3')
    const captured = await drainCaptured(page)

    expect(captured).toContain('line1\nline2\nline3')
    // Confirm \r did NOT make it through
    expect(captured.some(s => s.includes('\r'))).toBe(false)
  })

  test('\\r\\n is normalised to \\n in non-bracketed mode', async ({ page }) => {
    await setBracketedPasteMode(page, false)

    await dispatchPaste(page, 'line1\r\nline2')
    const captured = await drainCaptured(page)

    expect(captured).toContain('line1\nline2')
  })

  // ── Bracketed paste mode ────────────────────────────────────────────────────

  test('multi-line paste is wrapped in bracket sequences in bracketed mode', async ({ page }) => {
    await setBracketedPasteMode(page, true)

    await dispatchPaste(page, 'line1\nline2')
    const captured = await drainCaptured(page)

    // Newlines are first normalised to \r, then the whole blob is bracketed.
    expect(captured).toContain('\x1b[200~line1\rline2\x1b[201~')

    await resetBracketedPasteMode(page)
  })

  test('single-line paste in bracketed mode is still wrapped', async ({ page }) => {
    await setBracketedPasteMode(page, true)

    await dispatchPaste(page, 'hello')
    const captured = await drainCaptured(page)

    expect(captured).toContain('\x1b[200~hello\x1b[201~')

    await resetBracketedPasteMode(page)
  })

  // ── ESC sanitisation (bracketed mode security) ──────────────────────────────

  test('ESC characters in pasted text are replaced with ␛ in bracketed mode', async ({ page }) => {
    await setBracketedPasteMode(page, true)

    // An attacker could craft clipboard content containing \x1b[201~ to
    // terminate the bracket early and inject commands.
    await dispatchPaste(page, 'safe\x1b[201~injected')
    const captured = await drainCaptured(page)

    // The ESC should be replaced with U+241B, keeping the brackets intact.
    expect(captured).toContain('\x1b[200~safe\u241b[201~injected\x1b[201~')
    // The raw attack sequence must not appear unguarded
    expect(captured.some(s => s.includes('\x1b[201~injected'))).toBe(false)

    await resetBracketedPasteMode(page)
  })

  // ── Binary failure feedback ─────────────────────────────────────────────────

  test('delayed image upload is cancelled after its socket is replaced', async ({ page }) => {
    let releaseUpload!: () => void
    const uploadMayFinish = new Promise<void>(resolve => { releaseUpload = resolve })
    let uploadPending!: () => void
    const sawPendingUpload = new Promise<void>(resolve => { uploadPending = resolve })
    await page.route('**/v1/sessions/*/clipboard', async route => {
      uploadPending()
      await uploadMayFinish
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ ok: true, data: { path: '/tmp/socket-replaced.png' } }),
      })
    })

    await dispatchBinaryPaste(page, 3)
    await sawPendingUpload
    const originalIndex = await page.evaluate(() => {
      const sockets = (window as any).__wsInstances as WebSocket[]
      const index = sockets.findLastIndex(ws => ws.url.includes('/ws/') && ws.readyState === WebSocket.OPEN)
      if (index < 0) throw new Error('no open terminal WebSocket')
      sockets[index].close()
      return index
    })
    await expect.poll(() => page.evaluate(index => {
      const sockets = (window as any).__wsInstances as WebSocket[]
      return sockets.some((ws, i) => i > index && ws.url.includes('/ws/') && ws.readyState === WebSocket.OPEN)
    }, originalIndex)).toBe(true)
    await page.evaluate(index => {
      const sockets = (window as any).__wsInstances as WebSocket[]
      // Keep the stale socket's state check non-vacuous: cancellation must be
      // enforced by connection object identity, not merely CLOSED state. If
      // that production guard is removed, the send attempt does not report
      // cancellation, so the expected cancellation toast never appears.
      Object.defineProperty(sockets[index], 'readyState', {
        configurable: true,
        get: () => WebSocket.OPEN,
      })
    }, originalIndex)

    await drainCaptured(page)
    releaseUpload()

    await expect(page.getByText('Paste cancelled: terminal connection changed')).toBeVisible()
    await expect.poll(async () => (await drainCaptured(page)).some(data => data.includes('/tmp/socket-replaced.png')))
      .toBe(false)
  })

  test('oversize binary paste is not injected and shows a toast', async ({ page }) => {
    await dispatchBinaryPaste(page, 10 * 1024 * 1024 + 1)

    await expect(page.getByText('Clipboard item too large (limit 10MB)')).toBeVisible()
    expect(await drainCaptured(page)).toEqual([])
  })

  // ── No double-paste ─────────────────────────────────────────────────────────

  test('paste fires exactly once — xterm native handler is suppressed', async ({ page }) => {
    await setBracketedPasteMode(page, false)

    const marker = 'unique-paste-marker-99x'
    await dispatchPaste(page, marker)
    const captured = await drainCaptured(page)

    const hits = captured.filter(s => s.includes(marker))
    expect(hits).toHaveLength(1)
  })
})

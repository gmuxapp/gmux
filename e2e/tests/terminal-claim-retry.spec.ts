import { expect, test, type Page } from '@playwright/test'
import { apiGet, openApp, pollUntil } from '../helpers'

/**
 * Regression tests for the first-viewport-claim retry (v2.1 review finding
 * F2): if `measureTerminalFit` returns null at the moment of the first claim
 * (e.g. xterm renderer dimensions transiently undefined), the attach must
 * never stay parked in 'claiming' — output buffered and input dropped forever.
 *
 * Contract under test:
 *  - a later real measurement (layout/renderer lifecycle) completes the claim,
 *  - a permanently-null measurement falls back to flowing at checkpoint
 *    geometry within a bounded deadline, without sending any made-up size,
 *  - reconnect and session switch cancel the pending retry cleanly,
 *  - input stays closed while unclaimed and opens on recovery.
 *
 * The null measurement is forced by shadowing `term.dimensions` with an
 * instance getter (the only known trigger lives inside xterm internals);
 * everything downstream of measureTerminalFit is exercised for real.
 */

const FALLBACK_MS = 4000 // keep in sync with CLAIM_RETRY_FALLBACK_MS in terminal.tsx

async function launchSession(page: Page, command: string): Promise<string> {
  const cwd = process.env.GMUX_TEST_WORKSPACE!
  const before = await apiGet<{ data: Array<{ id: string }> }>('/v1/sessions')
  const known = new Set(before.body.data.map(s => s.id))
  const launched = await page.evaluate(async ({ command, cwd }) => {
    const r = await fetch('/v1/launch', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ command: ['bash', '-c', command], cwd }) })
    return { ok: r.ok, text: await r.text() }
  }, { command, cwd })
  expect(launched.ok, launched.text).toBe(true)
  return pollUntil(async () => {
    const r = await apiGet<{ data: Array<{ id: string, alive: boolean }> }>('/v1/sessions')
    return r.body.data.find(s => s.alive && !known.has(s.id))?.id
  }, { timeoutMs: 10_000, description: 'claim-retry session' })
}

function tickerCommand(tag: string): string {
  return `echo MARKER_${tag}; i=0; while true; do i=$((i+1)); echo TICK_${tag}_$i; sleep 0.3; done`
}

/**
 * Gate WS message delivery so the test can shadow `term.dimensions` after
 * mount but before the checkpoint replay lands (the claim runs on replay
 * completion, so this is the only deterministic interposition point).
 */
function installWsGate(page: Page) {
  return page.addInitScript(() => {
    const Orig = window.WebSocket
    // @ts-expect-error test shim
    window.WebSocket = class extends Orig {
      constructor(url: string, protos?: string | string[]) {
        super(url, protos)
        if (!String(url).includes('/ws/')) return
        ;(window as any).__testWs = this
        const q: MessageEvent[] = []
        let held = true
        let handler: ((ev: MessageEvent) => void) | null = null
        Object.defineProperty(this, 'onmessage', {
          set: (h) => { handler = h },
          get: () => handler,
        })
        this.addEventListener('message', (ev) => {
          // Record the checkpoint geometry the runner declared for this
          // frame: the fallback claim must leave local xterm geometry
          // exactly there (the replay barrier's size), never a made-up one.
          if (typeof (ev as MessageEvent).data === 'string') {
            try {
              const msg = JSON.parse((ev as MessageEvent).data)
              if (msg?.type === 'terminal_checkpoint' && Number.isInteger(msg.cols) && Number.isInteger(msg.rows)) {
                ;(window as any).__ckptGeom = { cols: msg.cols, rows: msg.rows }
              }
            } catch { /* binary/terminal bytes */ }
          }
          if (held) q.push(ev as MessageEvent)
          else handler?.call(this, ev as MessageEvent)
        })
        ;(window as any).__releaseWs = () => {
          held = false
          while (q.length) handler?.call(this, q.shift()!)
        }
      }
    }
  })
}

async function navigate(page: Page, sessionId: string) {
  await page.waitForFunction((id) => {
    const nav = (window as any).__gmuxNavigateToSession
    return typeof nav === 'function' && nav(id) === true
  }, sessionId, { timeout: 10_000 })
  await page.locator('.xterm').waitFor({ state: 'visible', timeout: 5_000 })
}

async function bufferText(page: Page): Promise<string> {
  return page.evaluate(() => {
    const buf = (window as any).__gmuxTerm.buffer.active
    let t = ''
    for (let y = 0; y < buf.length; y++) t += (buf.getLine(y)?.translateToString(true) ?? '') + '\n'
    return t
  })
}

function maxTick(text: string, tag: string): number {
  const nums = (text.match(new RegExp(`TICK_${tag}_(\\d+)`, 'g')) || []).map(m => parseInt(m.slice(6 + tag.length), 10))
  return nums.length ? Math.max(...nums) : 0
}

/** Shadow term.dimensions with undefined → measureTerminalFit returns null. */
async function forceNullMeasurement(page: Page) {
  await page.waitForFunction(() => (window as any).__gmuxTerm && (window as any).__testWs && (window as any).__releaseWs)
  await page.evaluate(() => {
    const term = (window as any).__gmuxTerm
    Object.defineProperty(term, 'dimensions', { get: () => undefined, configurable: true })
    ;(window as any).__releaseWs()
  })
}

async function restoreMeasurement(page: Page) {
  await page.evaluate(() => { delete (window as any).__gmuxTerm.dimensions })
}

async function terminalCols(sessionId: string): Promise<number | undefined> {
  const r = await apiGet<{ data: Array<{ id: string, terminal_cols?: number }> }>('/v1/sessions')
  return r.body.data.find(s => s.id === sessionId)?.terminal_cols
}

test('null first claim recovers via a later real measurement (resize lifecycle)', async ({ page }) => {
  await installWsGate(page)
  await openApp(page)
  const id = await launchSession(page, tickerCommand('R'))
  await navigate(page, id)
  await forceNullMeasurement(page)

  // Stall begins: replay rendered, live ticks held, spinner up. Wait past
  // the immediate rAF re-measurement budget so recovery must come from the
  // ResizeObserver lifecycle, not the initial frame chain. Ticks rendered so
  // far came from the checkpoint; held live ticks must not render.
  await page.waitForTimeout(400)
  const base = maxTick(await bufferText(page), 'R')
  await page.waitForTimeout(1000)
  expect(maxTick(await bufferText(page), 'R')).toBeLessThanOrEqual(base + 1)
  await expect(page.locator('.terminal-loading')).toHaveCount(1)

  // Measurement becomes available again; a real layout change triggers the
  // retry. Recovery must beat the fallback deadline (proving it came from
  // the measured ResizeObserver path), and the claim must be the *measured*
  // size — verify the runner adopted this browser's geometry.
  await restoreMeasurement(page)
  await page.setViewportSize({ width: 1000, height: 700 })

  await expect(page.locator('.terminal-loading')).toHaveCount(0, { timeout: 2_000 })
  await pollUntil(async () => {
    const text = await bufferText(page)
    return maxTick(text, 'R') > base + 3 ? text : null
  }, { timeoutMs: 5_000, description: 'live output flows after measured recovery' })

  const xtermCols = await page.evaluate(() => (window as any).__gmuxTerm.cols as number)
  await pollUntil(async () => (await terminalCols(id)) === xtermCols || undefined,
    { timeoutMs: 5_000, description: 'runner adopted the measured claim size' })

  // Input opened with the claim.
  await page.locator('.xterm').click()
  await page.keyboard.type('echo INPUT_OK_R\n')
  await pollUntil(async () => (await bufferText(page)).includes('INPUT_OK_R') || undefined,
    { timeoutMs: 5_000, description: 'input echoes after measured recovery' })
})

test('permanently-null measurement falls back within the deadline; input reopens; no bogus size sent', async ({ page }) => {
  await installWsGate(page)
  await openApp(page)
  const id = await launchSession(page, tickerCommand('P'))
  // A fresh session has no terminal_cols until some client sends a resize;
  // capture whatever the runner reports (possibly undefined) as baseline.
  const colsBefore = await terminalCols(id)
  await navigate(page, id)
  await forceNullMeasurement(page)

  // While unclaimed, typed input must be dropped, not queued.
  await page.waitForTimeout(500)
  await page.locator('.xterm').click({ force: true }) // spinner overlays the grid
  await page.keyboard.type('echo INPUT_DROPPED_P\n')

  // Bounded: the deterministic fallback flips to claimed within the deadline
  // and releases held output — despite measurement staying null forever.
  await pollUntil(async () => {
    const text = await bufferText(page)
    return maxTick(text, 'P') >= 8 ? text : null
  }, { timeoutMs: FALLBACK_MS + 4_000, description: 'live output flows after fallback claim' })
  await expect(page.locator('.terminal-loading')).toHaveCount(0)

  // Load-bearing: the fallback must leave the local xterm grid exactly at
  // the checkpoint geometry the runner declared for the replayed frame —
  // proceeding with any fake/measured-less local geometry would resize the
  // grid away from the frame it just parsed.
  const geom = await page.evaluate(() => ({
    ckpt: (window as any).__ckptGeom as { cols: number, rows: number } | undefined,
    cols: (window as any).__gmuxTerm.cols as number,
    rows: (window as any).__gmuxTerm.rows as number,
  }))
  expect(geom.ckpt, 'runner did not declare checkpoint geometry').toBeTruthy()
  expect({ cols: geom.cols, rows: geom.rows }).toEqual(geom.ckpt)

  // Pre-claim input never reached the shell; post-fallback input echoes.
  await page.locator('.xterm').click()
  await page.keyboard.type('echo INPUT_OK_P\n')
  const text = await pollUntil(async () => {
    const t = await bufferText(page)
    return t.includes('INPUT_OK_P') ? t : null
  }, { timeoutMs: 5_000, description: 'input echoes after fallback claim' })
  expect(text).not.toContain('INPUT_DROPPED_P')

  // No claim was ever sent: the runner keeps its original geometry.
  expect(await terminalCols(id)).toBe(colsBefore)
})

test('socket bounce during pending retry cancels it and reconnect flows claimed', async ({ page }) => {
  await installWsGate(page)
  await openApp(page)
  const id = await launchSession(page, tickerCommand('B'))
  await navigate(page, id)
  await forceNullMeasurement(page)
  await page.waitForTimeout(800)
  await expect(page.locator('.terminal-loading')).toHaveCount(1)

  // Bounce the socket while the retry is pending (before the deadline).
  await page.evaluate(() => {
    ;(window as any).__testWsOld = (window as any).__testWs
    ;(window as any).__testWs.close()
  })
  await page.waitForFunction(() => (window as any).__testWs !== (window as any).__testWsOld, undefined, { timeout: 15_000 })
  await page.evaluate(() => (window as any).__releaseWs())

  // Reconnect enters 'claimed' (no reclaim); output flows even though the
  // measurement is still null. The cancelled retry must not fire later —
  // watch through the old deadline window for anomalies.
  await pollUntil(async () => {
    const text = await bufferText(page)
    return maxTick(text, 'B') >= 8 ? text : null
  }, { timeoutMs: 15_000, description: 'live output flows after reconnect' })
  await page.waitForTimeout(FALLBACK_MS + 500)
  await expect(page.locator('.terminal-loading')).toHaveCount(0)
  const t1 = maxTick(await bufferText(page), 'B')
  await page.waitForTimeout(1_000)
  expect(maxTick(await bufferText(page), 'B')).toBeGreaterThan(t1)
})

test('session switch during pending retry cancels it; stale claim never lands', async ({ page }) => {
  await installWsGate(page)
  await openApp(page)
  const idA = await launchSession(page, tickerCommand('A'))
  const idB = await launchSession(page, tickerCommand('C'))
  const colsABefore = await terminalCols(idA) // possibly undefined pre-claim

  await navigate(page, idA)
  await forceNullMeasurement(page)
  await page.waitForTimeout(800)
  await expect(page.locator('.terminal-loading')).toHaveCount(1)

  // Switch sessions while A's retry is pending; restore measurement so B's
  // fresh connect claims normally on the same xterm instance.
  await restoreMeasurement(page)
  await page.evaluate(() => { (window as any).__testWsOld = (window as any).__testWs })
  await navigate(page, idB)
  // The gate rebinds per socket: wait for B's socket to exist before
  // releasing, or we'd release A's stale gate and hold B's forever.
  await page.waitForFunction(() => (window as any).__testWs !== (window as any).__testWsOld, undefined, { timeout: 15_000 })
  await page.evaluate(() => (window as any).__releaseWs())

  await pollUntil(async () => {
    const text = await bufferText(page)
    return maxTick(text, 'C') >= 5 ? text : null
  }, { timeoutMs: 15_000, description: 'session B attaches and flows' })
  await expect(page.locator('.terminal-loading')).toHaveCount(0)

  // A's cancelled retry must not fire after its deadline would have passed:
  // no fallback claim against A, no size ever sent to A's runner.
  await page.waitForTimeout(FALLBACK_MS + 500)
  expect(await terminalCols(idA)).toBe(colsABefore)
  expect(maxTick(await bufferText(page), 'A')).toBe(0)
})

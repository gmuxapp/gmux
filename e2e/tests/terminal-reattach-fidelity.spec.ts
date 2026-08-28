import { expect, test, type Page } from '@playwright/test'
import { apiGet, gotoSession, openApp, pollUntil } from '../helpers'

/**
 * Composed regression tests for checkpoint replay fidelity (v2.1 review
 * findings F1/F2 and the reflow follow-up).
 *
 * Contract under test: soft-wrapped prose replays with FULL WORD FIDELITY —
 * no concatenated words (trimmed boundary spaces) and no interior padding
 * runs (stale-width joins) — across:
 *  - a first attach whose content wrapped at the runner's pre-claim width,
 *  - a detach/reattach cycle spanning the runner's reconnect shrink
 *    (the emulator reflows its normal buffer on width changes), and
 *  - content that has rotated into scrollback before the width changes.
 */

const WORDS = ['alpha', 'bravo', 'charlie', 'delta', 'echo', 'foxtrot', 'golf', 'hotel', 'india', 'juliet', 'kilo', 'lima', 'mike', 'november', 'oscar', 'papa']

function sentence(): string {
  const parts: string[] = []
  for (let i = 0; i < 60; i++) parts.push(WORDS[i % WORDS.length])
  return parts.join(' ')
}

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
  }, { timeoutMs: 10_000, description: 'fidelity session' })
}

function wrappedSentenceCommand(scrollbackFiller = 0): string {
  const filler = scrollbackFiller > 0
    ? `for i in $(seq 1 ${scrollbackFiller}); do printf 'filler-%03d\\r\\n' "$i"; done; `
    : ''
  return `printf '%s\\r\\n' 'MARKER-BEGIN'; printf '%s\\r\\n' '${sentence()}'; printf '%s\\r\\n' 'MARKER-END'; ${filler}while :; do sleep 1; done`
}

async function waitForText(page: Page, text: string): Promise<void> {
  await page.waitForFunction((needle) => {
    const t = (window as any).__gmuxTerm
    if (!t) return false
    const buf = t.buffer.active
    for (let y = 0; y < buf.length; y++) {
      if (buf.getLine(y)?.translateToString(true).includes(needle)) return true
    }
    return false
  }, text, { timeout: 15_000 })
}

/** The sentence between the markers, joined across buffer rows unmodified. */
async function replayedWords(page: Page): Promise<string[]> {
  const lines = await page.evaluate(() => {
    const t = (window as any).__gmuxTerm
    const buf = t.buffer.active
    const out: string[] = []
    for (let y = 0; y < buf.length; y++) out.push(buf.getLine(y)?.translateToString(false) ?? '')
    return out
  })
  const begin = lines.findIndex(l => l.includes('MARKER-BEGIN'))
  const end = lines.findIndex(l => l.includes('MARKER-END'))
  expect(begin, `markers missing in buffer: begin=${begin} end=${end}`).toBeGreaterThanOrEqual(0)
  expect(end).toBeGreaterThan(begin)
  // No trailing-whitespace stripping per row: word fidelity must come from
  // the replayed cells themselves. Interior padding runs and concatenations
  // both surface as word-list mismatches.
  return lines.slice(begin + 1, end).join('').split(/\s+/).filter(Boolean)
}

async function detachAndReturn(page: Page, id: string): Promise<void> {
  // Last viewer leaving shrinks the PTY by one column without updating
  // session metadata — the historical stale-width trap.
  await page.goto('/')
  await page.waitForTimeout(1500)
  await gotoSession(page, id)
  await waitForText(page, 'MARKER-END')
  await page.waitForTimeout(500)
}

test('first attach joins pre-claim wrapped output without corrupting words', async ({ page }) => {
  await openApp(page)
  const id = await launchSession(page, wrappedSentenceCommand())
  await gotoSession(page, id)
  await waitForText(page, 'MARKER-END')
  await expect(page.locator('.terminal-loading')).not.toBeVisible({ timeout: 5_000 })
  await page.waitForTimeout(500)

  expect(await replayedWords(page)).toEqual(sentence().split(' '))
})

test('reattach after the reconnect shrink preserves word fidelity', async ({ page }) => {
  await openApp(page)
  const id = await launchSession(page, wrappedSentenceCommand())
  await gotoSession(page, id)
  await waitForText(page, 'MARKER-END')
  await expect(page.locator('.terminal-loading')).not.toBeVisible({ timeout: 5_000 })

  await detachAndReturn(page, id)
  expect(await replayedWords(page)).toEqual(sentence().split(' '))
})

test('wrapped prose in scrollback survives the reconnect shrink', async ({ page }) => {
  await openApp(page)
  // Enough filler to push the wrapped sentence well into scrollback before
  // any width change, so the replay exercises scrollback reflow + join.
  const id = await launchSession(page, wrappedSentenceCommand(80))
  await gotoSession(page, id)
  await waitForText(page, 'filler-080')
  await expect(page.locator('.terminal-loading')).not.toBeVisible({ timeout: 5_000 })

  await detachAndReturn(page, id)
  expect(await replayedWords(page)).toEqual(sentence().split(' '))
})

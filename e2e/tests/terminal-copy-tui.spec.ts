/**
 * E2E test for copying from TUI-rendered output (pi and friends).
 *
 * The selectionToText fix (#189) handles the easy case: shell output
 * where cells past the content are never-written, so xterm.js's
 * translateToString(trimRight=true) drops them. The harder case the
 * codepoint-0 trim doesn't catch is the one TUIs produce: the renderer
 * fills each row with *explicit* space cells before the newline, so
 * those cells have codepoint 32 and survive the trim. Without an extra
 * step, selecting and copying yielded a wall of spaces.
 *
 * The fix this test pins extends the "trailing whitespace belongs to
 * the line break" rule to codepoint-32 cells too: when the selection
 * reaches the row boundary, post-strip ASCII whitespace from the
 * slice. See selection.ts's docstring for the full rationale.
 *
 * Pi's actual output shape was verified out-of-band by running pi
 * under a PTY and replaying the raw stream through pyte: each content
 * row is content text + explicit space cells (via `\x1b[2K` followed
 * by literal-space fill to col cols-1) + `\r\n`. The fixture below
 * mirrors that.
 */
import { test, expect, type Page } from '@playwright/test'
import { openApp, gotoTestSession } from '../helpers'

// ── Helpers ──────────────────────────────────────────────────────────────────

/**
 * Capture every call to navigator.clipboard.writeText so the test can
 * assert what the production code put on the clipboard, without needing
 * the OS clipboard or a clipboard-read permission grant. Same shape as
 * terminal-paste.spec.ts's WS capture.
 */
async function captureClipboardWrites(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const w = window as unknown as { __clipboardWrites: string[] }
    w.__clipboardWrites = []
    const orig = navigator.clipboard.writeText.bind(navigator.clipboard)
    navigator.clipboard.writeText = (text: string) => {
      w.__clipboardWrites.push(text)
      return orig(text)
    }
  })
}

async function lastClipboardWrite(page: Page): Promise<string | null> {
  return page.evaluate(() => {
    const writes = (window as unknown as { __clipboardWrites: string[] }).__clipboardWrites
    return writes.length > 0 ? writes[writes.length - 1] : null
  })
}

/**
 * Inject `lines` into the xterm buffer separated by CRLFs and return the
 * absolute buffer-row range the block occupies. Bypasses the PTY/WS so
 * the test is deterministic and doesn't depend on a running pi binary
 * in CI.
 *
 * Computing the range from the cursor position after the write avoids
 * brittle assumptions about scrollback and prior session output: the
 * cursor's absolute row (`baseY + cursorY`) lands on the last written
 * line, and the block is `lines.length - 1` rows tall above it.
 */
async function injectLines(page: Page, lines: string[]): Promise<{ startRow: number, endRow: number }> {
  return page.evaluate(({ ls }) => {
    return new Promise<{ startRow: number, endRow: number }>(resolve => {
      const term = (window as unknown as {
        __gmuxTerm: {
          write: (s: string, cb: () => void) => void
          buffer: { active: { baseY: number, cursorY: number } }
        }
      }).__gmuxTerm
      term.write(ls.join('\r\n'), () => {
        const buf = term.buffer.active
        const endRow = buf.baseY + buf.cursorY
        resolve({ startRow: endRow - (ls.length - 1), endRow })
      })
    })
  }, { ls: lines })
}

/**
 * Programmatically select rows [startRow, endRow] inclusive across the
 * full terminal width. xterm's `select(col, row, length)` walks `length`
 * cells forward in row-major order, so the length must be measured in
 * the terminal's actual `cols`, not the visible box's narrower width;
 * otherwise the end coordinate falls in the middle of a row.
 *
 * Selecting the full row width matches the user gesture we care about
 * (drag-select that includes pad cells past the rendered box edge),
 * which is also what xterm's selectLineAt / triple-click produces.
 */
async function selectRows(page: Page, startRow: number, endRow: number): Promise<void> {
  await page.evaluate(({ s, e }) => {
    const term = (window as unknown as {
      __gmuxTerm: {
        cols: number
        select: (col: number, row: number, length: number) => void
      }
    }).__gmuxTerm
    term.select(0, s, term.cols * (e - s + 1))
  }, { s: startRow, e: endRow })
}

/** Focus the terminal so keyboard events route through xterm. */
async function focusTerm(page: Page): Promise<void> {
  await page.evaluate(() => {
    (window as unknown as { __gmuxTerm: { focus: () => void } }).__gmuxTerm.focus()
  })
}

// ── Test suite ───────────────────────────────────────────────────────────────

test.describe('copy from TUI output', () => {
  test.beforeEach(async ({ page }) => {
    await captureClipboardWrites(page)
    await openApp(page)
    await gotoTestSession(page)
    await focusTerm(page)
  })

  // Pi's actual output shape (verified by running pi under a PTY and
  // replaying the raw stream through pyte): each content row is content
  // text followed by *explicit* space cells (literal codepoint-32 fill
  // to col 79), then `\r\n`. No vertical borders. Horizontal rules
  // (U+2500) separate sections.
  //
  // The trouble cells are the trailing spaces before the newline: they
  // have codepoint 32, so xterm.js's translateToString(trimRight=true)
  // (which keys off codepoint 0) leaves them in place. Selecting and
  // copying yields a wall of spaces.
  const cols = 80
  const piRow = (s: string) => ` ${s}` + ' '.repeat(cols - s.length - 1)
  const piTranscript = [
    piRow('hello world'),
    piRow('Hello! How can I help you today?'),
  ]

  test('selecting through pi chat output copies only the source text',
    async ({ page }) => {
      const range = await injectLines(page, piTranscript)
      await selectRows(page, range.startRow, range.endRow)
      await page.keyboard.press('Control+Shift+KeyC')

      const clipboard = await lastClipboardWrite(page)
      expect(clipboard).toBe(' hello world\n Hello! How can I help you today?')
    },
  )

  // Single-row selection. Verifies the trim happens per-row, not only
  // at the seam between rows. xterm's selectLineAt sets end.x === cols,
  // so this is also the triple-click case for a content row.
  test('selecting a single pi row strips its trailing pad',
    async ({ page }) => {
      const range = await injectLines(page, piTranscript)
      await selectRows(page, range.endRow, range.endRow)
      await page.keyboard.press('Control+Shift+KeyC')

      const clipboard = await lastClipboardWrite(page)
      expect(clipboard).toBe(' Hello! How can I help you today?')
    },
  )
})

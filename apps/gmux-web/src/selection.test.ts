import { describe, expect, it } from 'vitest'
import {
  selectionToText,
  type SelectionBufferLine,
  type SelectionTerminal,
} from './selection'

/**
 * Build a fake terminal where each row has explicitly-written content
 * followed by never-written cells (the row's "pad"). The fake's
 * `translateToString` honors `trimRight` the same way ghostty-web does: it
 * drops trailing never-written cells from the slice, but leaves
 * explicitly-typed spaces alone. This keeps the tests faithful to the
 * real cell-codepoint distinction the algorithm relies on.
 *
 * Selection coordinates use ghostty-web's convention:
 *   end.x is INCLUSIVE (the last selected column, 0-indexed).
 * This matches `getSelectionPosition()` from ghostty-web's SelectionManager.
 */
function makeTerm(opts: {
  cols: number
  rows: string[]
  wrapped?: number[]
  selection?: { start: { x: number, y: number }, end: { x: number, y: number } }
  /** Number of leading rows that are scrollback (default 0). */
  scrollbackLen?: number
  /** Lines scrolled back from bottom (default 0 = at bottom). */
  viewportY?: number
}): SelectionTerminal {
  const wrapped = new Set(opts.wrapped ?? [])
  const lines: SelectionBufferLine[] = opts.rows.map((written, i) => {
    const writtenLen = Math.min(written.length, opts.cols)
    const padded = written.padEnd(opts.cols, ' ').slice(0, opts.cols)
    return {
      isWrapped: wrapped.has(i),
      translateToString(trim, start = 0, end = opts.cols) {
        if (!trim) return padded.slice(start, end)
        // Mirror xterm.js: trimRight drops never-written cells (those past
        // `writtenLen`) from the slice's right edge. Written content,
        // including any trailing typed spaces, is preserved.
        const cap = Math.min(end, writtenLen)
        return cap <= start ? '' : padded.slice(start, cap)
      },
    }
  })
  return {
    cols: opts.cols,
    buffer: { active: { getLine: y => lines[y] } },
    getSelectionPosition: () => opts.selection,
    getScrollbackLength: () => opts.scrollbackLen ?? 0,
    getViewportY: () => opts.viewportY ?? 0,
  }
}

describe('selectionToText', () => {
  it('returns empty string when nothing is selected', () => {
    const term = makeTerm({ cols: 10, rows: ['hello'] })
    expect(selectionToText(term)).toBe('')
  })

  it('drops trailing pad when selection crosses a line break', () => {
    // ghostty-web end.x is inclusive. end=(3,1) selects 'echo' (cols 0-3) from
    // line 1. The non-last row (line 0) must have its trailing pad stripped.
    const term = makeTerm({
      cols: 20,
      rows: ['echo 1', 'echo 2'],
      selection: { start: { x: 0, y: 0 }, end: { x: 3, y: 1 } },
    })
    expect(selectionToText(term)).toBe('echo 1\necho')
  })

  it('preserves trailing pad when selection ends inside a row past content', () => {
    // User explicitly drags past EOL but stops before end of row. Those
    // cells render as spaces and are part of the user's selection, not
    // part of the line break.
    const term = makeTerm({
      cols: 20,
      rows: ['echo 1'],
      selection: { start: { x: 0, y: 0 }, end: { x: 8, y: 0 } },
    })
    expect(selectionToText(term)).toBe('echo 1   ')
  })

  it('trims pad on triple-click / line select (end.x === cols-1, single row)', () => {
    // ghostty-web full-row selection gives end.x === cols-1 (inclusive last
    // column). After the +1 conversion ex === cols, triggering the boundary
    // trim that drops trailing pad. Without this a TUI row copies as a wall
    // of spaces.
    const term = makeTerm({
      cols: 20,
      rows: ['echo 1'],
      selection: { start: { x: 0, y: 0 }, end: { x: 19, y: 0 } },
    })
    expect(selectionToText(term)).toBe('echo 1')
  })

  it('strips explicitly-written trailing whitespace at row boundaries', () => {
    // The shipped rule: when a row's selection reaches the row boundary,
    // trim trailing whitespace including explicitly-written spaces
    // (codepoint 32). Without this, TUIs that paint a full-width row of
    // content + space-fill + newline (pi, Claude Code, btop, …) copy as a
    // wall of spaces.
    //
    // The trade-off (rare): a row ending with typed trailing spaces, then
    // a hard newline, copies without those spaces. Pinning that
    // explicitly here so any future relaxation is a deliberate decision,
    // not a quiet revert.
    const term = makeTerm({
      cols: 20,
      rows: ['echo 1   ', 'echo 2'],
      selection: { start: { x: 0, y: 0 }, end: { x: 5, y: 1 } },
    })
    expect(selectionToText(term)).toBe('echo 1\necho 2')
  })

  it('strips trailing pad on a TUI-style row (full-width explicit space fill)', () => {
    // Mirrors what pi and similar TUIs render: content followed by an
    // explicit-space fill all the way to the right edge, then a hard
    // newline. Verified against pi's actual output (raw stream, replayed
    // through pyte). The boundary-reaching trim must drop those spaces
    // even though they have codepoint 32.
    const term = makeTerm({
      cols: 80,
      rows: [
        ' Hello! How can I help you today?                                               ',
      ],
      // ghostty-web inclusive: last col of 80-col row = 79.
      selection: { start: { x: 0, y: 0 }, end: { x: 79, y: 0 } },
    })
    expect(selectionToText(term)).toBe(' Hello! How can I help you today?')
  })

  it('preserves typed trailing spaces when selection ends mid-row', () => {
    // Companion to the boundary-trim test above: when the user stops
    // *inside* the row, the spaces they explicitly dragged across are
    // content (semantically a mid-row selection of pad), not part of a
    // line break. The mid-row case must keep them.
    const term = makeTerm({
      cols: 20,
      rows: ['echo 1   '],
      selection: { start: { x: 0, y: 0 }, end: { x: 8, y: 0 } },
    })
    expect(selectionToText(term)).toBe('echo 1   ')
  })

  it('joins soft-wrapped rows without inserting a newline', () => {
    const term = makeTerm({
      cols: 10,
      rows: ['sudo npm i', 'nstall expr'],
      wrapped: [1],
      // ghostty-web inclusive: last col of 10-col row = 9.
      selection: { start: { x: 0, y: 0 }, end: { x: 9, y: 1 } },
    })
    // Row 1 is "nstall expr" truncated to cols=10 → "nstall exp"
    expect(selectionToText(term)).toBe('sudo npm install exp')
  })

  it('keeps blank lines that are part of the selection', () => {
    const term = makeTerm({
      cols: 10,
      rows: ['line a', '', 'line b'],
      // ghostty-web inclusive: 'line b' = 6 chars, last col = 5.
      selection: { start: { x: 0, y: 0 }, end: { x: 5, y: 2 } },
    })
    expect(selectionToText(term)).toBe('line a\n\nline b')
  })

  it('handles a selection that starts mid-row and ends mid-row across rows', () => {
    const term = makeTerm({
      cols: 20,
      rows: ['hello world', 'foo bar baz'],
      // ghostty-web inclusive: 'foo bar' = 7 chars, last col = 6.
      selection: { start: { x: 6, y: 0 }, end: { x: 6, y: 1 } },
    })
    expect(selectionToText(term)).toBe('world\nfoo bar')
  })

  // ── Coordinate-offset regression tests ────────────────────────────────────
  //
  // ghostty-web's getSelectionPosition() returns viewport-RELATIVE row coords
  // (y=0 = first visible line).  buffer.active.getLine() expects ABSOLUTE rows
  // (y=0 = oldest scrollback line).  selectionToText() must offset by
  // scrollbackLen - getViewportY() so it reads the right lines.

  it('reads from viewport rows not scrollback rows when scrollback is present', () => {
    // Buffer: [sb0, sb1, sb2, vp0, vp1].  scrollbackLen=3, gvY=0 (at bottom).
    // Selection viewport-relative y ∈ {0, 1} should resolve to absolute rows 3, 4.
    // Without the offset fix, getLine(0) and getLine(1) would return sb0/sb1.
    const term = makeTerm({
      cols: 10,
      rows: [
        'sb0',       // absolute 0 (scrollback)
        'sb1',       // absolute 1 (scrollback)
        'sb2',       // absolute 2 (scrollback)
        'viewport0', // absolute 3 (viewport row 0 at bottom)
        'viewport1', // absolute 4 (viewport row 1 at bottom)
      ],
      scrollbackLen: 3,
      viewportY: 0,
      selection: { start: { x: 0, y: 0 }, end: { x: 8, y: 1 } },
    })
    expect(selectionToText(term)).toBe('viewport0\nviewport1')
  })

  it('reads from the correct absolute rows when scrolled back', () => {
    // Buffer: [sb0, sb1, sb2, vp0, vp1].  scrollbackLen=3, gvY=2 (scrolled back 2).
    // Viewport shows absolute rows 1, 2, 3 (sb1, sb2, vp0).
    // Selection viewport-relative y=0 → absolute 1 (sb1), y=1 → absolute 2 (sb2).
    // absoluteRow = 3 + viewportRow - 2.
    const term = makeTerm({
      cols: 10,
      rows: [
        'sb0',  // absolute 0
        'sb1',  // absolute 1 (viewport row 0 when gvY=2)
        'sb2',  // absolute 2 (viewport row 1 when gvY=2)
        'vp0',  // absolute 3 (viewport row 2 when gvY=2)
        'vp1',  // absolute 4
      ],
      scrollbackLen: 3,
      viewportY: 2,
      selection: { start: { x: 0, y: 0 }, end: { x: 2, y: 1 } },
    })
    expect(selectionToText(term)).toBe('sb1\nsb2')
  })

  it('does not insert a newline after the last selected row', () => {
    const term = makeTerm({
      cols: 10,
      rows: ['only line'],
      // ghostty-web inclusive: 'only line' = 9 chars, last col = 8.
      selection: { start: { x: 0, y: 0 }, end: { x: 8, y: 0 } },
    })
    expect(selectionToText(term)).toBe('only line')
  })

  it('drops trailing pad on every non-last row when selection spans many rows', () => {
    const term = makeTerm({
      cols: 20,
      rows: ['a', 'bb', 'ccc'],
      // ghostty-web inclusive: 'ccc' = 3 chars, last col = 2.
      selection: { start: { x: 0, y: 0 }, end: { x: 2, y: 2 } },
    })
    expect(selectionToText(term)).toBe('a\nbb\nccc')
  })
})

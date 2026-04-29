import { describe, expect, it } from 'vitest'
import {
  selectionToText,
  type SelectionBufferLine,
  type SelectionTerminal,
} from './selection'

/**
 * Build a fake terminal where each row has explicitly-written content
 * followed by never-written cells (the row's "pad"). The fake's
 * `translateToString` honors `trimRight` the same way xterm.js does: it
 * drops trailing never-written cells from the slice, but leaves
 * explicitly-typed spaces alone. This keeps the tests faithful to the
 * real cell-codepoint distinction the algorithm relies on.
 *
 * Selection coordinates use the same 0-based, half-open-on-x convention
 * as `Terminal.getSelectionPosition()`.
 */
function makeTerm(opts: {
  cols: number
  rows: string[]
  wrapped?: number[]
  selection?: { start: { x: number, y: number }, end: { x: number, y: number } }
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
  }
}

describe('selectionToText', () => {
  it('returns empty string when nothing is selected', () => {
    const term = makeTerm({ cols: 10, rows: ['hello'] })
    expect(selectionToText(term)).toBe('')
  })

  it('drops trailing pad when selection crosses a line break', () => {
    const term = makeTerm({
      cols: 20,
      rows: ['echo 1', 'echo 2'],
      selection: { start: { x: 0, y: 0 }, end: { x: 0, y: 1 } },
    })
    expect(selectionToText(term)).toBe('echo 1\n')
  })

  it('preserves trailing pad when selection ends inside a row past content', () => {
    // User explicitly drags past EOL but stops before end of row. Those
    // cells render as spaces and are part of the user's selection, not
    // part of the line break.
    const term = makeTerm({
      cols: 20,
      rows: ['echo 1'],
      selection: { start: { x: 0, y: 0 }, end: { x: 9, y: 0 } },
    })
    expect(selectionToText(term)).toBe('echo 1   ')
  })

  it('trims pad on triple-click / line select (end.x === cols, single row)', () => {
    // xterm's selectLineAt sets end.x = cols. We must treat that as
    // "selection reached the row boundary" and drop the pad — otherwise
    // every triple-click copies a wall of spaces. Regression test for the
    // bug introduced when first trying the naive "preserve pad on last
    // row" rule.
    const term = makeTerm({
      cols: 20,
      rows: ['echo 1'],
      selection: { start: { x: 0, y: 0 }, end: { x: 20, y: 0 } },
    })
    expect(selectionToText(term)).toBe('echo 1')
  })

  it('preserves explicitly-typed trailing spaces across a line break', () => {
    // Distinguishing written-spaces from never-written pad is the whole
    // point of using trimRight=true (which keys off cell codepoint, not
    // the rendered character). If the user typed `echo 1   ` followed
    // by Enter, those three spaces are content and must survive a copy
    // that crosses the resulting line break.
    const term = makeTerm({
      cols: 20,
      rows: ['echo 1   ', 'echo 2'],
      selection: { start: { x: 0, y: 0 }, end: { x: 6, y: 1 } },
    })
    expect(selectionToText(term)).toBe('echo 1   \necho 2')
  })

  it('joins soft-wrapped rows without inserting a newline', () => {
    const term = makeTerm({
      cols: 10,
      rows: ['sudo npm i', 'nstall expr'],
      wrapped: [1],
      selection: { start: { x: 0, y: 0 }, end: { x: 10, y: 1 } },
    })
    // Row 1 is "nstall expr" truncated to cols=10 → "nstall exp"
    expect(selectionToText(term)).toBe('sudo npm install exp')
  })

  it('keeps blank lines that are part of the selection', () => {
    const term = makeTerm({
      cols: 10,
      rows: ['line a', '', 'line b'],
      selection: { start: { x: 0, y: 0 }, end: { x: 6, y: 2 } },
    })
    expect(selectionToText(term)).toBe('line a\n\nline b')
  })

  it('handles a selection that starts mid-row and ends mid-row across rows', () => {
    const term = makeTerm({
      cols: 20,
      rows: ['hello world', 'foo bar baz'],
      selection: { start: { x: 6, y: 0 }, end: { x: 7, y: 1 } },
    })
    expect(selectionToText(term)).toBe('world\nfoo bar')
  })

  it('does not insert a newline after the last selected row', () => {
    const term = makeTerm({
      cols: 10,
      rows: ['only line'],
      selection: { start: { x: 0, y: 0 }, end: { x: 9, y: 0 } },
    })
    expect(selectionToText(term)).toBe('only line')
  })

  it('drops trailing pad on every non-last row when selection spans many rows', () => {
    const term = makeTerm({
      cols: 20,
      rows: ['a', 'bb', 'ccc'],
      selection: { start: { x: 0, y: 0 }, end: { x: 3, y: 2 } },
    })
    expect(selectionToText(term)).toBe('a\nbb\nccc')
  })
})

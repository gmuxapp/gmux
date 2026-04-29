import { describe, expect, it } from 'vitest'
import {
  selectionToText,
  type SelectionBufferLine,
  type SelectionTerminal,
} from './selection'

/** Build a fake terminal where each row is a fixed-width string padded with
 * spaces to `cols`. `wrapped` lists the y-indices that are continuations
 * of the row above (i.e. soft-wrapped rows). Selection range is in the same
 * coordinate space the test passes; the algorithm doesn't care whether it's
 * 0- or 1-based as long as inputs are consistent. */
function makeTerm(opts: {
  cols: number
  rows: string[]
  wrapped?: number[]
  selection?: { start: { x: number, y: number }, end: { x: number, y: number } }
}): SelectionTerminal {
  const wrapped = new Set(opts.wrapped ?? [])
  const lines: SelectionBufferLine[] = opts.rows.map((raw, i) => {
    const padded = raw.padEnd(opts.cols, ' ').slice(0, opts.cols)
    return {
      isWrapped: wrapped.has(i),
      translateToString(_trim, start = 0, end = opts.cols) {
        return padded.slice(start, end)
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
    // User triple-clicks line 0, drag extends to start of line 1.
    const term = makeTerm({
      cols: 20,
      rows: ['echo 1', 'echo 2'],
      selection: { start: { x: 0, y: 0 }, end: { x: 0, y: 1 } },
    })
    expect(selectionToText(term)).toBe('echo 1\n')
  })

  it('preserves trailing spaces when selection ends mid-row inside the pad', () => {
    // User explicitly drags past EOL but stops before end of row. Those
    // spaces are part of the selection, not part of the line break.
    const term = makeTerm({
      cols: 20,
      rows: ['echo 1'],
      selection: { start: { x: 0, y: 0 }, end: { x: 9, y: 0 } },
    })
    expect(selectionToText(term)).toBe('echo 1   ')
  })

  it('joins soft-wrapped rows without inserting a newline', () => {
    // Long shell command terminal-wrapped across two rows.
    const term = makeTerm({
      cols: 10,
      rows: ['sudo npm i', 'nstall expr'],
      wrapped: [1],
      selection: { start: { x: 0, y: 0 }, end: { x: 10, y: 1 } },
    })
    // Row 1 is "nstall expr" truncated to cols=10 → "nstall exp"
    expect(selectionToText(term)).toBe('sudo npm install exp')
  })

  it('keeps blank lines when they are part of the selection', () => {
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

  it('emits no padding for fully blank intermediate rows', () => {
    // Three-row selection where the middle row is empty — the trailing
    // pad of an empty row is the entire row, which must collapse to "".
    const term = makeTerm({
      cols: 30,
      rows: ['top', '', 'bottom'],
      selection: { start: { x: 0, y: 0 }, end: { x: 6, y: 2 } },
    })
    expect(selectionToText(term)).toBe('top\n\nbottom')
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

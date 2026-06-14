import { describe, expect, it } from 'vitest'
import { dropTrailingBlankRows, readTerminalText, rowAtClientY, type TextTerminal } from './terminal-text'

/** Fake terminal whose rows are plain strings; translateToString returns
 * the row verbatim (the real one trims never-written cells — our rows are
 * already concrete, so we exercise the post-strip and blank-row logic). */
function fakeTerm(rows: (string | undefined)[]): TextTerminal {
  return {
    buffer: {
      active: {
        length: rows.length,
        getLine: y => {
          const row = rows[y]
          return row === undefined ? undefined : { translateToString: () => row }
        },
      },
    },
  }
}

describe('dropTrailingBlankRows', () => {
  it('drops only trailing blanks, preserving interior ones', () => {
    expect(dropTrailingBlankRows(['a', '', 'b', '', ''])).toEqual(['a', '', 'b'])
  })
  it('returns empty for an all-blank buffer', () => {
    expect(dropTrailingBlankRows(['', '', ''])).toEqual([])
  })
  it('leaves a buffer with no trailing blanks untouched', () => {
    expect(dropTrailingBlankRows(['a', 'b'])).toEqual(['a', 'b'])
  })
})

describe('readTerminalText', () => {
  it('strips trailing whitespace from each row and trailing blank rows', () => {
    const lines = readTerminalText(fakeTerm(['hello   ', 'world\t', '   ', '']))
    expect(lines).toEqual(['hello', 'world'])
  })
  it('preserves interior blank rows and leading whitespace', () => {
    const lines = readTerminalText(fakeTerm(['  indented', '', 'after', '']))
    expect(lines).toEqual(['  indented', '', 'after'])
  })
  it('treats missing lines as blank', () => {
    expect(readTerminalText(fakeTerm(['a', undefined, 'b']))).toEqual(['a', '', 'b'])
  })
})

describe('rowAtClientY', () => {
  it('maps a press to viewportY plus the row offset within the screen', () => {
    expect(rowAtClientY({ clientY: 100, screenTop: 20, cellHeight: 16, viewportY: 50 })).toBe(55)
  })
  it('returns the top visible row for a press on the first row', () => {
    expect(rowAtClientY({ clientY: 24, screenTop: 20, cellHeight: 16, viewportY: 50 })).toBe(50)
  })
  it('goes below viewportY for a press above the screen top', () => {
    expect(rowAtClientY({ clientY: 4, screenTop: 20, cellHeight: 16, viewportY: 50 })).toBe(49)
  })
})

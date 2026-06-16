/**
 * Snapshot the terminal buffer as plain text for the long-press text
 * sheet, where native selection gives real mobile copy.
 *
 * The pure pieces (row extraction, blank-row trimming, point→row math)
 * live here so they can be unit-tested with hand-rolled fakes, mirroring
 * selection.ts. `pressedBufferRow` is the one DOM-touching wrapper.
 */
import type { Terminal } from '@xterm/xterm'

/** Minimal buffer surface needed to read rows as text. Subset of
 * `@xterm/xterm`'s public API, redeclared locally for testability. */
export interface TextBufferLine {
  translateToString(trimRight: boolean, startColumn?: number, endColumn?: number): string
}
export interface TextBuffer {
  readonly length: number
  getLine(y: number): TextBufferLine | undefined
}
export interface TextTerminal {
  readonly buffer: { readonly active: TextBuffer }
}

/**
 * Read the whole buffer (scrollback + viewport) as one trimmed string
 * per visual row, with trailing blank rows dropped.
 *
 * Each row is trimmed on the right two ways, same as selection.ts:
 * `translateToString(trimRight=true)` drops never-written cells
 * (codepoint 0, shell output), and the post-strip removes explicit
 * trailing spaces that TUIs pad rows with (pi, Claude Code, btop, …).
 * Rows aren't reflowed — one buffer row per line matches what the user
 * saw on screen and makes the scroll-to-press land exactly.
 */
export function readTerminalText(term: TextTerminal): string[] {
  const buf = term.buffer.active
  const lines: string[] = []
  for (let y = 0; y < buf.length; y++) {
    const line = buf.getLine(y)
    lines.push(line ? line.translateToString(true).replace(/[ \t]+$/, '') : '')
  }
  return dropTrailingBlankRows(lines)
}

/** Drop empty rows from the end so the sheet doesn't open into a sea of
 * blank lines. Blank rows between content are preserved. */
export function dropTrailingBlankRows(lines: string[]): string[] {
  let end = lines.length
  while (end > 0 && lines[end - 1] === '') end--
  return lines.slice(0, end)
}

/**
 * Absolute buffer row under a vertical client coordinate. Pure: the
 * caller supplies the measured screen top, cell height, and the buffer's
 * current top visible row (`viewportY`). The result indexes the array
 * from {@link readTerminalText} (clamp it to that array's bounds).
 */
export function rowAtClientY(args: {
  clientY: number
  screenTop: number
  cellHeight: number
  viewportY: number
}): number {
  const { clientY, screenTop, cellHeight, viewportY } = args
  return viewportY + Math.floor((clientY - screenTop) / cellHeight)
}

/** Resolve the buffer row under a touch's clientY, reading layout from
 * the live terminal. Returns 0 when dimensions aren't measured yet. */
export function pressedBufferRow(term: Terminal, clientY: number): number {
  const screen = term.element?.querySelector('.xterm-screen')
  const cellHeight = term.dimensions?.css.cell.height
  if (!screen || !cellHeight) return 0
  return rowAtClientY({
    clientY,
    screenTop: screen.getBoundingClientRect().top,
    cellHeight,
    viewportY: term.buffer.active.viewportY,
  })
}

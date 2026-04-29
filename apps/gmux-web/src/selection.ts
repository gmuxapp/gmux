/**
 * Convert an xterm.js selection to a clipboard string.
 *
 * Terminal-buffer-to-text models, summarised:
 *
 * 1. "Trim everything" — strip trailing whitespace from every copied line.
 *    Loses information when the user genuinely wants to copy padding (e.g.
 *    selecting inside a TUI cell). Ghostty's `clipboard-trim-trailing-spaces`
 *    option works this way and its own community considers it a smell
 *    (ghostty-org/ghostty#2709).
 *
 * 2. "Trim never" — emit cells verbatim. Wall of trailing spaces after every
 *    short line. xterm.js' default `getSelection()` lands here in many cases.
 *
 * 3. **"Trailing pad belongs to the line break"** — Terminal.app, Alacritty,
 *    WezTerm, GNOME Terminal, xterm. Selections that *cross* a row boundary
 *    drop the trailing pad of the row before emitting `\n`. Selections that
 *    *end inside* a row preserve the cells the user actually dragged across,
 *    spaces included. This is what people mean when they say "no trailing
 *    whitespace on copy", and it's the model gmux implements here.
 *
 * Soft wraps (terminal-driven, signalled by `IBufferLine.isWrapped` on the
 * following row) are joined without an inserted `\n`, matching every modern
 * terminal. TUI-driven wraps (Vim, tmux, Claude Code, pi, …) are
 * indistinguishable from hard newlines in the buffer and therefore copy as
 * separate lines. No terminal can recover those without app cooperation.
 */

/** Minimal terminal surface needed for selection-to-text. Matches the shape
 * of `@xterm/xterm`'s public API but typed locally so the algorithm can be
 * unit-tested with hand-rolled fakes. */
export interface SelectionBufferLine {
  readonly isWrapped: boolean
  translateToString(trimRight: boolean, startColumn?: number, endColumn?: number): string
}

export interface SelectionBuffer {
  getLine(y: number): SelectionBufferLine | undefined
}

export interface SelectionTerminal {
  readonly cols: number
  readonly buffer: { readonly active: SelectionBuffer }
  getSelectionPosition(): { start: { x: number, y: number }, end: { x: number, y: number } } | undefined
}

/**
 * Render the active selection as a clipboard string using the
 * trailing-pad-as-line-break model. Returns `''` when there is no selection.
 */
export function selectionToText(term: SelectionTerminal): string {
  const range = term.getSelectionPosition()
  if (!range) return ''

  const { start, end } = range
  const buffer = term.buffer.active
  const cols = term.cols

  let out = ''
  for (let y = start.y; y <= end.y; y++) {
    const line = buffer.getLine(y)
    if (!line) continue

    const isLast = y === end.y
    const isFirst = y === start.y
    const sx = isFirst ? start.x : 0
    const ex = isLast ? end.x : cols

    // Logical content extent: last non-space column on the row. The cells
    // beyond this are the row's "trailing pad" and conceptually belong to
    // the line break, not to the line.
    const full = line.translateToString(false, 0, cols)
    let contentEnd = full.length
    while (contentEnd > 0 && full.charCodeAt(contentEnd - 1) === 0x20) contentEnd--

    // Last selected row: emit cells exactly as the user dragged. If they
    // dragged into the trailing pad we keep those spaces — that's an
    // explicit selection of padding, distinct from crossing a line break.
    // Earlier rows: clip to contentEnd so the pad is dropped before \n.
    const cutEx = isLast ? ex : Math.min(ex, Math.max(contentEnd, sx))
    if (cutEx > sx) out += line.translateToString(false, sx, cutEx)

    if (!isLast) {
      // Soft wrap: next row continues this one, suppress the newline.
      const next = buffer.getLine(y + 1)
      if (!next?.isWrapped) out += '\n'
    }
  }
  return out
}

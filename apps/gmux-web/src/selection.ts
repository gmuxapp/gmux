/**
 * Convert an xterm.js selection to a clipboard string.
 *
 * Terminal-buffer-to-text models, summarised:
 *
 * 1. "Trim everything" — strip trailing whitespace from every copied line.
 *    Loses information when the user genuinely wants to copy spaces typed
 *    into a row. Ghostty's `clipboard-trim-trailing-spaces` option works
 *    this way and its own community considers it a smell
 *    (ghostty-org/ghostty#2709).
 *
 * 2. "Trim never" — emit cells verbatim. Wall of trailing spaces after
 *    every short line, because the cells past the content render as
 *    spaces. xterm.js' default `getSelection()` lands here in many cases.
 *
 * 3. **"Trailing pad belongs to the line break"** — Terminal.app, Alacritty,
 *    WezTerm, GNOME Terminal, xterm. Selections that *cross* a row boundary
 *    drop the trailing pad of the row before emitting `\n`. Selections that
 *    *end inside* a row preserve the cells the user actually dragged
 *    across, spaces included. This is what people mean when they say "no
 *    trailing whitespace on copy", and it's the model gmux implements.
 *
 * Why xterm.js' `trimRight` parameter is the right primitive: it trims by
 * cell *codepoint* (0 = never written) rather than by rendered character,
 * so it cleanly distinguishes never-written pad cells from explicitly-typed
 * spaces, and is automatically wide-character-aware.
 *
 * Soft wraps (terminal-driven, signalled by `IBufferLine.isWrapped` on the
 * following row) are joined without an inserted `\n`, matching every modern
 * terminal. TUI-driven wraps (Vim, tmux, Claude Code, pi, …) are
 * indistinguishable from hard newlines in the buffer and copy as separate
 * lines. No terminal can recover those without app cooperation.
 *
 * Coordinate notes (xterm.js 6.x): `getSelectionPosition` returns 0-based
 * `x` and `y`, where `y` is an absolute buffer row index (compatible with
 * `buffer.active.getLine`) and `x` is half-open on the right (`x === cols`
 * means "past last column"). The public typings claim 1-based, but the
 * runtime behavior is 0-based; verified against the bundled source.
 */

/** Minimal terminal surface needed for selection-to-text. Subset of
 * `@xterm/xterm`'s public API, redeclared locally so the algorithm can be
 * unit-tested with hand-rolled fakes. */
export interface SelectionBufferLine {
  readonly isWrapped: boolean
  /** Slice the line as a string. With `trimRight=true`, never-written
   * trailing cells inside the slice are dropped (pad stripping). Written
   * spaces are preserved either way. */
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
    const sx = y === start.y ? start.x : 0
    const ex = isLast ? end.x : cols

    // Trim trailing pad when the row's selection reaches the row boundary
    // (`ex >= cols`). That covers every non-last row, plus the last row
    // when the user selected past EOL (triple-click, line-select, drag
    // through the line break). When the user stopped *inside* the row we
    // preserve the cells they dragged across, including any pad spaces:
    // that's an explicit selection of padding, semantically distinct from
    // crossing a line break.
    const trim = ex >= cols
    if (ex > sx) out += line.translateToString(trim, sx, ex)

    if (!isLast) {
      // Soft wrap: next row continues this one, suppress the newline.
      const next = buffer.getLine(y + 1)
      if (!next?.isWrapped) out += '\n'
    }
  }
  return out
}

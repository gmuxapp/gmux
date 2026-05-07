/**
 * Convert an xterm.js selection to a clipboard string.
 *
 * Terminal-buffer-to-text models, summarised:
 *
 * 1. "Trim everything": strip trailing whitespace from every copied line
 *    unconditionally. Ghostty's `clipboard-trim-trailing-spaces` option.
 *    Loses information when the user explicitly drag-selects through pad
 *    inside a row.
 *
 * 2. "Trim never": emit cells verbatim. Wall of trailing spaces after
 *    every short line, because the cells past the content render as
 *    spaces. xterm.js' default `getSelection()` lands here in many cases.
 *
 * 3. **"Trailing whitespace belongs to the line break"**: trim trailing
 *    whitespace only when the selection reaches the row boundary;
 *    preserve mid-row pad the user explicitly dragged across. This is the
 *    model gmux implements. It handles two different cases that look the
 *    same to the buffer:
 *
 *      a. Shell output (`echo hello`) leaves the rest of the row as
 *         never-written cells (codepoint 0). xterm.js's
 *         `translateToString(trimRight=true)` drops these by walking the
 *         line until it finds a cell with `HAS_CONTENT_MASK`.
 *
 *      b. TUI output (pi, Claude Code, btop, fzf, lazygit, k9s, …) fills
 *         each row with *explicit* space cells (codepoint 32) before the
 *         newline. Those cells survive xterm's codepoint-0 trim because
 *         they have content. Without an extra step, copying any TUI
 *         response yields a wall of spaces.
 *
 *    For (b) we post-strip ASCII whitespace from the right edge of every
 *    boundary-reaching slice. Trade-off: a row that ends with
 *    explicitly-typed trailing whitespace, then a hard newline, copies
 *    without those spaces. In practice that's vanishingly rare. The
 *    `trim = ex >= cols` guard still preserves spaces a user explicitly
 *    drags through without crossing the line break.
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

    // Trim trailing whitespace when the row's selection reaches the row
    // boundary (`ex >= cols`). That covers every non-last row, plus the
    // last row when the user selected past EOL (triple-click,
    // line-select, drag through the line break). When the user stopped
    // *inside* the row we preserve the cells they dragged across,
    // including any pad spaces.
    //
    // Two layers of trimming, both gated on `trim`:
    //   1. xterm's translateToString(trimRight=true) drops never-written
    //      cells (codepoint 0). Handles shell output cleanly.
    //   2. We post-strip ASCII whitespace from the right edge. Handles
    //      TUIs that fill rows with explicit space cells before the
    //      newline (pi, CC, btop, …). See file-level docstring for the
    //      full rationale and the rare case it loses fidelity in.
    const trim = ex >= cols
    if (ex > sx) {
      const slice = line.translateToString(trim, sx, ex)
      out += trim ? slice.replace(/[ \t]+$/, '') : slice
    }

    if (!isLast) {
      // Soft wrap: next row continues this one, suppress the newline.
      const next = buffer.getLine(y + 1)
      if (!next?.isWrapped) out += '\n'
    }
  }
  return out
}

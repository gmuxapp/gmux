# gmux wrap-provenance patch

This directory is the unmodified source of `github.com/charmbracelet/x/vt` at
`v0.0.0-20260330094520-2dce04b6f8a4`, plus one isolated terminal-state change
intended for upstreaming.

## Delta

- `Screen` has one `[]bool` bit per visible row. `Emulator.Wrapped(y)` exposes
  read-only access to the active buffer's bit.
- `Scrollback` carries a parallel wrap slice. `PushWrapped` records the bit and
  `Wrapped(index)` exposes read-only access. The old public `PushN` operation,
  which accepted a metadata-free `RenderBuffer`, is replaced by private,
  screen-aware `pushN`; full-row transfers therefore cannot silently lose wrap
  provenance. Wrapped pushes preserve the complete source row, including
  trailing blank cells consumed at the wrap boundary; only terminating rows
  trim unused padding. Truncation, reflow, and clearing keep metadata aligned.
- `utf8.go` marks a row only when a subsequent grapheme consumes pending
  phantom autowrap. Filling the final cell alone does not mark it because CR,
  cursor movement, and controls can cancel phantom state.
- Full-width insert/delete/scroll operations move bits with their rows;
  partial-width vertical operations clear the affected ambiguous provenance.
  Whole-row erase/fill and reset clear bits. Height-only resize preserves
  surviving row bits. On a width change the normal buffer groups scrollback
  and visible rows into logical lines, preserves consumed cells and cursor
  offset, and rewraps them at the new width without splitting wide graphemes.
  It then repartitions the result between bounded scrollback and the new
  viewport. This matches xterm.js: non-reflowing history cannot preserve both
  word and grid fidelity, while reflow ensures every retained wrap bit refers
  to exactly its row's stored width. The alternate buffer remains
  non-reflowing and clears its wrap bits on width changes.
- Full-width scroll regions carry the discarded rows' bits into normal
  scrollback, matching vt's existing history behavior. Normal and alternate
  screens retain independent visible-row bits; public scrollback remains the
  normal buffer's.
- `wrap_test.go` covers phantom confirmation/cancellation, DECAWM off,
  bottom-row scrolling, wide and combining graphemes, margins, row edits,
  alternate-buffer isolation, normal-buffer reflow across the
  scrollback/viewport boundary, boundary spaces, wide and styled cells,
  cursor remapping, blank lines, scrollback bounds, height-only resize
  preservation, RIS, and ED3.

No parser, cell, rendering, or hyperlink representation was replaced. In
particular, `ultraviolet.Cell.Link` and existing ANSI rendering remain intact.

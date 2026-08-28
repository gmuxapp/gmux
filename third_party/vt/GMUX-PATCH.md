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
- `utf8.go` fixes an upstream wide-write bug: with one column remaining,
  autowrap now moves a width-2 grapheme to the next row instead of passing it
  to `RenderBuffer` to be clipped and silently lost. The skipped column is an
  explicit zero cell, distinct from an `EmptyCell` written space. Reflow drops
  only these trailing synthetic skips and recreates them when a wide grapheme
  again wraps early. Ordinary pending phantom autowrap is still marked only
  when a subsequent grapheme consumes it; exactly-fitting wide cells compute
  phantom state from their post-write edge.
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
  preservation, RIS, and ED3. `BenchmarkReflow2000ScrollbackLines` measured
  16.1 ms per 2,000-line reflow on an Intel i7-9700K (`-benchtime=5x`).
  Amortizing repeated reflows caused by `shrinkForReconnect` remains a known
  follow-up.

No parser, cell, rendering, or hyperlink representation was replaced. In
particular, `ultraviolet.Cell.Link` and existing ANSI rendering remain intact.

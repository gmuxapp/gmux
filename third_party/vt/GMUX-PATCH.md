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
  provenance. Truncation, resizing, and clearing keep the slices aligned.
- `utf8.go` marks a row only when a subsequent grapheme consumes pending
  phantom autowrap. Filling the final cell alone does not mark it because CR,
  cursor movement, and controls can cancel phantom state.
- Full-width insert/delete/scroll operations move bits with their rows;
  partial-width vertical operations clear the affected ambiguous provenance.
  Whole-row erase/fill and reset clear bits. Resize preserves surviving row
  bits, matching the underlying buffer's current non-reflowing resize.
- Full-width scroll regions carry the discarded rows' bits into normal
  scrollback, matching vt's existing history behavior. Normal and alternate
  screens retain independent visible-row bits; public scrollback remains the
  normal buffer's.
- `wrap_test.go` covers phantom confirmation/cancellation, DECAWM off,
  bottom-row scrolling, wide and combining graphemes, margins, row edits,
  alternate-buffer isolation, resize, RIS, and ED3.

No parser, cell, rendering, or hyperlink representation was replaced. In
particular, `ultraviolet.Cell.Link` and existing ANSI rendering remain intact.

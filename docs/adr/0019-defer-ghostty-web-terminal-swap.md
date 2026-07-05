# ADR 0019: Defer switching the web terminal from xterm.js to ghostty-web

**Status:** Accepted (deferred — not currently pursued)
**Date:** 2026-07-04

## Context

A working prototype (branch `prototype/ghostty-web`, "Prototype 6") swapped the
web terminal engine from xterm.js to `ghostty-web` (Coder's WASM wrapper
around Ghostty's VT parser) behind a `?ghostty=1` runtime feature flag,
keeping the xterm.js path intact for A/B comparison. The swap itself was
~270 LOC because ghostty-web's API is deliberately xterm-compatible; the
prototype passed build, typecheck, and the full unit-test suite.

### What the swap would buy us

- Deletes several hard-won mobile hacks that ghostty handles natively:
  the ~200-line iOS/Android autocorrect interceptor (`mobile-input.ts`),
  the hidden-textarea hop-and-restore focus dance, the font-preload gate,
  and `measureTerminalFit` precision juggling.
- Replaces `@xterm/addon-web-links` with built-in OSC8 + URL-regex
  handling; drops the WebGL/canvas renderer fallback dance.
- Terminal fidelity (escape sequences, Unicode, OSC8, complex scripts)
  would track upstream Ghostty rather than xterm.js.
- WASM parser is synchronous, which may obsolete the write-queue
  resize-serialization in `createTerminalIO` (unverified).

### What blocks shipping it

- **Inline images are a hard regression with no path**: ghostty-web has no
  sixel/iTerm image support, which would kill our `@gmux/addon-image`
  feature (not just the fork's maintenance burden). Upstreaming image
  support is the only honest fix.
- **OSC52 clipboard set** needs a byte-stream interceptor (ghostty exposes
  no parser hooks) — ~50 LOC, workable.
- Ports needed for parity: BSU/ESU scroll preservation (1–2 days),
  resize echo gate + "sized for another device" pill, custom keybind
  handler, E2E test hooks (`__gmuxTerm`, `__gmuxInject`).
- Unverified on-device behavior: iOS autocorrect duplication, iOS focus
  scroll-jump, Canvas-2D rendering performance vs xterm's WebGL on heavy
  output.
- No public `dispose()` in ghostty-web's d.ts — likely small per-session
  leak; needs an upstream issue.

## Decision

We stay on xterm.js. The ghostty-web direction is **deferred, not
rejected on the merits**: the prototype proved the renderer swap is
mechanically cheap, but the image regression, the parity-porting work,
and the unanswered real-device questions make it a poor trade right now.

## Consequences

- The xterm.js-specific mobile workarounds (`mobile-input.ts`,
  hop-and-restore focus, font preload gate) remain maintained code.
- The `@gmux/addon-image` fork remains ours to maintain.
- If revisited, start from the prototype's full engineering notes
  (`NOTES.md` on the `prototype/ghostty-web` branch), whose deferred-work
  table enumerates the parity gaps with effort estimates. The suggested
  order there still stands: port the easy pieces behind the flag, do a
  real-device pass (iPhone Safari + Android Chrome), then decide on
  images before deleting either engine path.

# Prototype 6 — Switch terminal from xterm.js to ghostty-web

**Branch goal:** prove that swapping the terminal engine from xterm.js
to `ghostty-web` (Coder's WASM-Ghostty wrapper) is mechanically feasible
in our app, and surface the concrete list of things that would need to
follow before it could ship. The xterm.js implementation is left in
place; a runtime feature flag picks between the two so we can A/B them.

## How to evaluate

```bash
# build / typecheck / tests already pass on this branch.
cd .grove/switch-to-ghostty
pnpm dev

# Then open the app and append ?ghostty=1 to the URL, e.g.
#   http://localhost:5173/projects/foo/sessions/bar?ghostty=1
# That sets localStorage.gmux_ghostty=1 so subsequent loads stay on
# ghostty. Reset with `?ghostty=0`.
```

## What's wired up

1. **Dependency** — `pnpm add ghostty-web@0.4.0` (zero runtime deps,
   ~400 KB WASM blob).
2. **WASM loading** — `import ghosttyWasmUrl from
   'ghostty-web/ghostty-vt.wasm?url'`. Vite emits the WASM as a
   content-hashed asset; `Ghostty.load(ghosttyWasmUrl)` consumes it.
   Verified: `pnpm build` emits
   `dist/assets/ghostty-vt-<hash>.wasm` (414 KB). Dev server transforms
   resolve the import to a `?import&url` query that Vite serves.
3. **Singleton** — one `Ghostty` instance shared across all Terminal
   mounts (`ghosttyLoadPromise`).
4. **New component** — `apps/gmux-web/src/terminal-ghostty.tsx`
   exports `TerminalViewGhostty` with the *same prop shape* as the
   existing xterm.js `TerminalView`. Drop-in compatible at the call
   site.
5. **Feature flag in `main.tsx`** — module-load constant `useGhostty`
   reads `?ghostty=...` URL param (one-shot) or
   `localStorage.gmux_ghostty` (persistent), and aliases `TerminalView`
   to either implementation. No conditional rendering at the call site;
   one bundle, runtime switch.
6. **End-to-end loop** — WS connect → `term.write(bytes)` for output;
   `term.onData(cb)` → `ws.send(bytes)` for input; `term.onResize` →
   server `{type:'resize',cols,rows}` exactly matching the existing
   protocol. WS reconnect with exponential backoff. Replay buffer
   (`replay.ts`) is reused as-is.
7. **Mobile bar wiring** — `onInputReady`, `onPasteReady`, `onFocusReady`
   callbacks fire the same way, so the existing mobile bottom bar
   (esc/tab/ctrl/alt/arrows/send/paste) still works without changes.
8. **Bracketed paste** — `term.paste(text)` from `onPasteReady` auto-
   wraps with `\x1b[200~` / `\x1b[201~` if the app is in bracketed mode
   (ghostty's own `hasBracketedPaste()` check). Replaces the
   `handlePasteAction(...)` path; binary paste isn't ported (text only).
9. **Hyperlinks** — OSC8 + URL-regex providers are registered by
   ghostty's `Terminal` constructor by default. No `WebLinksAddon`
   needed. Link activation goes through ghostty's `handleClick`.
10. **Theme / font** — `terminalOptions.theme`, `fontSize`, `fontFamily`,
   `cursorBlink`, `cursorStyle`, `scrollback` are passed through. Theme
   color keys align 1:1 between xterm and ghostty.
11. **Resize** — `FitAddon` from `ghostty-web` + a local
   `ResizeObserver` on the container, plus an rAF debounce. No echo
   gate (see deferred list).

## What's deferred or regressed (must address before shipping)

These are the gaps you can directly attribute to the engine swap, with
the level of effort to close each.

### Hacks that *go away* (the wins)

- **`mobile-input.ts`** — the 200-line iOS / Android autocorrect
  interceptor. ghostty has its own `beforeinput` handling in its
  `InputHandler`. **Not wired in this prototype.** Worth a real-device
  test to confirm the cascading-duplicates bug doesn't recur; if it
  does, we'd need to port (or contribute upstream).
- **Hidden-textarea hop-and-restore** (`focusTerminalInput` in
  `terminal.tsx:120-160`) — ghostty's textarea is already
  `clip-path: inset(50%)` and sits inside the canvas parent; the iOS
  off-screen-focus scroll-jump may not happen. **Not used in the
  prototype**; `term.focus()` is called directly.
- **Font preload dance** (`document.fonts.load(spec)` gate before
  `term.open()`) — ghostty re-measures via `remeasureFont()` and a
  render loop, so a late-arriving font fixes itself. *Not strictly
  proven*, but our existing `fontReady` gate is dropped in the ghostty
  path; the prototype mounts immediately.
- **`measureTerminalFit` precision juggling** — ghostty's FitAddon
  owns this. Removed.
- **`@xterm/addon-web-links`** — replaced by ghostty's built-in OSC8
  + URL-regex providers. One less addon.
- **WebGL renderer + canvas fallback** — ghostty is Canvas 2D only,
  no fallback dance.

### Regressions (functionality lost in this prototype)

| Feature | Status | Notes |
|---|---|---|
| **Image addon (sixel / iTerm)** | **Regression, no path.** | ghostty-web has no image support. Inline image escape sequences will be parsed and dropped. If gmux users rely on image output (`pi /image`, sixel from `chafa`, etc.) this is a blocker. Upstreaming image support to ghostty-web is the only honest fix. |
| **OSC52 clipboard set** | Stubbed | Currently we register `term.parser.registerOscHandler(52, ...)`. ghostty exposes no parser hook. Workable: intercept the WS byte stream before `term.write()` and pull OSC52 out with a small state-machine. ~50 LOC. |
| **BSU/ESU scroll preservation** (`terminal-io.ts`) | Stubbed | Different buffer-row model; the anchor logic in `createTerminalIO` doesn't port directly. TUI redraws (vim, htop) may scroll unexpectedly. Effort: 1–2 days to port; the anchor concept itself transfers. |
| **Resize echo gate** ("sized for another device" pill) | Stubbed | Prototype trusts server resize echoes and locally calls `term.resize()`. Multi-driver UX (the pill, reclaim button) is gone. Effort: small; just port the existing logic since it's renderer-agnostic. |
| **`attachKeyboardHandler` custom keybinds** | Stubbed | ghostty has `attachCustomKeyEventHandler(ev => bool)` with the same shape as xterm. Direct port is straightforward. The armed Ctrl/Alt mobile-bar modifiers *do* still work — we wrap `sendInput` in `terminal-ghostty.tsx` exactly like the xterm path. |
| **OSC52 read** | Not applicable | We don't implement clipboard read today either. |
| **`createTerminalIO` write queue** | Stubbed | Bytes go straight through `term.write(bytes, cb)`. Loses the resize-serialization that prevents async-parser races during simultaneous writes + resizes. ghostty's WASM parser is synchronous (no setTimeout in the parser loop), so the original race might not exist; needs verification. If safe, this is a *simplification* win. |
| **Test hooks** (`__gmuxTerm`, `__gmuxInject`) | Not exposed | E2E `terminal-scroll.spec.ts` relies on these. Either wire them or skip the spec while on ghostty. |
| **`reset()` between sessions** | Used | ghostty's `Terminal.reset()` exists and is called. |
| **Disposal** | Best-effort | No `dispose()` in the public d.ts; we drop refs and let GC handle. Likely a small leak per session swap in this prototype; worth a maintainer issue upstream. |
| **`@gmux/addon-image`** fork (sixel) | Gone with the swap | We currently maintain a fork of the xterm image addon. Switching kills that maintenance burden but also kills the feature. |

### Behaviors I'm not yet sure about (need device verification)

- **Mobile autocorrect / IME**. ghostty has its own `beforeinput`
  handling and a clip-path-hidden textarea. Whether it suffers the
  same iOS replacement-text duplication our `mobile-input.ts`
  patches around is *not* documented anywhere I could find. Easy test:
  type on an iPhone with the prototype and see.
- **iOS focus race on `term.focus()`**. ghostty calls
  `textarea.focus()` directly; we don't know if it suppresses the
  iOS scroll-jump the way our hop-and-restore does. Test on device.
- **Performance** on large outputs. ghostty's renderer is Canvas 2D
  with dirty-line tracking. xterm.js with WebGL is generally faster
  on heavy redraw (think `cat largefile`). Probably fine for normal
  TUI use, but worth benchmarking against btop / a large `git log`
  scroll.

## Bundle impact

- xterm.js path: ~280 KB JS + 0 KB WASM (current).
- ghostty-web path: ~? KB JS + ~414 KB WASM (gzipped: WASM is
  generally about 1.5–2× more compressible than minified JS, so
  net wire weight is roughly comparable).
- Both engines currently coexist in the prototype build, hence the
  1.4 MB JS bundle warning. Once xterm.js is removed (post-prototype),
  the JS bundle should shrink considerably; the WASM ships separately
  and is HTTP-cached.

## Architecture observations

- `ghostty-web`'s public API is **very** xterm-compatible by design,
  which is why this prototype is ~270 LOC. The hard work is in the
  *deferred* list, not in the renderer swap itself.
- The "WASM-Ghostty parser, JS renderer + I/O" split feels right for
  a long-term direction: terminal fidelity (escape sequence handling,
  Unicode, OSC8, complex scripts) tracks upstream Ghostty, while
  rendering / input / clipboard can stay in our hands.
- The OSC52 + image-addon gaps are the real ship-blockers. Everything
  else on the deferred list is "port the existing logic with light
  edits", linear engineering rather than open research.

## Suggested follow-up plan

If the device test results look promising, the path I'd suggest:

1. **Port the easy pieces** behind the same feature flag, on this
   branch or a follow-on:
   - OSC52 byte-stream interceptor
   - `attachKeyboardHandler` → `attachCustomKeyEventHandler`
   - Resize echo gate + pill
   - BSU/ESU scroll preservation
   - Test hooks (`__gmuxTerm`, `__gmuxInject`)
2. **Real-device pass**: iPhone Safari + Android Chrome, with the
   mobile prototypes (3/4/5) layered on top to see if the combined
   UX is better than xterm + our existing hacks.
3. **Decide on images.** Either upstream sixel/iTerm protocol support
   to ghostty-web, or accept the regression and rely on out-of-band
   image display (browser-side preview pane for `gmux image`, etc.).
4. **Once parity is reached**, delete the `?ghostty=` flag and the
   xterm.js path. That's the moment the simplifications actually
   materialize as deletions.

## Files changed

- `apps/gmux-web/package.json` — `ghostty-web@^0.4.0` added.
- `apps/gmux-web/pnpm-lock.yaml` — lockfile updated.
- `apps/gmux-web/src/terminal-ghostty.tsx` — **new**, parallel
  TerminalView powered by ghostty-web.
- `apps/gmux-web/src/main.tsx` — feature flag + import switch (a
  ~20-line module-scope block); no JSX changes.

## Build / test / lint

- `npx tsc --noEmit` — clean
- `npx vitest run` — 420/420 pass (no new tests added; the prototype
  is integration code best validated by real-device evaluation)
- `npx vite build` — 1.27 s, emits `ghostty-vt-<hash>.wasm` (414 KB)
- `npx biome check src/terminal-ghostty.tsx` — clean after auto-fix

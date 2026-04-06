---
bump: minor
---

### Terminal defaults

- **New default cursor.** The cursor is now a steady vertical bar when focused
  and a hollow outline when unfocused (was: blinking block). Configurable via
  `cursorStyle`, `cursorBlink`, and `cursorInactiveStyle` in `settings.jsonc`.
- **Better selection visibility.** The text selection highlight color has more
  contrast against the dark background.
- **Bold text no longer shifts colors.** `drawBoldTextInBrightColors` now
  defaults to `false`, matching modern terminals (kitty, WezTerm, Alacritty).
  Bold text renders as heavier font weight instead of switching to bright ANSI
  colors.
- **macOS Option key sends Meta by default.** `macOptionIsMeta` now defaults
  to `true`, so Alt-based readline shortcuts (Alt+B, Alt+F) work out of the
  box. Set to `false` in `settings.jsonc` to restore special character input.

### Bug fixes

- **Ctrl+Backspace and Ctrl+Delete now work on Linux.** The browser was
  swallowing these events on xterm's hidden textarea. They now send the
  standard terminal sequences.
- **`sendKeys` action handles modifier keys correctly.** Custom keybinds using
  `sendKeys` with `ctrl`, `shift`, or `alt` on special keys (arrows, Home,
  End, Delete, etc.) now produce the correct CSI escape sequences instead of
  ignoring the modifiers.
- **Future-proofed platform detection.** Replaced the deprecated
  `navigator.platform` API with `navigator.userAgentData.platform` (with
  fallback) for Mac detection.

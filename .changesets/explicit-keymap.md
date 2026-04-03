---
bump: minor
---

### Keyboard shortcuts

- **Explicit default keymap.** Every keyboard shortcut is now defined
  explicitly rather than relying on implicit xterm.js or browser passthrough.
  The defaults are split by platform (Linux/Windows and Mac) and cover
  clipboard, navigation, and browser-stolen key workarounds.

- **New actions: `copy`, `paste`, `selectAll`.** `copy` always copies the
  selection (no SIGINT fallback). `paste` reads the system clipboard via the
  Clipboard API. `selectAll` selects all terminal content. These join the
  existing `sendText`, `sendKeys`, `copyOrInterrupt`, and `none`.

- **Ctrl+Shift+C/V on Linux.** The standard Linux terminal copy/paste
  shortcuts now work out of the box.

- **Ctrl+V no longer leaks `\x16`.** Previously, xterm.js sent a
  quoted-insert byte to the terminal before the browser's paste event
  arrived. Paste is now intercepted in the keymap so this no longer happens.

- **`macCommandIsCtrl` option.** A single config flag that makes every
  Cmd+character send its Ctrl equivalent on Mac. Cmd+Shift combinations
  produce CSI u (Kitty keyboard protocol) sequences. Cmd+Left/Right/Backspace
  keep their navigation behavior. Set it in `keybinds.jsonc`:

  ```jsonc
  { "macCommandIsCtrl": true }
  ```

- **`keybinds.jsonc` object format.** The file now accepts either an array
  (backward compatible) or an object with `macCommandIsCtrl` and `bindings`
  fields.

- **Expanded key names.** `keyComboToSequence` and event matching now support
  arrow keys, `pageup`/`pagedown`, `insert`, and short aliases (`esc`, `del`,
  `ins`, `page_up`, `page_down`).

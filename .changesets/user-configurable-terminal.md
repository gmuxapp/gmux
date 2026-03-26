---
bump: minor
---

- **User-configurable terminal settings.** Three config files in `~/.config/gmux/` let you customize the terminal without rebuilding:
  - `theme.jsonc`: colors, font, cursor style, scrollback, and more. Drop in a [Windows Terminal theme](https://github.com/mbadolato/iTerm2-Color-Schemes/tree/master/windowsterminal) and it works out of the box.
  - `keybinds.jsonc`: remap keys the browser steals (Ctrl+T, Ctrl+N, Ctrl+W) to PTY sequences via `sendKeys`, send raw text with `sendText`, or disable built-in bindings with `"none"`.
  - `config.toml`: unchanged, still handles gmuxd behavior (port, network, tailscale).
- **No restart required.** Config files are read from disk on each page load, so edit and refresh.

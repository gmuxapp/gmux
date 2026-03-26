---
bump: minor
---

- **User-configurable terminal settings.** Two new config files in `~/.config/gmux/` let you customize the terminal without rebuilding:
  - `theme.jsonc`: colors, font, cursor style, scrollback, and more. Drop in a [Windows Terminal theme](https://github.com/mbadolato/iTerm2-Color-Schemes/tree/master/windowsterminal) and it works out of the box.
  - `keybinds.jsonc`: remap keys the browser steals (Ctrl+T, Ctrl+N, Ctrl+W) to PTY sequences via `sendKeys`, send raw text with `sendText`, or disable built-in bindings with `"none"`. A virtual `secondary` modifier resolves to Cmd on macOS and Ctrl elsewhere for cross-platform keybinds.
- **Platform-aware default keybinds.** On Linux, Ctrl+Alt+T/N/W send Ctrl+T/N/W (workaround for browser-stolen shortcuts). On Mac, Cmd+Left/Right send Home/End, Cmd+Backspace deletes to start of line, and Cmd+K clears the screen, matching iTerm2 conventions.
- **No restart required.** Config files are read from disk on each page load, so edit and refresh.

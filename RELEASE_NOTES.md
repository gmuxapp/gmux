- **Fixed dev instances unable to connect to their backend.** The `gmux` CLI
  now reads `GMUXD_PORT` directly (same env var as `gmuxd`), so a single
  export is sufficient for both binaries. The redundant `GMUXD_ADDR` env var
  has been removed.

- **Dev instances now isolate pi session storage.** The pi adapter respects
  `PI_CODING_AGENT_DIR`, and the dev scripts export it to an instance-specific
  directory. Sessions launched from a dev instance no longer appear in (or
  pollute) your real pi session history. ([#39](https://github.com/gmuxapp/gmux/pull/39))
- **Nested gmux detection.** Running `gmux <command>` inside an existing gmux
  session no longer creates a PTY-within-PTY nest. Instead, the session is
  launched in the background and appears in the gmux UI automatically. ([#37](https://github.com/gmuxapp/gmux/pull/37))
- **User-configurable terminal settings.** Two new config files in `~/.config/gmux/` let you customize the terminal without rebuilding:
  - `theme.jsonc`: terminal color palette. Drop in a [Windows Terminal theme](https://github.com/mbadolato/iTerm2-Color-Schemes/tree/master/windowsterminal) and it works out of the box.
  - `settings.jsonc`: terminal options (font, cursor, scrollback), keybinds, and other frontend preferences. Remap keys the browser steals (Ctrl+T, Ctrl+N, Ctrl+W) to PTY sequences via `sendKeys`, send raw text with `sendText`, or disable built-in bindings with `"none"`.
- **Config file renamed.** `config.toml` is now `host.toml` to better reflect its purpose (daemon/host behavior).
- **Platform-aware default keybinds.** On Linux, Ctrl+Alt+T/N/W send Ctrl+T/N/W (workaround for browser-stolen shortcuts). On Mac, Cmd+Left/Right send Home/End, Cmd+Backspace deletes to start of line, and Cmd+K clears the screen, matching iTerm2 conventions.
- **No restart required.** Config files are read from disk on each page load, so edit and refresh. ([#35](https://github.com/gmuxapp/gmux/pull/35))

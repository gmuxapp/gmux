- **Clickable links in the terminal.** URLs in terminal output are now detected
  and open in a new tab on click. OSC 8 hyperlinks emitted by programs also
  work without a confirmation dialog. ([#29](https://github.com/gmuxapp/gmux/pull/29))
- Fixed scroll position jumping when a terminal resize coincides with a synchronized update (BSU/ESU) block, most visible on mobile when the virtual keyboard opens during heavy output.
- **Fix scrollback for TUI apps.** Pi and claude sessions now retain conversation content. ([#27](https://github.com/gmuxapp/gmux/pull/27))
- **Fixed workspace grouping for adapter sessions (pi, codex).** Resumable sessions
  discovered from session files now resolve their git/jj workspace root correctly. ([#25](https://github.com/gmuxapp/gmux/pull/25))
- **Network listener for containers and VPNs.** gmuxd can now bind to a network address beyond localhost, protected by a bearer token. Set `GMUXD_LISTEN=10.0.0.5` or add `[network] listen = "10.0.0.5"` to your config file. Browsers get a login page; programmatic clients use the `Authorization: Bearer` header. Run `gmuxd auth-link` to see the token and a ready-to-scan URL for mobile devices. ([#31](https://github.com/gmuxapp/gmux/pull/31))
- **Sessions from the same directory now appear next to each other.** Within a
  workspace folder, sessions are grouped by their working directory instead of
  being interleaved by creation time. ([#28](https://github.com/gmuxapp/gmux/pull/28))

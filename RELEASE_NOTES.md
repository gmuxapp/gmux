**Breaking changes**
- Config files split and renamed: `config.toml` → `host.toml` (daemon/network), `keybinds.jsonc` merged into `settings.jsonc`, and `theme.jsonc` restricted to colors only. The `[network]` block and legacy `GMUXD_PORT`/`ADDR`/`SOCKET` env vars are removed; TCP bind address is now controlled exclusively via `GMUXD_LISTEN`.
- Local IPC switched to a permission-restricted Unix socket (`~/.local/state/gmux/gmuxd.sock`). The TCP listener now requires bearer token auth on every request. CLI commands updated to `gmuxd start|stop|status|auth`, with `start` always replacing existing instances.

**Features**
- Sidebar folders replaced by explicit, server-synced projects stored in `projects.json` and broadcast via SSE. Matching uses filesystem paths or normalized remote URLs with longest-prefix precedence. Hierarchical, bookmarkable URLs (`/<project>/<adapter>/<slug>`) use stable session slugs derived from resume keys or commands.
- VCS remote detection normalizes SSH/HTTPS/SC URLs to `host/owner/repo`. Sessions sharing any remote (including upstream/fork or worktrees) group together, falling back to workspace root for repos without remotes.
- Explicit, platform-split default keymap with `copy`, `paste`, and `selectAll` actions. `keybinds.jsonc` supports object format with a `macCommandIsCtrl` toggle that maps Cmd+char to Ctrl+char while preserving Cmd+arrow/navigation. Standard `Ctrl+Shift+C/V` paste works on Linux, and raw `\x16` injection on paste is eliminated.
- OSC 52 support allows terminal applications (`pi`, `tmux`, `vim`) to write to the system clipboard. Mobile UI uses touch detection (`pointer: coarse`), moves submission to a toolbar send button, preserves `\n` during non-bracketed paste, and correctly emits `CSI u` sequences for Ctrl+Shift combinations.
- Status indicators decoupled from raw PTY output. Unread states are blue and adapter-controlled (triggered on assistant turn completion). Working displays as a pulsating hollow ring, while raw shell output emits a transient activity indicator that auto-fades after 3 seconds. Arrival animations trigger on all unread transitions.

**Fixes**
- Session replay replaced the 128KB raw ring buffer with a `midterm` VT100 virtual terminal. TUI spinner frames now update a single cell instead of flushing the buffer, guaranteeing correct screen, color, and cursor state on reconnect.
- Scrollback ring buffer now flushes bare CR characters instead of discarding them, preserving cursor-positioning sequences for differential renders. On buffer wrap, snapshots trim to the last frame-start marker (`ESC[?2026h` or `ESC[H`) instead of the first newline, preventing stacked/duplicate lines in `pi-tui` and `bubbletea` sessions.
- Mobile URL tapping fixed by deferring `scrollToBottom()` to the next event loop iteration, allowing xterm.js linkifier synthesized mouse events to resolve before the viewport shifts.

---

### Breaking
- refactor!: simplify config layout (host.toml, settings.jsonc, theme.jsonc) ([#64](https://github.com/gmuxapp/gmux/pull/64))

### Features
- feat: detect VCS remotes, group sessions by shared remote URL ([#41](https://github.com/gmuxapp/gmux/pull/41))
- changeset: unix-socket-ipc security and architecture changes ([#43](https://github.com/gmuxapp/gmux/pull/43))
- Redesign status indicators: adapter-controlled unread, transient activity, recolored badges ([#46](https://github.com/gmuxapp/gmux/pull/46))
- feat: project management with server-side state, URL routing, and session lifecycle ([#50](https://github.com/gmuxapp/gmux/pull/50))
- feat(keybinds): explicit keymap with macCommandIsCtrl option ([#56](https://github.com/gmuxapp/gmux/pull/56))

### Fixes
- Fix: allow link taps on mobile by deferring scrollToBottom ([#44](https://github.com/gmuxapp/gmux/pull/44))
- feat(web): handle OSC 52 clipboard ([#48](https://github.com/gmuxapp/gmux/pull/48))
- fix(ringbuf): flush bare CR and trim to frame-start on wrap ([#51](https://github.com/gmuxapp/gmux/pull/51))
- fix: use virtual terminal for session replay instead of ring buffer ([#68](https://github.com/gmuxapp/gmux/pull/68))

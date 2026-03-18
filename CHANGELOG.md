# Changelog

Full commit-level changelogs are on each [GitHub release](https://github.com/gmuxapp/gmux/releases). This file tracks user-facing highlights only.

## v0.4.1

### Terminal

- **Serialized terminal writes** — xterm writes and resizes are queued so the async parser never races with resize (fixes image addon + resize glitches)
- **Stable fit measurement** — terminal size is now measured from the shell viewport directly instead of relying on FitAddon's container measurement, which was affected by terminal content, scrollbars, and sidebar state
- **Unified resize model** — `terminal_resize` event replaces the old `resize_state` / `claim_resize` protocol; resize pill is derived from viewport vs PTY size mismatch
- **Connect takes over resize** — opening the UI on a device automatically sizes the terminal to fit, instead of starting passive

### Mobile

- **Tap to focus** — tapping the terminal opens the soft keyboard on mobile (works around xterm's hidden textarea not triggering keyboard activation)
- **Keyboard-aware layout** — the app tracks `visualViewport` height so the terminal resizes above the on-screen keyboard instead of being covered by it
- **Passive panning** — when a session is sized for another device, drag to pan the oversized terminal in the browser; tap still focuses
- **Accessory bar keeps keyboard open** — tapping esc, tab, ctrl, arrows, or enter no longer dismisses the soft keyboard

### Developer experience

- **Dev environment** — `scripts/dev.sh` starts an isolated dev instance with vite HMR, Go file watching, and Tailscale access on a separate port

## v0.4.0

### Breaking changes

- **`gmuxd` is now subcommand-based** — run `gmuxd start`, `gmuxd shutdown`, `gmuxd version`, or `gmuxd help`; bare `gmuxd` now prints help instead of starting the daemon
- **`gmuxd start` is now strict by default** — if another daemon is already running, it fails unless you pass `--replace`

## v0.3.0

Sidebar redesign, file attribution fixes, integration tests.

- **Sidebar split into live + resumable sections** — live sessions at top, "Resume previous" collapsible drawer for resumable sessions
- **Simplified close button** — always ×; live sessions kill, resumable sessions dismiss
- **Stale session cleanup** — dead runners detected and cleaned up automatically
- **File attribution fixes** — single-candidate attribution now works correctly for pi; title derived from first user message even when the tool creates the file before user input
- **Title from first user message** — adapters no longer emit title events on every message; initial title set once via file parsing
- **`close_action` removed** — field was redundant; frontend derives behavior from `session.alive`
- **`CommandTitler` interface** — optional adapter capability for custom fallback titles (shell shows `pytest -x` instead of "shell")
- **WebGL terminal renderer** — switched from canvas to WebGL for better performance
- **Terminal resize handoff polish** — the "sized for another device" hint is now a compact floating pill and no longer affects terminal height when taking resize ownership
- **Integration tests** — end-to-end tests for pi, claude, codex, and shell that launch real tools through gmuxd
- Fixed macOS "app is damaged" Gatekeeper prompt

## v0.2.4

- File attribution refactored into adapter interface (`FileAttributor`)
- Codex adapter now attributes session files

## v0.2.3

- Fixed Homebrew tap (was empty after cleanup)

## v0.2.2

- Switched Homebrew distribution from cask to formula (install command unchanged)
- Fixed adapter title being lost after session resume
- Added analytics to website (cookieless, no banner)

## v0.2.1

State management rearchitecture. Session state flows through a single path (Register) instead of multiple creation paths racing. Frontend is a pure projection of backend state with no optimistic updates.

- Fixed resume race conditions (duplicate sessions, stale sockets, premature terminal attach)
- Dismissed sessions stay dismissed across scanner cycles
- Dead sessions no longer auto-selected
- Status labels null by default, shown only when informative
- Chrome app-mode launch fixed on macOS (direct binary call instead of `open -a`)
- New docs: state management, session schema

## v0.2.0

- **Claude Code and Codex adapters** with session file parsing, live status, and resume
- **Resumable sessions** — sessions transition between alive and resumable states seamlessly
- Empty state redesign with launcher buttons
- Fixed Chrome app-mode flag passing on macOS
- Resume race conditions and dismissed session reappearance fixed
- `Resumable` and `CloseAction` derived from adapter capabilities, not hardcoded

## v0.1.0

Initial release.

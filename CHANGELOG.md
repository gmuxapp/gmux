# Changelog

Full commit-level changelogs are on each [GitHub release](https://github.com/gmuxapp/gmux/releases). This file tracks user-facing highlights only.

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

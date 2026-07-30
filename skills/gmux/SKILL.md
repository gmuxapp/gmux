---
name: gmux
description: Run long-running commands synchronously or backgrounded through gmux sessions. Use when running a build, server, or other command that would benefit. Check its output, wait for it to finish or print something, or let the user watch.
---

Add `gmux --` before any command to run it in a session the user can watch live in a browser. Details: `gmux help`.

## Core flow

```bash
gmux -- pnpm build | tail          # run in the foreground, watchable live
id=$(gmux -d -- pnpm dev)           # run detached and capture the session id
gmux wait "$id"                    # block until the process exits
gmux wait "$id" --for-text 'listening on'   # ...or until output matches
gmux tail "$id" [-n N]             # last N lines of terminal output
gmux ls                            # list sessions
```

The `| tail` pattern is the default for long builds and test runs: the user watches the full output stream live in gmux, while you only take the last few lines: enough to see success or the failing tail without flooding your context.

## Rules of thumb

- `gmux -- <cmd>` and `gmux wait` can be composed based on success condition: `if gmux -- pytest; then ...`.
- For servers and watchers, capture the detached ID as above, gate readiness with `gmux wait "$id" --for-text`, and run `gmux kill "$id"` when done.
- With an ID captured as above, `gmux send "$id" 'text' Enter` types into an interactive session (a shell or TUI). Hotkey example: `C-c`

---
title: pi
description: How gmux works with the pi coding agent.
---

gmux has built-in support for [pi](https://github.com/mariozechner/pi-coding-agent). No configuration is needed — launch pi through gmux and everything works automatically.

## What you get

### Live status

The sidebar shows when pi is actively working. gmux loads a small extension into pi at launch which reports each turn boundary, so the agent shows as **working** (pulsing cyan dot) while it is processing and clears when the turn completes — no PTY scraping or log parsing.

### Session titles from conversations

Instead of showing "pi" for every session, the extension reports pi's session name — which pi auto-generates from the conversation (and you can change with pi's `/name` command):

```
▼ ~/dev/myapp
  ● Fix the auth bug in login.go
  ● Add pagination to the API
  ○ Refactor database layer
```

Renaming with pi's `/name` command updates the sidebar live.

### Resumable sessions

When a pi session exits, it remains in the sidebar as a resumable entry. Click it to resume exactly where you left off — gmux launches `pi --session <path> -c` with the right session file.

Resumable sessions are deduplicated: if you're already running a session that matches a resumable entry, only the live one appears.

### Launch from the UI

Pi appears in the launch menu only when it is available on the current machine. `gmuxd` checks this at startup by running `pi --version`; if that fails, the pi launcher is omitted from the UI.

## How it works

### Detection

There are two separate pi checks:

- **availability discovery** in `gmuxd`: run `pi --version` at startup to see whether pi is installed and the pi launcher should be shown
- **runtime matching** in `gmux`: scan the launched command for a `pi` or `pi-coding-agent` binary name

The runtime matching works with direct invocation, full paths, `npx`, `nix run`, and other wrappers:

```bash
gmux -- pi                           # ✓ matched
gmux -- /home/user/.local/bin/pi     # ✓ matched
gmux -- npx pi                       # ✓ matched
gmux -- echo "not pi"                # ✗ not matched
```

If detection fails (e.g., an unusual wrapper), override it:

```bash
GMUX_ADAPTER=pi gmux my-pi-wrapper
```

### Session files

Pi stores conversations as JSONL files in `~/.pi/agent/sessions/`. Each working directory gets its own subfolder with an encoded name:

```
~/.pi/agent/sessions/
  --home-mg-dev-myapp--/
    2026-03-15T10-00-00-000Z_abc123.jsonl
    2026-03-15T11-30-00-000Z_def456.jsonl
```

gmuxd indexes these files for conversation search and history. Live session state — attribution, title, and status — comes from the extension, not from parsing these files. The first line of each file is a session header with a UUID and timestamp.

### The gmux extension

When gmux owns the launch, it injects the gmux session extension into pi (`pi -e <materialized-extension>`; extensions accumulate, so it coexists with your own). The extension subscribes to pi's own lifecycle and reports state to the runner authoritatively — no inference:

- **`session_start`** (fires on startup *and* on every `/new`, `/resume`, and `/fork`) reports the active conversation file, id, and name. This is what binds a session to its file, and it's the only signal that survives selecting an already-loaded session from pi's `/resume` picker — pi serves that from memory without touching disk, so there is nothing for an external heuristic to observe. (This replaces the old scrollback content-matching; see ADR 0011.)
- **`agent_start` / `agent_end`** report each turn, so gmux drives status without watching the file.

### Status

The extension reports each turn with a normalized, agent-agnostic outcome; gmux maps it to the sidebar:

- **turn start** → working (pulsing cyan dot)
- **completed** → idle, marked unread
- **aborted** (you pressed Esc) → idle
- **error** (pi exhausted its auto-retries) → red dot; clears when you view the session or send a new message

### Disabling the extension

If a pi release ever breaks the extension, set `GMUX_NO_AGENT_HOOK=1` to launch pi without it. Pi runs normally; gmux just won't show hook-driven title/status/attribution until you unset it (or a fix ships). One-shot pi commands (`pi update`, `pi list`, `pi --help`, …) are never extended — gmux runs them directly rather than wrapping them in a session.

## Limitations

- **Title appears after the first turn.** Pi generates the session name once the first response completes, so a brand-new session shows a generic title until then.
- **The extension only loads when gmux controls the launch.** A shell-wrapped invocation (e.g. `bash -lc "pi …"`) doesn't receive the `-e` flag, so that session won't report hook-driven state.

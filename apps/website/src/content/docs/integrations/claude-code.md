---
title: Claude Code
description: How gmux works with Claude Code.
---

gmux has built-in support for [Claude Code](https://docs.anthropic.com/en/docs/claude-code). No configuration is needed — launch Claude Code through gmux and everything works automatically.

## What you get

### Live status

The sidebar shows when Claude Code is actively working. gmux injects a small hook configuration (via `--settings`) when it launches `claude`; the hooks report turn boundaries directly to gmux, so status is authoritative rather than inferred from output or file writes. A user prompt sets the status to **working** (pulsing cyan dot); the end of the turn clears it.

### Session titles

Instead of showing "claude" for every session, gmux extracts a meaningful title:

```
▼ ~/dev/myapp
  ● Fix the auth bug in login.go
  ● Add pagination to the API
  ○ Refactor database layer
```

Title priority:
1. Claude Code's own session title (reported by the hook, or the `custom-title` entry in the conversation file) — renaming with `/rename` shows up at your next prompt, and the URL slug follows the resolved title
2. The text of your first message
3. Otherwise the session falls back to its working directory / adapter name

### Resumable sessions

When a Claude Code session exits, it remains in the sidebar as a resumable entry. Click it to resume — gmux launches `claude --resume <session-id>`.

Resumable sessions are deduplicated: if you're already running a session that matches a resumable entry, only the live one appears.

### Launch from the UI

Claude Code appears in the launch menu only when the `claude` binary is on `PATH`. `gmuxd` checks this at startup; if not found, the Claude Code launcher is omitted from the UI.

## How it works

### Detection

- **Availability discovery** in `gmuxd`: `LookPath("claude")` at startup
- **Runtime matching** in `gmux`: scan the launched command for a `claude` binary name

The runtime matching works with direct invocation, full paths, and wrappers:

```bash
gmux -- claude                       # ✓ matched
gmux -- /usr/bin/claude              # ✓ matched
gmux -- env claude                   # ✓ matched
gmux -- echo "not claude"            # ✗ not matched
```

`GMUX_ADAPTER` can force adapter selection, but only if the adapter's own matcher also accepts the command — the binary must still be named `claude` (e.g. a symlink `claude -> my-wrapper`). A wrapper with a different name is not matched and cannot be overridden.

### Hooks

When gmux owns the launch, it appends a `--settings` argument registering Claude Code hooks that run `gmux __claude-hook`. If you pass your own `--settings` or have hooks configured, gmux deep-merges: hook arrays concatenate, your scalar values win, and your settings can never wipe gmux's hook entries. Nothing is written to `~/.claude` — the injection is per-launch and ephemeral. Set `GMUX_NO_AGENT_HOOK=1` to launch claude unmodified (the session then runs without live status).

| Hook event | Effect |
|---|---|
| `SessionStart` | Binds the session to its conversation file, ID, and title (also fires on `/resume`, `/clear`, compaction — gmux follows the new transcript) |
| `UserPromptSubmit` | Working (cyan dot); also refreshes the title, so a `/rename` shows up at your next prompt |
| `Stop` | Idle — turn completed |
| `SessionEnd` | Idle — session ended/aborted (Ctrl+C, exit) |

Because turn state is authoritative, `gmux wait <id>` works reliably with Claude Code for scripting.

### Conversation files

Claude Code stores conversations (it calls them session transcripts) as JSONL files in `~/.claude/projects/`. Each working directory gets its own subfolder with an encoded name — `/` and `.` are replaced with `-`:

```
~/.claude/projects/
  -home-mg-dev-myapp/
    a1b2c3d4-e5f6-7890-abcd-ef1234567890.jsonl
    f9e8d7c6-b5a4-3210-fedc-ba0987654321.jsonl
  -home-mg--local-share-chezmoi/
    1192413d-098c-47d5-9cae-8f622ad29463.jsonl
```

Note the double dash in `-home-mg--local-share-chezmoi` — that's because `/home/mg/.local` has a dot that also becomes a dash.

gmuxd's Claude adapter watches these directories to discover past conversations and populate resumable entries. Each line in the file is a JSON object with a `type` field (`user`, `assistant`, `system`, `custom-title`, etc.). Live status and attribution do **not** come from these files — the hook binds the exact transcript path at `SessionStart`, so multiple sessions in one directory attribute correctly and immediately.

## Limitations

- **Status is coarse.** gmux doesn't distinguish thinking/tool-use sub-states — all are shown as "working". An Esc-interrupted turn stays "working" until the next prompt (Claude's `Stop` hook only fires on a clean finish).
- **Hook injection needs a visible `claude` binary.** A shell-wrapped launch (`gmux -- bash -c 'claude'`, or `claude` after a `--`) can't be extended, so that session runs without live status.

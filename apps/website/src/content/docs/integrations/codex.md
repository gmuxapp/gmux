---
title: Codex
description: How gmux works with OpenAI Codex CLI.
---

gmux has built-in support for [Codex CLI](https://developers.openai.com/codex/cli). No configuration is needed — launch Codex through gmux and everything works automatically.

## What you get

### Live status

The sidebar shows when Codex is actively working. gmux detects `user_message` and `task_complete` events in the session file — a user message sets the status to **working** (pulsing cyan dot), and a completed task clears it.

### Session titles

Instead of showing "codex" for every session, gmux extracts the text of your first prompt as the title:

```
▼ ~/dev/myapp
  ● Fix the auth bug in login.go
  ● Add pagination to the API
  ○ Refactor database layer
```

System-injected context (permissions, environment, AGENTS.md) is automatically filtered out so only your actual prompt text appears.

### Resumable sessions

When a Codex session exits, it remains in the sidebar as a resumable entry. Click it to resume — gmux launches `codex resume <session-id>`.

### Launch from the UI

Codex appears in the launch menu only when the `codex` binary is on `PATH`. `gmuxd` checks this at startup; if not found, the Codex launcher is omitted from the UI.

## How it works

### Detection

- **Availability discovery** in `gmuxd`: `LookPath("codex")` at startup
- **Runtime matching** in `gmux`: scan the launched command for a `codex` binary name

```bash
gmux -- codex                        # ✓ matched
gmux -- codex "Fix the auth bug"     # ✓ matched
gmux -- /usr/bin/codex               # ✓ matched
gmux -- echo "not codex"             # ✗ not matched
```

If detection fails, override it:

```bash
GMUX_ADAPTER=codex gmux my-codex-wrapper
```

### Session files

Codex stores sessions as JSONL files in `~/.codex/sessions/`, organized by date:

```
~/.codex/sessions/
  2026/
    03/
      17/
        rollout-2026-03-17T01-38-24-019cf93a-c782-7942-ab76-010c81df6744.jsonl
        rollout-2026-03-17T01-38-44-019cf93b-131e-7b80-9e2f-c247ad4704f4.jsonl
```

Unlike Claude Code and pi which organize by working directory, Codex uses a flat date-based structure. The working directory is stored inside each session's `session_meta` header.

gmuxd walks the full date tree to discover session files.

### Session file format

Each line is a JSON object with a `type` field:

| Line type | Purpose |
|---|---|
| `session_meta` | First line — session ID, cwd, timestamp, CLI version |
| `response_item` | Message content — user prompts, assistant responses, function calls |
| `event_msg` | Lifecycle events — `user_message`, `agent_message`, `task_complete`, `task_started` |
| `turn_context` | Per-turn metadata (model, instructions) |

### Status detection

gmux watches event_msg lines in the session file for status signals:

| Event type | Sidebar effect |
|---|---|
| `user_message` | Working (cyan dot) — assistant will respond |
| `task_complete` | Idle (dot clears) — turn complete |
| `turn_cancelled` / `turn_aborted` | Idle — turn ended early |

## Limitations

- **No custom titles.** Codex doesn't generate session titles like Claude Code does. gmux uses the first user prompt as the title.
- **Date-based storage.** Sessions aren't grouped by project. gmux scans all session files and matches them to working directories by reading the `session_meta` header.
- **Status is event-based.** gmux doesn't distinguish between "thinking", "running a command", or "writing code" — all are shown as "working" between `user_message` and `task_complete`.

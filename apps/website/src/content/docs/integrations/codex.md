---
title: Codex
description: How gmux works with OpenAI Codex CLI.
---

gmux has built-in support for [Codex CLI](https://developers.openai.com/codex/cli). No configuration is needed — launch Codex through gmux and everything works automatically. Live status requires Codex CLI **0.135.0 or newer**; older versions still launch, resume, and get titles from the transcript, but show no working/idle status.

## What you get

### Live status

The sidebar shows when Codex is actively working. gmux injects a lightweight hook into every Codex launch; Codex reports prompt-submitted and turn-finished events directly to gmux, so status is exact — no file polling.

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

`GMUX_ADAPTER=codex` only disambiguates between adapters that already match — some argv token must still be a `codex` binary (e.g. `GMUX_ADAPTER=codex gmux -- env codex …`). It cannot force codex handling for an arbitrarily-named wrapper, and hook injection also needs a literal `codex` token in argv.

### Live status via hooks

When gmux owns the launch (and codex ≥ 0.135.0), it appends per-launch `-c hooks.…` config overrides so codex runs `gmux __codex-hook` on lifecycle events. Nothing is written to `~/.codex` — the injection is ephemeral. gmux pre-trusts *only its own* hooks by computing codex's per-hook trusted hash; your own hook trust model is untouched, and on any mismatch the hook is silently skipped (the session just lacks live state). Set `GMUX_NO_AGENT_HOOK=1` to disable injection.

| Codex hook event | Effect |
|---|---|
| `SessionStart` | Binds the session (transcript path, title, slug) |
| `UserPromptSubmit` | Working (cyan dot) |
| `Stop` | Idle + unread; title refreshed |

The hook also reports an explicit URL slug derived from your first prompt (codex's UUID session ids would make unreadable URLs), and refreshes the title at each turn end.

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

The codex adapter walks the full date tree to discover conversation files. If a transcript is deleted, the session stops being offered for resume.

### Session file format

Each line is a JSON object with a `type` field:

| Line type | Purpose |
|---|---|
| `session_meta` | First line — session ID, cwd, timestamp, CLI version |
| `response_item` | Message content — user prompts, assistant responses, function calls |
| `event_msg` | Codex's own lifecycle log — `user_message`, `agent_message`, `task_complete`, … (not consumed by gmux) |
| `turn_context` | Per-turn metadata (model, instructions) |

## Limitations

- **Requires codex ≥ 0.135.0 for live status.** Below that, no hooks are injected and the session runs without working/idle status (there is no file-watch fallback).
- **No custom titles.** Codex doesn't generate session titles like Claude Code does. gmux uses the first user prompt as the title.
- **Date-based storage.** Sessions aren't grouped by project. gmux scans all conversation files and matches them to working directories by reading the `session_meta` header.
- **Status is coarse.** gmux doesn't distinguish between "thinking", "running a command", or "writing code" — all are shown as "working". Codex's `Stop` hook carries no exit reason, so an interrupted turn is reported as completed (idle + unread) rather than aborted.

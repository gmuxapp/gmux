---
title: OpenCode Adapter
description: Adapter for the OpenCode AI coding agent.
---

> This feature is not yet implemented. Depends on [folder management](/planned/folder-management/).

[OpenCode](https://opencode.ai) is an open-source AI coding agent that runs in the terminal. Unlike Claude Code, pi, and Codex (which write JSONL session files to central directories), OpenCode stores sessions in a SQLite database at `.opencode/opencode.db` relative to the working directory. This per-project storage model requires the folder management refactor before the adapter can discover sessions.

## Phases

### Phase 1: Basic adapter

Implement the core `Adapter` and `Launchable` interfaces.

- **Name**: `opencode`
- **Binary**: `opencode`
- **Match**: scan command args for `opencode` (same pattern as claude/codex)
- **Discover**: `exec.LookPath("opencode")`
- **Monitor**: no-op. OpenCode uses a full-screen bubbletea TUI with animated spinners, making PTY byte parsing unreliable for status detection.
- **Launcher**: `{ id: "opencode", label: "OpenCode", command: ["opencode"], description: "Coding Agent" }`

This is enough for sessions to appear in gmux and be launchable from the UI. No working/idle status, no session discovery, no resume.

### Phase 2: Session discovery via SQLite

Requires the folder management refactor. With scoped scanning, the scanner checks each configured folder for `.opencode/opencode.db` and queries it for sessions.

OpenCode's SQLite schema stores sessions and messages in two tables:

```sql
-- sessions table
id TEXT PRIMARY KEY,
title TEXT NOT NULL,
message_count INTEGER NOT NULL DEFAULT 0,
updated_at INTEGER NOT NULL,  -- Unix ms
created_at INTEGER NOT NULL   -- Unix ms

-- messages table
id TEXT PRIMARY KEY,
session_id TEXT NOT NULL,
role TEXT NOT NULL,
parts TEXT NOT NULL default '[]',  -- JSON array
finished_at INTEGER
```

The adapter would implement `SessionFiler` by:
- Querying `SELECT id, title, message_count, created_at FROM sessions ORDER BY updated_at DESC`
- Mapping results to `SessionFileInfo` structs

This requires a SQLite dependency. `modernc.org/sqlite` (pure Go, no CGo) is preferred to avoid requiring a C compiler in the build chain.

### Phase 3: Status monitoring

Detect working/idle state by monitoring the SQLite database for changes.

The approach: watch the `.opencode/opencode.db-wal` file via inotify. On change, query the messages table for the latest message in the active session. If the most recent message has `role = "user"` and no subsequent assistant message with `finished_at IS NOT NULL`, the session is working. Otherwise idle.

An alternative is to propose that OpenCode write a lightweight status file (e.g. `.opencode/status.json`) upstream, which would let gmux use the existing `FileMonitor` infrastructure.

### Phase 4: Session resume

Blocked on upstream. OpenCode has no `--session <id>` CLI flag; session resume is TUI-only via an in-app session picker dialog. Once upstream adds CLI resume support, the adapter can implement `Resumer`.

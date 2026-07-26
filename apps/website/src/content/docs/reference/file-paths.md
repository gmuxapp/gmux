---
title: File paths
description: All file paths used by gmux and gmuxd.
sidebar:
  order: 2
---

## Config files

Created by the user. gmux does not write to these, except that `gmux remote` can add `[tailscale]` to `host.toml` with your confirmation.

| Path | Purpose | Reference |
|------|---------|-----------|
| `~/.config/gmux/host.toml` | Daemon behavior (port, Tailscale) | [host.toml](/reference/host-toml/) |
| `~/.config/gmux/settings.jsonc` | Terminal options, keybinds, UI prefs | [settings.jsonc](/reference/settings/) |
| `~/.config/gmux/theme.jsonc` | Terminal color palette | [theme.jsonc](/reference/theme/) |

`~/.config` can be overridden with `XDG_CONFIG_HOME`.

## Runtime state

Created by gmuxd. Lives under `~/.local/state/gmux` (or `$XDG_STATE_HOME/gmux`).

| Path | Purpose |
|------|---------|
| `~/.local/state/gmux/gmuxd.sock` | Daemon Unix socket (local IPC between gmux CLI and gmuxd) |
| `~/.local/state/gmux/auth-token` | Bearer token for TCP authentication |
| `~/.local/state/gmux/state.db`, `state.db-wal`, `state.db-shm` | Central SQLite database (ADR 0026) — sessions, projects, peers, and their tokens. Owner-only (0600); backups contain peer tokens and must be treated as secrets. Use `gmux daemon state backup <path>` for consistent online backups |
| `~/.local/state/gmux/node-id` | Stable, opaque per-node identity (ADR 0007), generated on first start. Not a secret, but back it up with `auth-token` — if it changes, peers see a different host |
| `~/.local/state/gmux/sessions/<session-id>/scrollback`, `scrollback.0` | Terminal scrollback (written by the runner). For dead sessions this is a cache: an aggregate budget (`GMUX_SCROLLBACK_CACHE_MB`, default 256 MB) evicts oldest-first while the session row in `state.db` survives |
| `~/.local/state/gmux/shell-sessions/` | Shell adapter per-session state files |
| `~/.local/state/gmux/gmuxd.log` | Daemon log (when started via `gmux daemon start`) |
| `~/.local/state/gmux/tsnet/` | Tailscale state directory (when remote access is enabled) |

## Session sockets

Created by `gmux` (the CLI) for each running session. gmuxd connects to these to stream terminal I/O.

| Path | Purpose |
|------|---------|
| `~/.local/state/gmux/run/sessions/<session-id>.sock` | Per-session Unix socket |

Override the directory with `GMUX_SOCKET_DIR`. The default deliberately lives under the state dir rather than `$XDG_RUNTIME_DIR`: runtime dirs are torn down by logind when your last login session ends, which would unlink the sockets of still-running sessions.

During the 2.0 transition, gmuxd also scans the legacy locations (`$XDG_RUNTIME_DIR/gmux/sessions` and the per-uid temp dir) so runners started under older versions stay discoverable; this shim is dropped in v2.1 and is disabled when `GMUX_SOCKET_DIR` is set.

## Adapter-specific paths

| Path | Purpose | Used by |
|------|---------|---------|
| `~/.pi/agent/sessions/` | Pi conversation files (JSONL). Root overridable with `PI_CODING_AGENT_DIR` | Pi adapter (conversation discovery and resume) |
| `~/.claude/projects/<encoded-cwd>/` | Claude Code conversation files (JSONL) | Claude adapter |
| `~/.codex/sessions/YYYY/MM/DD/` | Codex conversation files (JSONL) | Codex adapter |

gmux only reads these directories — agent hooks are injected per launch, never written into `~/.claude` or `~/.codex`.

## Temporary files

| Path | Purpose |
|------|---------|
| `$TMPDIR/paste-<n>.<ext>` | Image/binary pastes from the web UI (created 0600) |

## Logs

| Path | Purpose |
|------|---------|
| `~/.local/state/gmux/gmuxd.log` | Daemon log when started via `gmux daemon start` (truncated on each start) |
| `$TMPDIR/gmuxd.log` (usually `/tmp/gmuxd.log`) | Daemon stderr when gmuxd is auto-started by a `gmux` command |

`gmux daemon log-path` prints the state-dir log path.

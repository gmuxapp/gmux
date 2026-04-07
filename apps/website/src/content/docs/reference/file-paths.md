---
title: File paths
description: All file paths used by gmux and gmuxd.
sidebar:
  order: 2
---

## Config files

Created by the user. gmux does not write to these, except that `gmuxd remote` can add `[tailscale]` to `host.toml` with your confirmation.

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
| `~/.local/state/gmux/projects.json` | User-curated project list (sidebar grouping, ordering) |
| `~/.local/state/gmux/gmuxd.log` | Daemon log (when started via `gmuxd start`) |
| `~/.local/state/gmux/tailscale-discovery.json` | Cache of probed tailnet devices (auto-discovery) |
| `~/.local/state/gmux/tsnet/` | Tailscale state directory (when remote access is enabled) |

## Session sockets

Created by `gmux` (the CLI) for each running session. gmuxd connects to these to stream terminal I/O.

| Path | Purpose |
|------|---------|
| `/tmp/gmux-sessions/<session-id>.sock` | Per-session Unix socket |

Override the directory with `GMUX_SOCKET_DIR`.

## Adapter-specific paths

| Path | Purpose | Used by |
|------|---------|---------|
| `~/.pi/agent/sessions/` | Pi conversation files (JSONL) | Pi adapter (session discovery and resume) |

## Logs

| Path | Purpose |
|------|---------|
| `~/.local/state/gmux/gmuxd.log` | Daemon log when started via `gmuxd start` or auto-started by `gmux` |

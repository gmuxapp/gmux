---
title: Configuration
description: Overview of gmux configuration files, CLI commands, and environment variables.
---

gmux works out of the box with no configuration. Everything is customizable through three config files in `~/.config/gmux/`:

| File | Purpose | Reference |
|------|---------|-----------|
| `host.toml` | Daemon behavior: port, Tailscale remote access, devcontainer discovery | [host.toml →](/reference/host-toml/) |
| `settings.jsonc` | Terminal options, keybinds, UI preferences | [settings.jsonc →](/reference/settings/) |
| `theme.jsonc` | Terminal color palette (Windows Terminal theme compatible) | [theme.jsonc →](/reference/theme/) |

All files are optional. Create or edit them manually. The only exception is `gmux remote`, which can add `[tailscale]` to `host.toml` with your confirmation.

Settings and theme changes take effect on browser refresh (no daemon restart needed). Host config changes require restarting gmuxd.

## Runtime state

Not everything lives in config files. gmux keeps daemon-owned state in `~/.local/state/gmux/`:

- **Peers** are added at runtime via *Settings → Hosts → Connect to host* and stored in `peers.json` — they are **not** configured in `host.toml` (the pre-2.0 `[[peers]]` key is ignored with a warning).
- **Projects** live in `projects.json`, managed via *Settings → Projects*.
- The **host auth token** lives in `auth-token` (seedable with `GMUXD_TOKEN`); `gmux auth` prints it along with a login/pairing URL.

See [Multi-machine](/multi-machine/) and [File paths](/reference/file-paths/) for details.

## More reference

- [File paths](/reference/file-paths/) — config files, sockets, runtime state, logs
- [CLI commands](/reference/cli/) — `gmux` and `gmuxd` usage
- [Environment variables](/reference/environment/) — variables that affect gmux and variables set inside sessions
- [projects.json](/reference/projects-json/) — project list state

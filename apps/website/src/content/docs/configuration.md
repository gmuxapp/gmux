---
title: Configuration
description: Overview of gmux configuration files, CLI commands, and environment variables.
---

gmux works out of the box with no configuration. Everything is customizable through three config files in `~/.config/gmux/`:

| File | Purpose | Reference |
|------|---------|-----------|
| `host.toml` | Daemon behavior: port, Tailscale remote access | [host.toml →](/reference/host-toml/) |
| `settings.jsonc` | Terminal options, keybinds, UI preferences | [settings.jsonc →](/reference/settings/) |
| `theme.jsonc` | Terminal color palette (Windows Terminal theme compatible) | [theme.jsonc →](/reference/theme/) |

All files are optional. Create them manually; gmux never writes to them.

Settings and theme changes take effect on browser refresh (no daemon restart needed). Host config changes require restarting gmuxd.

## More reference

- [File paths](/reference/file-paths/) — config files, sockets, runtime state, logs
- [CLI commands](/reference/cli/) — `gmux` and `gmuxd` usage
- [Environment variables](/reference/environment/) — variables that affect gmux and variables set inside sessions

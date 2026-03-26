---
title: Configuration
description: Config file, environment variables, and file paths.
---

gmux works out of the box with no configuration. This page documents everything you can customize.

## Config file

`~/.config/gmux/config.toml` (or `$XDG_CONFIG_HOME/gmux/config.toml`)

gmuxd reads this file at startup. Create it manually — gmuxd never writes to it. If the file doesn't exist, safe defaults are used.

```toml
# Listen port for the localhost HTTP server.
# Default: 8790
port = 8790

# Optional tailscale remote access.
# See the Remote Access guide for setup.
[tailscale]
enabled = false
hostname = "gmux"       # → gmux.your-tailnet.ts.net
allow = []               # additional login names (owner is auto-whitelisted)

# Optional network listener for containers and VPN setups.
# See develop/network-listener for setup and security implications.
# [network]
# listen = "10.0.0.5"
```

### Strict validation

The config file is strictly validated at startup. gmuxd refuses to start if:

- **Unknown keys** are present — catches typos like `alow` instead of `allow`
- **`allow` entries don't contain `@`** — likely not a valid tailscale login name
- **`hostname` is empty** when tailscale is enabled
- **`port` is out of range** (must be 1–65535)
- **TOML syntax is invalid**

This is intentional — silent fallback to defaults is dangerous for security settings. See [Security](/security) for the reasoning.

## Environment variables

### gmuxd (daemon)

| Variable | Purpose | Default |
|----------|---------|---------|
| `GMUXD_PORT` | Listen port (overrides config file) | `8790` |
| `GMUXD_LISTEN` | Bind a [network listener](/develop/network-listener) to this address | *(disabled)* |
| `XDG_CONFIG_HOME` | Base directory for config file | `~/.config` |
| `XDG_STATE_HOME` | Base directory for runtime state | `~/.local/state` |

### gmux (session runner)

| Variable | Purpose | Default |
|----------|---------|---------|
| `GMUXD_PORT` | Port to reach gmuxd on localhost (shared with gmuxd) | `8790` |
| `GMUX_ADAPTER` | Force a specific adapter instead of auto-detection | *(auto)* |
| `GMUX_SOCKET_DIR` | Directory for session Unix sockets | `/tmp/gmux-sessions` |

### Set by gmux in the child process

These are available inside every session launched by gmux. Use them to detect gmux or report status back:

| Variable | Purpose | Example |
|----------|---------|---------|
| `GMUX_SOCKET` | Unix socket path for callbacks to the runner | `/tmp/gmux-sessions/sess-abc123.sock` |
| `GMUX_SESSION_ID` | Unique session identifier | `sess-abc123` |
| `GMUX_ADAPTER` | Name of the matched adapter | `pi`, `shell` |
| `GMUX_VERSION` | Protocol version | `0.1.0` |

See [Adapter Architecture](/develop/adapter-architecture) for how to use the child-to-runner API.

## File paths

| Path | Purpose | Created by |
|------|---------|------------|
| `~/.config/gmux/config.toml` | Config file | User |
| `~/.local/state/gmux/tsnet/` | Tailscale state (when enabled) | gmuxd |
| `~/.local/state/gmux/auth-token` | Network listener bearer token | gmuxd |
| `/tmp/gmux-sessions/*.sock` | Live session Unix sockets | gmux |

### Adapter-specific paths

| Path | Purpose | Used by |
|------|---------|---------|
| `~/.pi/agent/sessions/` | Pi conversation files (JSONL) | gmuxd (for session discovery and resume) |

## Port

The default port is **8790**. To change it, set it in the config file or via environment variable:

```toml
port = 9999
```

Or:

```bash
GMUXD_PORT=9999 gmuxd start
```

For daemon commands, run `gmuxd -h`.

The env var takes precedence over the config file. The localhost listener always binds to `127.0.0.1`. For binding to other addresses, see [Network Listener](/develop/network-listener).

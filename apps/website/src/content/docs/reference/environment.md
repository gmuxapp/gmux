---
title: Environment variables
description: Environment variables used and set by gmux.
tableOfContents:
  maxHeadingLevel: 3
---

## gmuxd

Variables that affect the daemon.

| Variable | Purpose | Default |
|----------|---------|---------|
| `GMUXD_LISTEN` | TCP bind address (IPv4 or IPv6). | `127.0.0.1` |
| `XDG_CONFIG_HOME` | Base directory for config files. | `~/.config` |
| `XDG_STATE_HOME` | Base directory for runtime state (socket, auth token). | `~/.local/state` |
| `GMUXD_DEV_PROXY` | Proxy frontend requests to a Vite dev server (development only). | *(none)* |

### Bind address

By default gmuxd binds to `127.0.0.1` (localhost only). All TCP connections require bearer token authentication.

To bind to all interfaces (containers, VPN setups):

```bash
GMUXD_LISTEN=0.0.0.0 gmuxd start
```

The bind address is controlled exclusively by the `GMUXD_LISTEN` environment variable. It is not a config file option because it is a deployment concern, not a user preference.

## gmux (CLI)

Variables that affect the session runner.

| Variable | Purpose | Default |
|----------|---------|---------|
| `GMUX_ADAPTER` | Force a specific adapter instead of auto-detection. | *(auto)* |
| `GMUX_SOCKET_DIR` | Directory for per-session Unix sockets. | `/tmp/gmux-sessions` |

## Set by gmux in child processes

These are available inside every session launched by `gmux`. Use them to detect that you are running inside gmux, or to communicate back to the session runner.

| Variable | Purpose | Example |
|----------|---------|---------|
| `GMUX` | Always `1` inside a gmux session. Used for nested-session detection. | `1` |
| `GMUX_SOCKET` | Unix socket path for callbacks to the session runner. | `/tmp/gmux-sessions/sess-abc123.sock` |
| `GMUX_SESSION_ID` | Unique session identifier. | `sess-abc123` |
| `GMUX_ADAPTER` | Name of the matched adapter. | `pi`, `shell` |
| `GMUX_VERSION` | gmux protocol version. | `0.4.0` |

See [Adapter Architecture](/develop/adapter-architecture) for how to use the child-to-runner API.

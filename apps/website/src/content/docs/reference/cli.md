---
title: CLI
description: Command reference for gmux and gmuxd.
tableOfContents:
  maxHeadingLevel: 3
sidebar:
  order: 1
---

## gmux

The session runner and primary entry point. Auto-starts gmuxd if needed.

### `gmux`

Open the gmux UI in a browser. Starts gmuxd if it is not already running. Prefers Chrome/Chromium in `--app` mode for a standalone window; falls back to the default browser.

### `gmux <command> [args...]`

Run a command inside a gmux session. The session registers with gmuxd and appears in the web UI.

```bash
gmux bash
gmux python3 main.py
gmux pi "build the feature"
```

When run inside an existing gmux session (detected via the `GMUX` environment variable), `gmux` automatically detaches into a headless background process instead of nesting PTY-within-PTY. The new session appears in the UI.

## gmuxd

The daemon. Manages sessions, serves the web UI, and optionally provides Tailscale remote access.

### `gmuxd start`

Start the daemon in the background, replacing any existing instance. Logs to `~/.local/state/gmux/gmuxd.log`. Prints the PID on success.

Reads [`host.toml`](/reference/host-toml/) for configuration. Binds to `127.0.0.1` on the configured port (default 8790) and creates a Unix socket for local IPC.

### `gmuxd run`

Run the daemon in the foreground. Same as `start`, but blocks until interrupted. Use this for systemd services, Docker containers, or debugging.

### `gmuxd restart`

Alias for `start`. Replaces any running instance with a fresh one.

### `gmuxd stop`

Stop the running daemon via the Unix socket.

### `gmuxd status`

Show daemon health, session counts, and peer status.

```
gmuxd 0.4.0 (ready)
  tcp:    127.0.0.1:8790
  socket: /home/user/.local/state/gmux/gmuxd.sock
  remote: https://gmux.tailnet.ts.net

Sessions: 3 alive (2 local, 1 remote), 12 dead (15 total)

Peers:
  • desktop (1 session)
    https://gmux-1.tailnet.ts.net
  ✗ server (connection refused)
    https://server.tailnet.ts.net
```

### `gmuxd auth`

Show the TCP listen address, auth token, and a ready-to-open login URL. Useful for connecting from another device on the local network.

```
Listen:     127.0.0.1:8790
Auth token: abc123...

Open this URL to authenticate:
  http://127.0.0.1:8790/auth/login?token=abc123...
```

### `gmuxd remote`

Set up or check Tailscale remote access. If Tailscale is not yet configured, walks you through enabling it. If already enabled, shows the connection status and diagnostics.

See [Remote Access](/remote-access/) for the full guide.

### `gmuxd version`

Print the version string.

### `gmuxd help`

Show the usage summary.

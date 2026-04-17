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

`gmux` has no subcommands. Flags before the command apply to gmux itself; once the first positional argument is seen, everything after it is the command to run, verbatim, including its own flags. Use `--` to disambiguate when the command starts with a dash.

### `gmux`

Open the gmux UI in a browser. Starts gmuxd if it is not already running. Prefers Chrome/Chromium in `--app` mode for a standalone window; falls back to the default browser.

### `gmux [--no-attach] <command> [args...]`

Run a command inside a gmux session. The session registers with gmuxd and appears in the web UI.

```bash
gmux bash
gmux python3 main.py
gmux pi "build the feature"
gmux --no-attach pytest --watch       # detach from the terminal
gmux -- --my-dash-cmd                 # `--` preserves a dashy command
```

With `--no-attach` the session is spawned in the background and appears in the UI, but `gmux` returns immediately instead of wiring your local terminal to it. Without it, `gmux` attaches transparently — Ctrl-C goes to the child, resize events follow your terminal, and closing the terminal detaches without killing the session.

When run inside an existing gmux session (detected via the `GMUX` environment variable), `gmux` automatically detaches into a headless background process instead of nesting PTY-within-PTY. The new session appears in the UI.

### `gmux --list` (`-l`)

List known sessions, alive first, newest first.

```
$ gmux --list
ID        STATUS  KIND   TITLE
a3f20187  alive   pi     fix auth bug  (/home/mg/dev/myapp)
be14b052  alive   shell  bash          (/home/mg/dev/gmux)
7d3304e9  dead    shell  make build    (/home/mg/dev/myapp)
```

The ID column shows the 8-character short form the web UI uses; most management flags accept unique ID prefixes, the full session ID, or the session's slug.

### `gmux --attach <id>` (`-a`)

Reattach your local terminal to an existing session. The session's scrollback is replayed on connect, SIGWINCH is forwarded, and closing the terminal detaches without killing the session — identical to how `gmux <cmd>` itself behaves.

```bash
gmux --attach a3f20187
gmux -a fix-auth-bug        # slug also works
```

Attach requires an interactive terminal. Remote peer sessions are supported transparently — gmuxd proxies the WebSocket to the owning node.

### `gmux --tail <N> <id>` (`-t`)

Print the last `N` lines of a session's output as plain text (scrollback plus the currently visible screen, ANSI stripped). Useful for peeking at a background session without attaching to it.

```bash
gmux --tail 100 a3f20187
gmux -t 20 fix-auth-bug
```

`--tail` currently only works for sessions owned by the local node; remote peer sessions are rejected with a clear error.

### `gmux --kill <id>` (`-k`)

Terminate a running session. Sends the same signal chain the UI's kill button does: `SIGTERM` to the child, normal exit lifecycle, session marked dead.

```bash
gmux --kill a3f20187
```

### `gmux --send <id> [text]`

Inject input into a running session, as if the bytes had been typed at the terminal. When `text` is given inline it is sent verbatim — no trailing newline is added, so you use shell `$'...\n'` (or pipe stdin) to submit:

```bash
gmux --send a3f20187 $'describe yourself\n'
echo 'describe yourself' | gmux --send a3f20187   # equivalent
printf '\x03' | gmux --send a3f20187              # send Ctrl-C
```

When `text` is omitted, gmux reads from stdin until EOF and sends whatever it sees (capped at 1 MiB). Stdin mode is the natural shape for piping, and avoids shell-escaping headaches for multi-line input.

**Access control.** `--send` is powerful — anything you send lands in the session's PTY, and the child has no way to distinguish it from keyboard input. Access is gated by filesystem permissions on the session's Unix socket (owner-only, `0700`), which means only the user that started the session can send to it. Other users on the same machine cannot connect to the socket at all. For the same reason, `--send` is local-only: cross-machine sending would need an explicit authorization model that doesn't exist yet.

## gmuxd

The daemon. Manages sessions, serves the web UI, and optionally provides Tailscale remote access.

### `gmuxd start`

Start the daemon in the background. If an existing instance is running, it is stopped first (the old version is printed for confirmation). Logs to `~/.local/state/gmux/gmuxd.log`. Prints the PID on success.

```
$ gmuxd start
gmuxd: stopping existing daemon (1.0.0)...
gmuxd: running (pid 12345)
  Logs: /home/user/.local/state/gmux/gmuxd.log
```

Reads [`host.toml`](/reference/host-toml/) for configuration. Binds to `127.0.0.1` on the configured port (default 8790) and creates a Unix socket for local IPC.

### `gmuxd run`

Run the daemon in the foreground. Same as `start`, but blocks until interrupted. Use this for systemd services, Docker containers, or debugging.

### `gmuxd restart`

Alias for `start`. Stops any running instance and starts a fresh one.

### `gmuxd stop`

Stop the running daemon via the Unix socket.

### `gmuxd status`

Show daemon health, session counts, and peer status.

```
gmuxd 1.0.0 (ready)
  tcp:    127.0.0.1:8790
  socket: /home/user/.local/state/gmux/gmuxd.sock
  remote: https://gmux.tailnet.ts.net

Sessions: 3 alive (2 local, 1 remote), 12 dead (15 total)

Peers:
  • desktop (1 session)
    https://gmux-desktop.tailnet.ts.net
  ○ gmux-server (offline)
    https://gmux-server.tailnet.ts.net
  ✗ manual-peer (connection refused)
    https://peer.example.com
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

Set up or check Tailscale remote access.

If Tailscale is not yet configured, explains what remote access is, asks for confirmation, enables it in `host.toml`, restarts the daemon, and waits for Tailscale to connect. The command guides you through the entire process interactively.

If already enabled, polls the daemon until Tailscale reaches a known state, then shows the connection status. It only reports HTTPS/MagicDNS problems after confirming the connection is established, so you never see false warnings from a daemon that is still starting.

See [Remote Access](/remote-access/) for the full guide.

### `gmuxd log-path`

Print the daemon log file path with no extra output, suitable for scripting.

```bash
tail -f $(gmuxd log-path)
cat $(gmuxd log-path) | grep tsauth
```

### `gmuxd version`

Print the version string.

### `gmuxd help`

Show the usage summary.

---
title: Troubleshooting
description: Common problems and how to fix them.
---

## Dashboard doesn't open / "gmuxd is not running"

`gmux open` (and any session launch) auto-starts `gmuxd` if it isn't running. If the dashboard doesn't appear at [localhost:8790](http://localhost:8790), `gmuxd` may have failed to start.

**Check the log:**

```bash
cat $(gmux daemon log-path)
```

If the daemon was auto-started by `gmux open`, startup errors may instead be in `$TMPDIR/gmuxd.log`.

Common causes:

- **Port already in use** — something else is on port 8790. Change it in `~/.config/gmux/host.toml` (`port = 9999`).
- **Config file error** — gmuxd refuses to start with unknown keys or invalid values. The log will say which key. (Keys removed in 2.0 only produce a warning.) See [host.toml reference](/reference/host-toml/#strict-validation).
- **`gmuxd` not in PATH** — `gmux` looks for `gmuxd` as a sibling binary first, then in `PATH`. Make sure both are installed together (e.g. via `brew install gmuxapp/tap/gmux`).

**Start manually to see errors immediately:**

```bash
gmuxd run
```

This runs the daemon in the foreground so you can see errors directly. Use `gmux daemon start` for normal background operation.

## Sessions don't appear in the sidebar

- **No project configured.** gmux discovers sessions but doesn't add them to the sidebar automatically. Open **Settings → Projects** (gear button) and add the project from the *Discovered* list, or click **Add a project** in the empty state.
- **Session exited immediately.** If the command exits before gmuxd discovers it, it won't appear. Check if the command works when run directly (outside of `gmux`).
- **Different daemon.** If you have multiple gmux installs (e.g. Homebrew and a dev build), `gmux` and `gmuxd` might not be talking to the same instance. Run `gmux daemon status` to see the running daemon's version and socket path and compare against `gmux version`.

## "outdated" badge on a session

After updating gmux, sessions that were started with the old version show an **outdated** tag. The session still works, but the runner binary doesn't match the daemon. Restart the session (session **⋮** menu → Restart) to pick up the new version.

## Session fails to launch: working directory missing

If a session's directory was deleted (e.g. a removed worktree), launch fails with a working-directory error. Resuming an existing session whose directory is gone falls back to the project's canonical folder instead.

## Actions fail with 403 behind a reverse proxy

gmuxd rejects cross-origin cookie-authenticated mutations and WebSocket upgrades (see [Security](/security/#browser-sessions-same-origin-enforcement)). A proxy that rewrites the `Host` header must forward the browser-facing host in `X-Forwarded-Host`; programmatic clients should use bearer-token auth instead of the cookie.

## WebSocket disconnects / terminal goes blank

- **gmuxd restarted.** The browser reconnects automatically when gmuxd comes back. If the terminal stays blank, refresh the page.
- **Network interruption** (remote access). The SSE event stream reconnects within a few seconds. If the terminal doesn't recover, the session's runner process may have exited while disconnected.
- **Laptop sleep/resume.** The browser re-establishes connections on wake. Give it a moment; if sessions are missing, gmuxd may have been stopped by the OS.

## Ctrl+V pastes `^V` instead of clipboard

This happens when the keybind system isn't intercepting the key. Possible causes:

- **Focus is not on the terminal.** Click inside the terminal area first.
- **Browser extension conflict.** Some extensions (Vimium, custom shortcut managers) intercept keys before gmux sees them. Try disabling extensions.
- **Using an iframe embed.** Clipboard API requires a [Permissions-Policy](https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Permissions-Policy) header when embedded in an iframe.

## Copy doesn't work (Ctrl+Shift+C or Cmd+C)

The clipboard API requires a secure context: either `localhost`, `127.0.0.1`, or HTTPS. If you're accessing gmux over plain HTTP on a LAN IP, the browser blocks clipboard access. Use [Remote Access](/remote-access) (Tailscale provides HTTPS) or run via `localhost`.

## Remote access issues

See the [Remote Access troubleshooting section](/remote-access/#troubleshooting) for Tailscale-specific issues (device not appearing, certificate warnings, hostname resolution).

## Updating

It's safe to update gmux while sessions are running; they reconnect automatically. gmux checks for new releases in the background and notifies you in the dashboard and when you run `gmux open` or `gmux daemon status`. Open dashboard tabs reload themselves after the daemon updates.

After updating, the old daemon is replaced automatically:

- **Homebrew**: the postflight hook restarts the daemon during install
- **`curl | sh` installer**: restarts the daemon if it was running
- **Manual installs**: the next `gmux open` (or session launch) detects the version mismatch and replaces the daemon

To force a restart manually: `gmux daemon restart` (or just `gmux daemon start`, which replaces any running instance).

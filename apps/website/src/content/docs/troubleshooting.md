---
title: Troubleshooting
description: Common problems and how to fix them.
---

## Dashboard doesn't open / "gmuxd is not running"

`gmux` auto-starts `gmuxd` on first run. If the dashboard doesn't appear at [localhost:8790](http://localhost:8790), `gmuxd` may have failed to start.

**Check the log:**

```bash
cat $(gmuxd log-path)
```

Common causes:

- **Port already in use** — something else is on port 8790. Change it in `~/.config/gmux/host.toml` (`port = 9999`).
- **Config file error** — gmuxd refuses to start with unknown keys or invalid values. The log will say which key. See [host.toml reference](/reference/host-toml/#strict-validation).
- **`gmuxd` not in PATH** — `gmux` looks for `gmuxd` as a sibling binary first, then in `PATH`. Make sure both are installed together (e.g. via `brew install gmuxapp/tap/gmux`).

**Start manually to see errors immediately:**

```bash
gmuxd run
```

This runs the daemon in the foreground so you can see errors directly. Use `gmuxd start` for normal background operation.

## Sessions don't appear in the sidebar

- **No project configured.** gmux discovers session groups but doesn't add them to the sidebar automatically. Click **Add project** in the empty state, or the **Manage projects** button. Unmatched sessions show a badge count on the manage button.
- **Session exited immediately.** If the command exits before gmuxd discovers it, it won't appear. Check if the command works when run directly (outside of `gmux`).
- **Different daemon.** If you have multiple gmux installs (e.g. Homebrew and a dev build), `gmux` and `gmuxd` might not be talking to the same instance. Run `gmuxd status` to check which binary is running.

## "outdated" badge on a session

After updating gmux, sessions that were started with the old version show an **outdated** tag. The session still works, but the runner binary doesn't match the daemon. Kill and relaunch the session to pick up the new version.

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

It's safe to update gmux while sessions are running; they reconnect automatically. gmux checks for new releases in the background and notifies you in the dashboard sidebar and when you run `gmux` with no arguments.

After updating, the old daemon is replaced automatically:

- **Homebrew**: the postflight hook restarts the daemon during install
- **`curl | sh` installer**: restarts the daemon if it was running
- **Manual installs**: the next `gmux` invocation detects the version mismatch and replaces the daemon

To force a restart manually: `gmuxd restart` (or just `gmuxd start`, which replaces any running instance).

---
title: Troubleshooting
description: Common problems and how to fix them.
---

## Dashboard doesn't open / "gmuxd is not running"

`gmux` auto-starts `gmuxd` on first run. If the dashboard doesn't appear at [localhost:8790](http://localhost:8790), `gmuxd` may have failed to start.

**Check the log:**

```bash
cat /tmp/gmuxd.log
```

Common causes:

- **Port already in use** — something else is on port 8790. Change it in `~/.config/gmux/host.toml` (`port = 9999`).
- **Config file error** — gmuxd refuses to start with unknown keys or invalid values. The log will say which key. See [Configuration](/configuration).
- **`gmuxd` not in PATH** — `gmux` looks for `gmuxd` as a sibling binary first, then in `PATH`. Make sure both are installed together (e.g. via `brew install gmuxapp/tap/gmux`).

**Start manually to see errors immediately:**

```bash
gmuxd start
```

## Updating

It's safe to update gmux while sessions are running — they reconnect automatically. gmux checks for new releases in the background and notifies you in the dashboard sidebar and when you run `gmux` with no arguments.

After updating, the old daemon is replaced automatically:

- **Homebrew**: the postflight hook restarts the daemon during install
- **`curl | sh` installer**: restarts the daemon if it was running
- **Manual installs**: the next `gmux` invocation detects the version mismatch and replaces the daemon

To force a restart manually: `gmuxd stop && gmuxd start`, or just `gmuxd start` (it replaces any running instance).

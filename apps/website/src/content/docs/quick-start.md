---
title: Quick Start
description: Install gmux and launch your first session in under a minute.
---

## Install

```bash
brew install gmuxapp/tap/gmux
```

Or download both binaries (`gmuxd` + `gmux`) from [GitHub Releases](https://github.com/gmuxapp/gmux/releases).

## Run

```bash
gmux pi                    # launch a coding agent
gmux pytest --watch     # or any command
gmux make build
```

Open **[localhost:8790](http://localhost:8790)**. Sessions appear in the sidebar grouped by project directory. Click one to attach a live terminal.

`gmux` auto-starts the daemon (`gmuxd`) on first run — there's nothing else to set up. For daemon commands, run `gmuxd -h`.

## Next steps

- [Using the UI](/using-the-ui) — what the dots mean, keyboard shortcuts, mobile, launching from the browser
- [Remote Access](/remote-access) — access gmux from your phone or another machine
- [Configuration](/configuration) — config file, environment variables, file paths
- [Architecture](/architecture) — how the pieces fit together
- [Troubleshooting](/troubleshooting) — dashboard not opening, sessions not appearing, version mismatches

---
title: Session Persistence
description: Survive reboots — resume all sessions from where you left off.
---

> This feature is not yet implemented.

When your computer reboots (or gmuxd restarts), all sessions are lost. Session persistence would let you pick up exactly where you left off.

## The idea

After a reboot, opening gmux shows your previous sessions in a "suspended" state. A **Resume All** button on each folder restarts every session in that folder. Sessions that have their own state management (like `pi`) resume seamlessly. Plain shell sessions restart with the previous scrollback visible above a separator line.

## How it would work

### On shutdown (best-effort)

When gmuxr exits (clean shutdown or SIGTERM), it writes the tail of its scrollback ring buffer to a file alongside the session metadata. This is best-effort — a hard power-off may lose it.

### On resume

1. gmuxd discovers persisted session files on startup (it already does this for `pi` sessions).
2. The user clicks **Resume** on a session or **Resume All** on a folder.
3. gmuxd launches a new gmuxr with the same command and cwd.
4. The new gmuxr seeds its scrollback with the saved content, followed by a separator:

```
─── session resumed ───
```

5. The child process starts writing after the separator. Tools like `pi` that clear the screen will overwrite the preamble naturally. Plain commands (shell, scripts) will have their old output visible above.

### What's needed

- **gmuxr**: Write raw scrollback to `~/.local/state/gmux/scrollback/<session-id>.raw` on exit.
- **gmuxd**: Discover persisted scrollback files. Include them in session metadata so the UI knows scrollback is available.
- **gmuxr**: Accept a `--seed-scrollback <path>` flag that pre-fills the ring buffer before the child starts.
- **UI**: "Resume All" button on folder headers. Visual treatment for suspended sessions.

## Prerequisite

This builds on the work to keep gmuxr alive after child exit — once sessions survive process exit within a single boot, surviving across boots is the natural next step.

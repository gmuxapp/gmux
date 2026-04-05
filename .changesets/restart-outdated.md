---
bump: minor
---

- **Click "outdated" to restart a session.** The `outdated` badge in the session header is now clickable: it kills the stale runner, waits for the exit lifecycle, and relaunches with the resume command and the same session ID. The backend handles the whole cycle atomically via `POST /v1/sessions/:id/restart`.
- **Selection stays sticky during restart and reconnect.** Sessions only get deselected when they're actually removed from the store (dismissed or purged). Dead-but-present sessions keep the header visible and show a minimal "Connecting…" placeholder, so restart cycles and brief connection gaps don't drop you back to the launch buttons.
- **`POST /kill` now uses SIGHUP.** Interactive shells ignored the old SIGTERM and stayed alive. SIGHUP matches the "terminal closed" semantics and works for both shells and TUI adapters.

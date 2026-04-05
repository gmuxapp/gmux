---
bump: patch
---

- **Fix: `Dismiss` / `Kill` / `Restart` now work on fish shell sessions.**
  Fish ignores `SIGHUP` on interactive shells, so the previous "close the
  X" button was a no-op: the runner survived, the discovery scan
  re-registered the session, and it reappeared in the sidebar. The runner
  now waits up to 2 seconds for the child to exit after `SIGHUP` and
  escalates to `SIGKILL` if it doesn't. `POST /kill` also blocks until the
  child is reaped, so gmuxd knows the session is really gone before
  removing it from its store.

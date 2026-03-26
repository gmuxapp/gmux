---
bump: patch
---

- **Fixed dev instances unable to connect to their backend.** The `gmux` CLI
  now reads `GMUXD_PORT` directly (same env var as `gmuxd`), so a single
  export is sufficient for both binaries. The redundant `GMUXD_ADDR` env var
  has been removed.

- **Dev instances now isolate pi session storage.** The pi adapter respects
  `PI_CODING_AGENT_DIR`, and the dev scripts export it to an instance-specific
  directory. Sessions launched from a dev instance no longer appear in (or
  pollute) your real pi session history.

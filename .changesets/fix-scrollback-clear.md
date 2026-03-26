---
bump: patch
---

- **Clean scrollback on screen clear.** `ESC[2J` and `ESC[3J` now reset the
  scrollback buffer, discarding stale pre-clear content. Previously, reconnecting
  clients would see 5-6 ghost lines left over from `watch(1)` redraws or pi's
  thinking-phase clears.
- **Wrapped ring buffer trimming.** When the 128KB scrollback buffer wraps around,
  snapshots now start at the first complete line boundary instead of mid-line or
  mid-escape-sequence, preventing rendering artifacts on reconnect.

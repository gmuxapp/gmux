---
bump: patch
---

- **Fix duplicated lines and missing content on scrollback replay.** TUI cursor-positioning sequences (used by pi-tui and bubbletea for frame rendering) were discarded by the bare CR handler, causing new frames to stack below old ones instead of overwriting. Bare CR now flushes content to the ring buffer, preserving cursor movement for correct replay. Additionally, when the ring buffer wraps, the snapshot now trims to the last frame-start marker (BSU for pi-tui differential rendering, or cursor-home for bubbletea-style TUIs) to replay only the latest frame, preventing stale frames from appearing as duplicate content.

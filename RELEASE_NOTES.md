- Fixed 1px horizontal scroll when the terminal is auto-fitted to the viewport ([#32](https://github.com/gmuxapp/gmux/pull/32))
- **Clean scrollback on screen clear.** `ESC[2J` and `ESC[3J` now reset the
  scrollback buffer, discarding stale pre-clear content. Previously, reconnecting
  clients would see 5-6 ghost lines left over from `watch(1)` redraws or pi's
  thinking-phase clears.
- **Wrapped ring buffer trimming.** When the 128KB scrollback buffer wraps around,
  snapshots now start at the first complete line boundary instead of mid-line or
  mid-escape-sequence, preventing rendering artifacts on reconnect. ([#36](https://github.com/gmuxapp/gmux/pull/36))
- **Session attributions survive gmuxd restart.** File-to-session attributions
  are now persisted to `attributions.json` inside the /tmp/gmux-sessions directory. ([#33](https://github.com/gmuxapp/gmux/pull/33))

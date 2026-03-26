- **Clean scrollback on screen clear, better ring buffer wrapping.** When the 128KB scrollback buffer wraps around,
  snapshots now start at the first complete line boundary instead of mid-line or
  mid-escape-sequence. ([#36](https://github.com/gmuxapp/gmux/pull/36))
- **Session attributions survive gmuxd restart.** File-to-session attributions
  are now persisted to `attributions.json` inside the /tmp/gmux-sessions directory. ([#33](https://github.com/gmuxapp/gmux/pull/33))
- Fixed 1px horizontal scroll when the terminal is auto-fitted to the viewport ([#32](https://github.com/gmuxapp/gmux/pull/32))

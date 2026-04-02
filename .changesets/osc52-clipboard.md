---
bump: patch
---

- **OSC 52 clipboard support.** Applications running in the terminal can now
  write to the browser clipboard using the standard OSC 52 escape sequence.
  This makes pi's `/copy` command, tmux's clipboard integration, and vim's
  `+y` yank work through the gmux web UI.

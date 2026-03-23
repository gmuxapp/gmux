---
bump: patch
---

- **Fixed scroll position drifting when scrollback is full.** When the user
  was scrolled up and new output arrived in synchronized update blocks,
  the viewport would gradually drift as old lines were evicted from the
  scrollback buffer. The scroll position now correctly tracks content
  through scrollback eviction.

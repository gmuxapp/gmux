---
bump: patch
---

- **Clickable links now work on mobile.** Tapping a URL in terminal output
  previously failed to open it because the touch handler scrolled the viewport
  before the browser could synthesize the mouse events that xterm.js uses for
  link activation. The scroll is now deferred so links resolve at the correct
  position.

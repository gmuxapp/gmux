---
bump: patch
---

- **Fixed session replay losing conversation content.** TUI spinner animations
  (which overwrite a single row repeatedly) filled the 128KB scrollback ring
  buffer in under 60 seconds, pushing out the actual conversation. Session
  replay now uses a virtual terminal emulator (midterm) that maintains the true
  screen state. Spinner frames update one cell; the rest of the screen is
  untouched. Reconnecting clients see the complete screen with all content,
  colors, and cursor state preserved.

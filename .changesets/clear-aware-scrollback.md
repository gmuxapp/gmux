---
bump: patch
---

- **Smarter scrollback storage.** Loading spinners and screen clears no longer
  waste scrollback buffer space. Spinner frames that overwrite each other via
  carriage return are collapsed to just the final frame, and screen clears
  (`ESC[2J` / `ESC[3J`) discard pre-clear content from the buffer.

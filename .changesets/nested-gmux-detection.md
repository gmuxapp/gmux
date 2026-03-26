---
bump: minor
---

- **Nested gmux detection.** Running `gmux <command>` inside an existing gmux
  session no longer creates a PTY-within-PTY nest. Instead, the session is
  launched in the background and appears in the gmux UI automatically.

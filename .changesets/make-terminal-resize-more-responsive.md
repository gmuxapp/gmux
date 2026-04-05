---
bump: patch
---

- **More responsive browser-driven terminal resize.** Window resizing now sends the next PTY resize as soon as the previous one is confirmed by the server, instead of waiting on a coarse trailing debounce. Soft-keyboard height changes still use a short touch-only debounce so mobile typing stays stable.

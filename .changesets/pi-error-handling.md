---
bump: patch
---

- **Smarter error handling for pi sessions.** Transient API errors (overloaded, rate-limited) no longer flicker the sidebar status. gmux stays in the working state while pi retries, and only shows a red error dot when all retries are exhausted. The dot clears when you view the session or send a new message.

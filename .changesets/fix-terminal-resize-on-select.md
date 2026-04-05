---
bump: patch
---

- **Terminals now resize to fit your window when you select a session.**
  Previously, selecting a session would show "Sized for another device, click
  to resize" even on the only connected browser, because the client started
  in a passive mode and a separate focus-triggered takeover never fired on
  session switches. The web client now claims ownership on every session
  select: the first WebSocket connect for a session resizes the PTY to fit
  the current viewport. Auto-reconnects after a network blip stay passive so
  they don't steal ownership from another driver, and the resize pill still
  appears when another client or a local terminal changes the PTY size.

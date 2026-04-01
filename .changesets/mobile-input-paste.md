---
bump: patch
---

- **Mobile input: Return key inserts newline, send button submits.** On touch
  devices, the on-screen keyboard's Return key now inserts a newline (`\n`)
  instead of submitting (`\r`). A new send button (▶) in the mobile toolbar
  handles submission. Desktop behavior is unchanged.

- **Paste preserves newlines.** In non-bracketed paste mode, newlines are now
  kept as `\n` instead of being converted to `\r`. This lets raw-mode
  applications (coding agents, editors) distinguish pasted newlines from Enter.
  Bracketed paste mode is unchanged.

- **Mobile toolbar uses touch detection.** The mobile toolbar and sidebar
  overlay now appear based on touch capability (`pointer: coarse`) instead of
  screen width. Tablets in landscape get touch controls; desktop users with
  narrow windows keep the sidebar.

- **Ctrl+Shift on mobile.** When the Ctrl soft button is armed and an uppercase
  letter is typed (Shift held on the soft keyboard), the terminal now sends the
  correct CSI u sequence for Ctrl+Shift+letter instead of collapsing to
  Ctrl+letter.

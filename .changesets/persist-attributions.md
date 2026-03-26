---
bump: patch
---

- **Session attributions survive gmuxd restart.** File-to-session attributions
  are now persisted to `attributions.json` inside the gmux-sessions directory.
  On restart, titles and resume keys are restored immediately instead of
  requiring a new file write to trigger re-attribution.

---
bump: minor
---

### Status indicators redesign

- **Unread indicator is now blue.** The sidebar dot for sessions with unread
  content changed from amber to blue, making it more visible against dark
  backgrounds.
- **Working indicator is now a hollow ring.** Sessions where an agent is
  actively processing show a pulsating ring outline instead of a filled dot,
  reducing visual noise while remaining recognizable.
- **Transient activity indicator for terminals.** Shell sessions that produce
  output briefly show a muted ring that fades after a few seconds, rather than
  permanently marking as unread. Agent sessions (pi, Claude, Codex) only
  trigger unread when the assistant completes a turn.
- **Arrival animation on all unread transitions.** The grow-pulse animation
  now fires whenever a session becomes unread (previously only on
  working-to-unread transitions). The mobile hamburger badge re-animates
  when additional sessions become unread.

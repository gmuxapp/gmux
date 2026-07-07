---
title: Notifications
description: Daemon-driven session notifications with presence tracking and smart routing.
---

> **Status (2.0): implemented** as described (presence tracking, grace period, coalescing, best-client routing). A follow-up redesign toward stateful, resolve-once interactive notifications is proposed in ADR 0018.

## How it works

The daemon owns all notification decisions. Browser clients are dumb reporters
that show what the daemon tells them to.

**Browser → Daemon** (via `/v1/presence` WebSocket):
- `client-hello`: device type, notification permission
- `client-state`: visibility, focus, selected session, last interaction timestamp
- `notif-permission`: updated permission after user grant/deny

**Daemon → Browser**:
- `notify`: show an OS notification (title, body, session ID, tag for dedup)
- `cancel`: dismiss a notification (e.g. user opened the session on another device)

### Trigger conditions

| Event | Condition |
|---|---|
| **Session finished** | `status.working` true → false on a live session |
| **New output** | `unread` false → true |

Both are skipped if any client is focused on gmux or viewing the session.

### Escalation model

1. **In-app dot** (always) — yellow/blue indicator on sidebar and hamburger button
2. **Tab title badge** — `(1) gmux` when sessions have unread output
3. **OS notification** — after a 5-second grace period; cancelled if user focuses gmux within that window
4. **Cross-device routing** — if the active device is idle (>2 min since last interaction), route to the most recently used other device

### Coalescing

If 3+ sessions trigger notifications within the same grace period window,
the daemon sends a single summary ("5 sessions finished") instead of
individual notifications.

## Implementation

- `internal/presence` — presence table tracking connected clients
- `internal/notify` — notification router with grace period, coalescing, device routing
- `apps/gmux-web/src/presence.ts` — presence WebSocket client with auto-reconnect
- Permission UI: "Enable notifications" button in sidebar footer

## Open items

- Background push when no browser tab is open (see [Mobile Notifications](/planned/mobile-notifications))
- Notification sounds (optional)
- Per-session notification preferences (mute noisy sessions)

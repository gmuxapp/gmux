Peering internals were refactored into shared `sseclient` and `apiclient`
packages, fixing reconnect failures on large terminal snapshots and
sessions disappearing across reconnects.

### Session file attribution
The daemon now attributes session files by adapter kind with scrollback
matching, fixing cases where sessions lost their title or showed stale
adapter badges.

### Sidebar and UI
Sessions can be reordered via drag-and-drop. Peer labels use
hash-based color assignment for stability, and the adapter/outdated
badges are replaced by a session info menu. The home and project pages
have distinct layouts with better mobile ergonomics.

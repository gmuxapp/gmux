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

<!-- highlights-end -->

### Features
- distinct project page layout and mobile ergonomics ([#125](https://github.com/gmuxapp/gmux/pull/125))
- **(peering)** extract sseclient package for shared SSE decoding ([#128](https://github.com/gmuxapp/gmux/pull/128))
- **(peering)** extract apiclient package for typed spoke HTTP access ([#128](https://github.com/gmuxapp/gmux/pull/128))
- **(store)** add UpsertRemote for sessions already resolved by a peer ([#128](https://github.com/gmuxapp/gmux/pull/128))
- **(sseclient)** detect idle streams with configurable read deadline ([#129](https://github.com/gmuxapp/gmux/pull/129))
- add conversations index for file-backed conversation lookup ([#130](https://github.com/gmuxapp/gmux/pull/130))
- wire conversations index into gmuxd with lookup API ([#130](https://github.com/gmuxapp/gmux/pull/130))
- replace stale bool with runner_version and binary_hash ([#135](https://github.com/gmuxapp/gmux/pull/135))
- **(web)** move version display from sidebar to home page ([#136](https://github.com/gmuxapp/gmux/pull/136))
- **(web)** replace adapter and outdated badges with session info menu ([#136](https://github.com/gmuxapp/gmux/pull/136))
- **(web)** peer labels with unique prefix and palette colors ([#141](https://github.com/gmuxapp/gmux/pull/141))
- **(web)** drag-to-reorder sessions in the sidebar ([#143](https://github.com/gmuxapp/gmux/pull/143))

### Fixes
- dismissed sessions no longer reappear after scanner cycle ([#115](https://github.com/gmuxapp/gmux/pull/115))
- **(peering)** reconnect loop on remote sessions with large terminal snapshots ([#128](https://github.com/gmuxapp/gmux/pull/128))
- **(peering)** preserve session titles forwarded from spokes ([#128](https://github.com/gmuxapp/gmux/pull/128))
- **(peering)** keep remote sessions visible across reconnects ([#129](https://github.com/gmuxapp/gmux/pull/129))
- **(web)** handle Android autocorrect in mobile input handler ([#133](https://github.com/gmuxapp/gmux/pull/133))
- **(web)** guard mobile-input handler with pointer:coarse check ([#138](https://github.com/gmuxapp/gmux/pull/138))
- **(web)** polish sidebar logo, spinner visibility, and peer status display ([#137](https://github.com/gmuxapp/gmux/pull/137))
- **(web)** surface update notification and clean up dead sidebar import ([#137](https://github.com/gmuxapp/gmux/pull/137))
- **(web)** fix session menu clipping, improve button style, rename to session-menu ([#139](https://github.com/gmuxapp/gmux/pull/139))
- **(web)** redesign session menu, fix remote staleness, delete dead CSS ([#140](https://github.com/gmuxapp/gmux/pull/140))
- **(web)** push home page footer to bottom of viewport ([#141](https://github.com/gmuxapp/gmux/pull/141))
- **(web)** use ResizeObserver for terminal fit on initial layout ([#141](https://github.com/gmuxapp/gmux/pull/141))
- **(cli)** rename GMUX_VERSION child env var to GMUX_RUNNER_VERSION ([#142](https://github.com/gmuxapp/gmux/pull/142))
- **(daemon)** exempt manifest.json from netauth ([#145](https://github.com/gmuxapp/gmux/pull/145))
- suppress false activity signals during session switching ([#144](https://github.com/gmuxapp/gmux/pull/144))
- skip unread flag from full-file re-reads on session restart ([#144](https://github.com/gmuxapp/gmux/pull/144))
- **(daemon)** attribute session files regardless of directory ([#147](https://github.com/gmuxapp/gmux/pull/147))
- **(web)** use hash-based peer color assignment for stability ([#149](https://github.com/gmuxapp/gmux/pull/149))
- **(web)** avoid false stale indicator when peer version is unknown ([#149](https://github.com/gmuxapp/gmux/pull/149))
- **(daemon)** attribute session files by adapter kind with throttled scrollback matching ([#148](https://github.com/gmuxapp/gmux/pull/148))
- **(daemon)** prune orphaned candidates and skip redundant file parsing ([#148](https://github.com/gmuxapp/gmux/pull/148))
- **(daemon)** reset adapter title when active session file changes ([#148](https://github.com/gmuxapp/gmux/pull/148))
- **(ci)** resolve PR numbers for rebase-merged commits in changelog ([#151](https://github.com/gmuxapp/gmux/pull/151))

### Docs
- cover shared peering client packages and planned reliability work ([#128](https://github.com/gmuxapp/gmux/pull/128))
- add release highlights for v1.2.0 ([#152](https://github.com/gmuxapp/gmux/pull/152))

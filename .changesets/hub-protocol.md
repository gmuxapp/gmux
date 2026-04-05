---
bump: minor
---

### Hub protocol (peer aggregation)

- **Aggregate sessions from remote gmuxd instances.** Configure `[[peers]]`
  in `host.toml` with a name, URL, and auth token. The hub subscribes to
  each spoke's SSE event stream, namespaces remote session IDs as
  `originalID@peerName`, and merges them into the local session store.
  Remote sessions appear in the sidebar grouped by project (matched by
  git remote URL on the hub side).

- **Interact with remote sessions.** Kill, resume, dismiss, and read
  actions are forwarded to the owning spoke. Terminal connections are
  proxied through the hub, so attaching to a remote session works the
  same as a local one.

- **URL routing for remote sessions.** Remote sessions use
  `/@peerName/` segments in URLs (e.g. `/gmux/@server/pi/fix-auth`).
  Local and remote sessions with the same slug are disambiguated by the
  peer segment.

- **Peer status indicators.** The sidebar footer shows connection state
  for each configured peer (connected, connecting, disconnected).

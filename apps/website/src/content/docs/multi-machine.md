---
title: Multi-Machine Sessions
description: See sessions from every machine, container, and VM in one dashboard.
---

gmux uses a hub-and-spoke model to aggregate sessions across machines. You pick one gmuxd as your dashboard (the hub). It connects outward to other gmuxd instances (the spokes) and merges their sessions into a single UI. The browser only talks to the hub.

```
browser --> gmux-laptop (hub)
               |-- local sessions
               |-- <-- gmux-desktop (spoke, via Tailscale)
               |       \-- desktop sessions
               \-- <-- devcontainer (spoke, Docker bridge)
                       \-- container sessions
```

Spokes need zero configuration changes. The hub authenticates with each spoke's bearer token and subscribes to its event stream. Actions like kill, resume, and launch are forwarded transparently.

## Tailscale auto-discovery

When [Tailscale](/remote-access) is enabled, gmuxd automatically discovers other gmux instances on the same tailnet. No manual peer configuration is needed: install gmux on two machines, enable Tailscale on both, and they find each other.

gmuxd subscribes to tailnet changes via Tailscale's `WatchIPNBus` API and reacts immediately when devices come online. Each new device is probed with a `/v1/health` request to confirm it's running gmux, then added as a peer. Results are cached so known devices are re-registered instantly on restart without re-probing.

Authentication is handled by Tailscale identity. The tailscale listener uses `WhoIs` to verify the connecting peer belongs to the same user. No bearer tokens are exchanged.

To disable auto-discovery while keeping remote access:

```toml
[discovery]
tailscale = false
```

## Devcontainer auto-discovery

Add one line to your `devcontainer.json` and sessions from inside the container appear in your dashboard automatically:

```json
"features": {
  "ghcr.io/gmuxapp/features/gmux": {}
}
```

The host gmuxd detects the container via Docker events, reads the auth token, and connects over the Docker bridge. See the [Devcontainers](/devcontainers) guide for setup, options, and details.

## Connecting to a host manually

For machines that aren't auto-discovered (different tailnet, reachable by URL/IP), open **Settings → Hosts → Connect to host** and enter the host's URL, e.g. `https://gmux-server.your-tailnet.ts.net`. Leave the token blank when the host is on your own tailnet (it authenticates by identity); otherwise paste its auth token.

gmux probes the host, adopts the name it reports about itself (no name to assign), and saves the connection to `peers.json` in the state directory. If a different host already uses that name, gmux suffixes it (`server-2`) for you. The hub connects immediately and reconnects with exponential backoff on failure; manual and auto-discovered peers use the same protocol.

There is no `[[peers]]` config block (removed in [ADR 0007](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0007-host-identity-and-peer-urls.md)) — peers are runtime state managed from the UI.

## When a referenced host is renamed

A host's name is derived from Tailscale (or its OS hostname), so it can change — for example when you upgrade a machine. A project reference pins another host's project into your sidebar by name, so a rename could silently strip those projects out.

To prevent that, a reference is anchored on the host's stable, opaque node ID, not just its name. References you create from the UI capture that ID immediately, so a later rename is followed automatically and the projects stay put under the new name.

If gmux can't match a reference to any current host — the host was renamed before it was anchored, or removed entirely — the reference is **not** silently dropped:

- The sidebar shows the project muted, with a warning marker.
- The settings gear gets a small red pip.
- **Settings → Hosts → Referenced but not found** lists each unmatched host with the projects pointing at it. Pick the host's current name from **Remap to…** to repoint every reference at once (this re-anchors them on the node ID, so the same rename won't break them again), or remove the references if the host is gone for good.

## Session namespacing

Remote sessions carry their origin in the session ID using `@` separators:

```
sess-abc123               # local
sess-abc123@desktop       # from spoke "desktop"
sess-abc123@dev@server    # from spoke "dev", which is a spoke of "server"
```

The UI parses these to build the topology breadcrumbs on the project hub. Routing uses the chain to forward actions hop-by-hop: the hub only knows its direct spokes, each spoke only knows its own.

## Fault tolerance

Each spoke connection is independent. A slow or dead spoke never blocks the hub or other spokes.

When a spoke goes offline:

- Its sessions remain visible but marked as disconnected.
- The host status indicator turns red on the project hub.
- The hub reconnects with exponential backoff (1s initial, 30s max, reset on success).
- When the spoke comes back, sessions go live again. No user action needed.

### Connection health detection

The SSE event stream uses two mechanisms to detect dead connections:

**Heartbeat.** The spoke emits a `:\n\n` SSE comment every 30 seconds when no real events are flowing. Comment lines are part of the SSE spec and produce no client-side events. They keep the connection alive through idle periods and give the hub something to read.

**Idle timeout.** The hub's SSE client sets a sliding 60-second read deadline that resets on every line received (events, comments, anything). If no data arrives within the window, the connection is declared dead and the peer reconnects. Since the heartbeat interval (30s) is well within the timeout (60s), the timeout only fires on actual connection failures, not legitimately idle spokes.

This pair covers the common failure modes: NAT rebinds, tunnel hiccups, and network changes that leave the TCP socket open but no bytes flowing. The same heartbeat also keeps the browser's native `EventSource` connection to the hub alive during quiet periods.

## Protocol details

A peer gmuxd is a regular client of the spoke's public API. There are no peer-only endpoints and no peer-only auth scheme; the hub uses the spoke's bearer token exactly the same way a browser uses it on the TCP listener. If the browser path works, the peer path works.

The hub connects to these public gmuxd endpoints on each spoke:

| Endpoint | Purpose |
|----------|---------|
| `GET /v1/events` | SSE subscription for session updates |
| `GET /v1/sessions` | Initial session list on connect |
| `GET /v1/health` | Version, hostname, available launchers |
| `POST /v1/launch` | Forward launch requests |
| `WS /ws/:id` | Proxy terminal WebSocket connections |
| `POST /v1/sessions/:id/kill` | Forward kill |
| `POST /v1/sessions/:id/dismiss` | Forward dismiss |
| `POST /v1/sessions/:id/resume` | Forward resume |

All spoke traffic flows through the same small Go packages (`sseclient` and `apiclient`) that also back internal uses of the public API. See [Architecture: Shared client packages](/architecture#shared-client-packages). That means read limits, auth, and error handling have a single implementation: terminal snapshots up to 4 MiB flow through the WebSocket proxy without tripping size limits, and spoke-resolved session titles are never re-derived on the hub (the hub trusts the spoke's `title` instead of computing one from empty internal fields).

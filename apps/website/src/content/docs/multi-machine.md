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

## Manual peers

For machines that aren't on the same tailnet, configure peers explicitly in `~/.config/gmux/host.toml`:

```toml
[[peers]]
name = "server"
url = "https://gmux-server.your-tailnet.ts.net"
token = "the-spoke-auth-token"
```

The hub connects on startup and reconnects with exponential backoff on failure. Manual and auto-discovered peers use the same protocol.

## Session namespacing

Remote sessions carry their origin in the session ID using `@` separators:

```
sess-abc123               # local
sess-abc123@desktop       # from spoke "desktop"
sess-abc123@dev@server    # from spoke "dev", which is a spoke of "server"
```

The UI parses these to build the topology breadcrumbs on the project hub. Routing uses the chain to forward actions hop-by-hop: the hub only knows its direct spokes, each spoke only knows its own.

## Fault tolerance

Each spoke connection is independent. When a spoke goes offline:

- Its sessions remain visible but marked as disconnected.
- The host status indicator turns red on the project hub.
- The hub reconnects with exponential backoff.
- When the spoke comes back, sessions go live again.

A slow or dead spoke never blocks the hub or other spokes. Planned reliability work (connection keepalives, silent-drop detection, and a unified session-identity model) is tracked in [Planned: peer reliability](/planned/peer-reliability).

## Protocol details

A peer gmuxd is a regular client of the spoke's public API. There are no peer-only endpoints and no peer-only auth scheme; the hub uses the spoke's bearer token exactly the same way a browser uses it on the TCP listener. If the browser path works, the peer path works.

The hub connects to these public gmuxd endpoints on each spoke:

| Endpoint | Purpose |
|----------|---------|
| `GET /v1/events` | SSE subscription for session updates |
| `GET /v1/sessions` | Initial session list on connect |
| `GET /v1/config` | Launcher configuration (available adapters) |
| `POST /v1/launch` | Forward launch requests |
| `WS /ws/:id` | Proxy terminal WebSocket connections |
| `POST /v1/sessions/:id/kill` | Forward kill |
| `POST /v1/sessions/:id/dismiss` | Forward dismiss |
| `POST /v1/sessions/:id/resume` | Forward resume |

All spoke traffic flows through the same small Go packages (`sseclient` and `apiclient`) that also back internal uses of the public API. See [Architecture: Shared client packages](/architecture#shared-client-packages). That means read limits, auth, and error handling have a single implementation: terminal snapshots up to 4 MiB flow through the WebSocket proxy without tripping size limits, and spoke-resolved session titles are never re-derived on the hub (the hub trusts the spoke's `title` instead of computing one from empty internal fields).

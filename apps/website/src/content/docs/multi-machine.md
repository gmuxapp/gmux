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

gmuxd periodically queries the tailnet for online devices, probes each with a `/v1/health` request to identify gmux instances, and subscribes to their event streams. Results are cached so known devices are never re-probed.

Authentication is handled by Tailscale identity. The tailscale listener uses `WhoIs` to verify the connecting peer belongs to the same user. No bearer tokens are exchanged.

To disable auto-discovery while keeping remote access:

```toml
[discovery]
tailscale = false
```

## Devcontainer auto-discovery

If a project directory contains `.devcontainer/devcontainer.json`, gmuxd discovers the running container automatically using Docker labels. No manual peer configuration is needed.

The [gmux devcontainer Feature](https://github.com/gmuxapp/features) installs gmux into any devcontainer:

```json
{
  "image": "mcr.microsoft.com/devcontainers/base:debian",
  "features": {
    "ghcr.io/gmuxapp/features/gmux": {}
  }
}
```

The feature sets `GMUXD_LISTEN=0.0.0.0` so gmuxd accepts connections on the Docker bridge, and generates a bearer token on first start. The host gmuxd reads the token from the container and connects as a peer.

Once connected, the container's sessions appear in the sidebar and on the [project hub](/using-the-ui#project-hub) alongside local sessions, with a host breadcrumb showing the topology (e.g. `workstation > alpine-dev`). Launching from the hub's per-folder **+** button routes the session to the correct machine.

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

A slow or dead spoke never blocks the hub or other spokes.

## Protocol details

The hub connects to standard gmuxd endpoints on each spoke:

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

No new protocol or auth scheme: the hub authenticates with the spoke's bearer token, the same mechanism browsers use for the TCP listener.

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

## Adding a tailnet machine

gmux does **not** auto-connect machines on your tailnet. Being on the same tailnet lets a machine *reach* another, but connecting still requires that host's bearer token, exactly like any other peer ([ADR 0008](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0008-peer-authentication-via-token.md)). This keeps a single compromised node (say, a container running an untrusted agent) from driving every other machine on the tailnet.

All connected hosts must run gmux 2.0. A 2.0 hub cannot aggregate a 1.x spoke and vice versa; upgrade every machine (and rebuild devcontainers) together.

To add a tailnet machine, run `gmux auth` on it and copy the **connect URL** it prints (it also prints a QR code you can scan from a device on your tailnet). If the machine doesn't have gmux remote access enabled yet, run `gmux remote` first — without it, `gmux auth` prints only the local URL:

```
To add this host from another gmux machine, paste this into "Connect to host":
  https://gmux-server.your-tailnet.ts.net/auth/login?token=…
```

Then paste it into **Settings → Hosts → Connect to host** (see below). The token rides in the URL, so it's one paste.

## Devcontainer auto-discovery

Add one line to your `devcontainer.json` and sessions from inside the container appear in your dashboard automatically:

```json
"features": {
  "ghcr.io/gmuxapp/features/gmux": {}
}
```

The host gmuxd detects the container via Docker events, reads the auth token, and connects over the Docker bridge. See the [Devcontainers](/devcontainers) guide for setup, options, and details.

## Connecting to a host manually

Open **Settings → Hosts → Connect to host**. Paste the connect URL from `gmux auth` (it splits into the URL and token fields automatically), or enter the host's URL (e.g. `https://gmux-server.your-tailnet.ts.net`) and its token separately. A token is required for every host, tailnet or not.

gmux probes the host, adopts the name it reports about itself (no name to assign), and saves the connection to the daemon’s SQLite database (`state.db`). If a different host already uses that name, gmux suffixes it (`server-2`) for you. The hub connects immediately and reconnects with exponential backoff on failure; every peer uses the same token-authenticated protocol.

There is no `[[peers]]` config block (removed in [ADR 0007](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0007-host-identity-and-peer-urls.md)) — peers are runtime state managed from the UI.

## When a referenced host is renamed or removed

A host's name is adopted at first contact and then frozen — renaming the machine later doesn't change what your roster calls it, so references keep working under the original label until you remove and re-add the host. Each reference also records the host's stable, opaque node ID as a liveness anchor: it lets a re-added host reclaim its old references automatically, and stops a *different* machine that reuses the name from silently adopting them.

If gmux can't match a reference to any current host — it's not in the roster because it was removed, or set up on a previous install — the reference is **not** silently dropped:

- The sidebar shows the project muted, with a warning marker.
- The settings gear gets a small red pip.
- **Settings → Hosts → Referenced but not found** lists each unmatched host with the projects pointing at it. Re-add the host under **Connect to host** (the reference re-anchors on the re-added host's node ID) and the references resolve again; otherwise remove them.

Removing a host clears the references that pointed at it, so a deliberate removal leaves nothing behind here.

## Upgrading from a version with tailnet autodiscovery

Earlier versions auto-discovered gmux machines on your tailnet. [ADR 0008](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0008-peer-authentication-via-token.md) removed that, so on first start the daemon migrates the hosts **you had projects on** into the roster as **Auth needed** (it imports the old discovery cache, then deletes it). Click **Add token** on each in **Settings → Hosts** and paste its token (`gmux auth` on that host) to bring it online. Machines you never pinned a project on aren't carried over — add them with **Connect to host** if you want them. 2.0 uses a clean SQLite database; there is no migration from 1.x JSON files.

## Session namespacing

Remote sessions carry their origin in the session ID using `@` separators:

```
sess-abc123               # local
sess-abc123@desktop       # from spoke "desktop"
sess-abc123@dev@server    # from "dev", a devcontainer attached to spoke "server"
```

The UI parses these to attribute each session to its host (the `@host` suffix and devcontainer marker). Actions are routed by splitting on the last `@` and forwarding one hop; only devcontainer (local-peer) sessions forward through their parent host — sessions of one network peer are never relayed through another.

## Fault tolerance

Each spoke connection is independent. A slow or dead spoke never blocks the hub or other spokes.

When a spoke goes offline:

- Its sessions remain visible but marked as disconnected.
- The host shows as **Offline** in Settings → Hosts (with the connection error as detail); a host whose token is missing or wrong shows **Auth needed** instead.
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
| `GET /v1/events?as=peer` | SSE snapshot stream — full `snapshot.sessions` on connect and on every change |
| `GET /v1/projects` | Spoke's projects + discovered list (refreshed on `projects-update` events) |
| `GET /v1/health` | Version, hostname, node ID, available launchers |
| `POST /v1/launch` | Forward launch requests |
| `WS /ws/:id` | Proxy terminal WebSocket connections |
| `POST /v1/sessions/:id/kill` | Forward kill |
| `POST /v1/sessions/:id/dismiss` | Forward dismiss |
| `POST /v1/sessions/:id/resume` | Forward resume |

All spoke traffic flows through the same small Go packages (`sseclient` and `apiclient`) that also back internal uses of the public API. See [Architecture: Shared client packages](/architecture#shared-client-packages). That means read limits, auth, and error handling have a single implementation: terminal snapshots up to 4 MiB flow through the WebSocket proxy without tripping size limits, and spoke-resolved session titles are never re-derived on the hub (the hub trusts the spoke's `title` instead of computing one from empty internal fields).

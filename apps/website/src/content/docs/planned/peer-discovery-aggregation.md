---
title: Peer Discovery & Aggregation
description: See sessions from every machine, container, and VM in one dashboard.
---

> This feature is not yet implemented.

Today each gmuxd instance is an island. You open `gmux-desktop.tailnet.ts.net` to see your desktop sessions and `gmux-server.tailnet.ts.net` to see your server sessions. If you run coding agents on three machines, you juggle three tabs.

Cross-instance lets any gmuxd show sessions from other gmuxd instances alongside its own. One URL, every machine.

## The model

The architecture is **hub-and-spoke**. You pick one gmuxd as your "home" dashboard (the hub). It connects outward to other gmuxd instances (the spokes) using the same HTTP/SSE/WS protocol that the browser already uses. The browser only ever talks to the hub. The hub merges remote sessions into its own store and proxies terminal connections through to the owning peer.

Peers do not need to reach each other. If your laptop can connect to both a desktop and a server, that's sufficient; the desktop and server don't need any connectivity between them. This matters because Tailscale ACLs, network topology, or mixed networks (tailnet + Docker bridge) can easily prevent peer-to-peer connectivity even when the hub can reach all spokes.

A peer that is also a hub for its own spokes (e.g. a host aggregating devcontainers) presents a unified session list upstream. The top-level hub doesn't need to know about the inner topology; it treats the intermediate peer as a single source.

```
browser ──→ gmux-laptop (hub)
               ├── local sessions
               ├── ← gmux-desktop (spoke, via tailscale)
               │      └── desktop sessions
               └── ← gmux-server (spoke, via tailscale)
                      ├── server sessions
                      └── ← container: project-a (spoke of server)
                             └── container sessions
```

## Discovery

### Tailscale auto-discovery

gmuxd instances already register as tailscale devices. Tailscale's local API (`/api/v0/status`) lists all nodes on the tailnet. gmuxd can query this periodically and look for peers:

1. List all online nodes on the tailnet.
2. Filter to nodes tagged with `tag:gmux` (via tailscale ACL tags) or matching a configurable hostname pattern.
3. For each candidate, probe `https://<hostname>/v1/sessions` to confirm it's a gmuxd.
4. Subscribe to its `/v1/events` stream.

This gives you zero-config discovery on a tailnet: install gmux on two machines, they find each other.

### Manual peers

For cases where auto-discovery doesn't apply (containers on a Docker network, VMs on a private subnet, instances behind firewalls), peers can be configured explicitly:

```toml
[[peers]]
name = "server"
url = "https://gmux-server.tailnet.ts.net"

[[peers]]
name = "project-a"
url = "http://project-a:8790"
```

Manual peers are tried on startup and reconnected on failure. Auto-discovered and manual peers use the same protocol.

### Container discovery

Containers are a special case of manual peers. A host gmuxd could auto-discover containers by scanning Docker for containers with a `gmux.peer=true` label and connecting to their internal IP on port 8790. This is container-specific glue on top of the general peer protocol.

See the [Devcontainers](#devcontainers) section for the full picture.

## Data model

### Namespaced sessions

Session namespacing is **preserved**: each layer passes through the namespace chain from its own spokes. This gives the hub full visibility into the topology for routing and display.

```
local session:       sess-abc123
remote session:      desktop/sess-def456
multi-layer session: server/project-a/sess-ghi789
```

When the hub receives sessions from a spoke, it prefixes them with the spoke's name. If that spoke is itself a hub aggregating its own spokes, the inner namespaces are preserved. The result is a path-like namespace: `server/project-a/sess-ghi789` means "session `sess-ghi789` on spoke `project-a`, which is a spoke of `server`."

The store distinguishes local sessions (discovered via Unix sockets as today) from remote sessions (received via peer SSE). Remote sessions are read-through: the hub caches them for rendering but delegates actions (kill, resume, launch) to the owning peer.

### Canonical project URI

Sessions gain a `project_uri` field: a normalized identifier derived from the VCS remote URL.

```
git@github.com:gmuxapp/gmux.git  →  github.com/gmuxapp/gmux
https://github.com/gmuxapp/gmux  →  github.com/gmuxapp/gmux
```

The runner detects this at session startup (it already walks up from cwd to find `workspace_root`; reading `git remote get-url origin` or `jj git remote list` is one more step). The field is included in the `/meta` response alongside `workspace_root`.

The UI uses `project_uri` to group sessions across peers. Two sessions on different machines with the same `project_uri` appear under one project heading:

```
gmuxapp/gmux
  Fix auth bug          laptop     pi
  Run tests             server     shell
  Refactor adapter      server     pi (container: project-a)
```

The local filesystem path (`cwd`, `workspace_root`) is still shown in session details. The canonical URI is for grouping only.

When `project_uri` is empty (no VCS remote, or a local-only repo), sessions fall back to grouping by `workspace_root` or `cwd` as they do today, scoped to their peer.

### Peer metadata

Each peer advertises metadata alongside its sessions:

```json
{
  "name": "gmux-server",
  "version": "0.9.0",
  "os": "linux",
  "hostname": "unraid"
}
```

The UI uses this for display (icons, labels) and compatibility checks. A peer running an incompatible protocol version is flagged but not rejected.

## Protocol

### Subscribing

The hub connects to `GET /v1/events` on each spoke, the same SSE endpoint the browser uses. Session upsert/remove events are prefixed with the spoke name and merged into the hub's store.

Authentication uses the same mechanism as browser connections: tailscale identity for tailnet spokes, bearer token for network-listener spokes. No new auth scheme.

### Proxying

Routing uses the `@host` segment from the URL. When the browser opens a terminal on a remote session, the hub identifies the spoke from the `@host` segment and forwards with the host stripped:

```
browser → hub:  WS /ws/@server/project-a/sess-ghi789
hub → server:   WS /ws/project-a/sess-ghi789
server → container: WS /ws/sess-ghi789
```

Each layer only knows about its direct spokes. The hub doesn't need to know that `project-a` is a container; it just forwards to `server`. Server strips its `@`-prefixed segment and forwards to the next layer.

This is the same pattern gmuxd already uses between the browser and local runner sockets, extended to multiple hops.

### Launching

`POST /v1/launch` accepts an optional `peer` field. When set, the hub forwards the request to that peer's `/v1/launch` endpoint. The peer launches the session locally and the hub discovers it via the SSE stream.

```json
{
  "launcher": "pi",
  "cwd": "/workspace/gmux",
  "peer": "server"
}
```

When `peer` is omitted, the session launches locally (current behavior).

### Fault tolerance

Peer connections are resilient:

- The hub tracks liveness for each spoke via the SSE connection. A dropped connection means the spoke is offline.
- When a spoke goes offline, its sessions are marked as disconnected (grey) in the UI. They remain visible so you know what was running. The spoke itself appears as offline in the peer status display.
- The hub attempts reconnection with exponential backoff. When the spoke comes back, sessions go live again.
- The hub never blocks on a slow or dead spoke. Each spoke subscription is independent.
- For multi-layer setups: if an intermediate spoke goes offline, all sessions behind it (including its own spokes' sessions) are marked as disconnected together.

## UI changes

### Sidebar

The sidebar shows projects configured by the user (see [Project Management](/planned/folder-management)). Sessions from spokes are matched to projects using the same rules as local sessions: remote-based projects match on the session's remote URLs, path-based projects match on cwd/workspace_root prefix.

For remote-based projects, cross-machine grouping is automatic: a session on the desktop with the same remote URL as a session on the server both appear under the same project.

Each session shows a subtle peer indicator (hostname or icon) so you can tell where it's running. Sessions on the local peer have no indicator.

The project list, ordering, and visibility are owned by the hub gmuxd. Spokes serve sessions but have no knowledge of the hub's project configuration.

### URL routing

Sessions are addressable via hierarchical URL paths. The project is the top-level segment, since aggregation groups sessions from multiple hosts under one project:

```
/<project>/<adapter>/<slug>              (local)
/<project>/@<host>/<adapter>/<slug>      (remote)
```

Examples:

```
/gmux/pi/fix-auth-bug                   (local session)
/gmux/shell/pytest-watch                (local session)
/gmux/@desktop/pi/fix-auth-bug          (session on desktop spoke)
/gmux/@server/shell/pytest-watch        (session on server spoke)
```

The `@` prefix on the host segment disambiguates it from an adapter name at any segment count. The router checks: after the project segment, if the next segment starts with `@`, it's a host; otherwise it's an adapter.

Partial URLs navigate naturally:

```
/gmux                        → project overview (all hosts)
/gmux/pi                     → all local pi sessions
/gmux/@desktop               → all desktop sessions for this project
/gmux/@desktop/pi            → pi sessions on desktop
/gmux/@desktop/pi/fix-auth   → specific remote session
```

Each segment is meaningful:

- **project**: the user-configured project slug (see [Project Management](/planned/folder-management)). Always first, matching the sidebar's primary grouping.
- **@host**: maps to the spoke namespace. Absent for local sessions. The `@` prefix is reserved in `parseSessionPath` so existing local URLs are never ambiguous with future host names.
- **adapter**: the session's `kind` (`pi`, `claude`, `shell`, etc.). Gives each adapter its own namespace, so adapters don't need to coordinate slug uniqueness.
- **slug**: adapter-provided stable identifier. See [Session Schema](/develop/session-schema) for the `slug` field.

The slug is stable across kill and resume: it's tied to the logical session (conversation ID, session file), not the process. Bookmarking `/gmux/pi/fix-auth-bug` works across restarts.

URLs are useful beyond the browser: external tools, notification actions, CI integrations, and scripts can link directly to a specific session.

### Peer status

A footer or header element shows connected spokes with their status (online, reconnecting, offline). Clicking a spoke could filter the sidebar to only show that spoke's sessions.

### Launch target

The launch modal gains a peer selector. When you have spokes configured, you choose where to launch. The default is the local machine. For projects that have an associated devcontainer, the peer selector could auto-suggest the right container.

## Devcontainers

Cross-instance provides the connection layer. Devcontainer support builds on top.

### Architecture

Each devcontainer runs its own gmuxd, listening on the Docker network. The host gmuxd connects to it as a peer. No shared volumes, no socket leakage between containers.

```
host gmuxd
  ├── local sessions
  ├── ← container-a gmuxd (peer, Docker network)
  │      └── container-a sessions
  └── ← container-b gmuxd (peer, Docker network)
         └── container-b sessions
```

The container's gmuxd does not need tailscale. It uses the network listener (`network.listen = "0.0.0.0"`) with bearer-token auth on the Docker bridge. The host gmuxd is the only client.

### Lifecycle

When a project folder has a `.devcontainer/devcontainer.json`, gmuxd manages the container lifecycle:

1. **Start**: `devcontainer up --workspace-folder <path>` builds and starts the container if needed.
2. **Connect**: gmuxd connects to the container's gmuxd as a peer.
3. **Launch**: sessions in this project are launched inside the container via the peer's `/v1/launch`.
4. **Stop**: containers can be stopped from the UI or left running (user preference).

The `devcontainer` CLI handles all the complexity of building images, installing features, applying dotfiles. gmuxd just calls it.

### gmux as a devcontainer Feature

A devcontainer Feature (`ghcr.io/gmuxapp/features/gmux`) installs gmux and gmuxd into any devcontainer. Users add it to their `devcontainer.json`:

```json
{
  "image": "mcr.microsoft.com/devcontainers/base:debian",
  "features": {
    "ghcr.io/gmuxapp/features/gmux": {}
  }
}
```

The feature:
- Installs `gmux` and `gmuxd` binaries.
- Configures gmuxd to listen on `0.0.0.0:8790` with a generated bearer token.
- Sets up gmuxd as the container entrypoint (or an init process alongside the user's entrypoint).
- Writes the bearer token to a well-known path so the host gmuxd can read it.

### Dotfiles

Devcontainers have native dotfiles support. Users configure their dotfiles repo in `devcontainer.json` or their editor settings, and the devcontainer CLI clones and installs them on container creation. This is orthogonal to gmux; gmux doesn't need to know about dotfiles.

## Incremental delivery

Project management (server-side project state, management UI, URL routing) ships first as a prerequisite. It fixes client sync, establishes the project identity model, and introduces the URL structure that aggregation extends with the `@host` segment. See [Project Management](/planned/folder-management).

### Step 1: Canonical project URI

Add `project_uri` to the runner's session metadata. Detected from VCS remote at session startup. Included in `/meta` response, stored in `store.Session`, broadcast via SSE. The frontend uses it for grouping within a single gmuxd instance (replaces path-based grouping for repos with remotes).

Small, self-contained. Useful today for workspace grouping even without cross-instance.

### Step 2: Hub protocol

gmuxd can connect to other gmuxd instances as spokes. Manual `[[peers]]` config. Preserved namespace chains, SSE subscription, multi-layer WebSocket proxying, launch forwarding, spoke liveness tracking. No auto-discovery yet.

This unlocks the "one dashboard, every machine" use case for users with explicit config. The `@host` segment slots into existing URLs: `/gmux/pi/fix-auth-bug` becomes `/gmux/@desktop/pi/fix-auth-bug`. The project remains the top-level segment, matching the sidebar's primary grouping.

### Step 3: Tailscale auto-discovery

gmuxd queries the tailnet for other gmux instances. Zero-config for the common case. Builds on the hub protocol from step 2.

### Step 4: Devcontainer integration

gmuxd detects `.devcontainer/devcontainer.json` in project folders. Manages container lifecycle via the `devcontainer` CLI. Connects to container gmuxd as a spoke. Multi-layer proxying means the host's own hub sees container sessions transparently. gmux devcontainer Feature for easy installation.

Each step is independently useful and shippable.

---
title: host.toml
description: Reference for ~/.config/gmux/host.toml — daemon behavior.
tableOfContents:
  maxHeadingLevel: 3
---

`~/.config/gmux/host.toml` (or `$XDG_CONFIG_HOME/gmux/host.toml`)

Daemon behavior. gmuxd reads this file once at startup. Create or edit it manually. The only command that modifies this file is `gmux remote`, which can add the `[tailscale]` section with your confirmation. If the file does not exist, safe defaults are used. Changes require restarting gmuxd.

## Example

```toml
# TCP port for the HTTP listener.
# Default: 8790
port = 8790

# Semantic-agent policy for this host.
[agent]
# Per behavioral root, cap live semantic agents at each descendant depth.
# Direct children are unlimited, grandchildren share 8 slots, deeper spawning is blocked.
max_subagents_by_depth = [-1, 8]

# Optional Tailscale remote access.
# See the Remote Access guide for setup.
[tailscale]
enabled = false
allow = []               # additional login names or device tags (owner is auto-whitelisted)

# Auto-discover devcontainer peers. Defaults to true.
[discovery]
devcontainers = true     # subscribe to Docker events, register gmux containers

# Dead-session scrollback cache target.
[sessions]
scrollback_cache_mb = 256

# Optional best-effort phone notifications via ntfy.
# Use `chmod 600 ~/.config/gmux/host.toml` before enabling.
[notifications.ntfy]
enabled = false
server_url = "https://ntfy.sh"
topic = "gmux_USE_A_LONG_RANDOM_TOPIC"
```

## Node identity

This host's name — what peers see in their UI and URLs — is **not** configured here. When Tailscale is enabled the name is your Tailscale machine name (owned and kept stable by Tailscale itself); otherwise it is the OS hostname. The first time the daemon joins a tailnet it requests `gmux-<hostname>`, and Tailscale keeps that name across restarts and container recreation. See [ADR 0007](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0007-host-identity-and-peer-urls.md).

To seed a specific name at first registration — e.g. when running several daemons on one machine — set the `GMUXD_TS_HOSTNAME` environment variable (used verbatim). It only applies before the node is registered; afterward Tailscale owns the name.

## Connecting to other hosts

There is **no `[[peers]]` config**. Add a host you want to aggregate sessions from at runtime via **Settings → Hosts → Connect to host** (paste the connect URL from `gmux auth`, or enter the host’s URL and token). A token is required for every host, tailnet or not ([ADR 0008](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0008-peer-authentication-via-token.md)). Connected hosts are stored in the daemon’s SQLite database (`state.db`), and the peer’s name is taken from the host itself — you don’t assign one.

## Fields

### Top-level

| Field | Type | Default | Range | Description |
|-------|------|---------|-------|-------------|
| `port` | `number` | `8790` | 1–65535 | TCP port for the HTTP listener. |

### `[agent]`

What `gmux agent …` and the web launch flow may do on this host. The budget
counts semantic agents only, so shell and process children in a family are
never charged against it.

| Field | Type | Default | Range | Description |
|-------|------|---------|-------|-------------|
| `max_subagents_by_depth` | `number[]` or `false` | `[-1, 8]` | 1–8 entries; each -1 or 0–1024 | Shared live semantic-agent budget per behavioral root and descendant depth. Only the first entry may be `-1` (unlimited). |

`max_subagents_by_depth` is read once at daemon startup. Array element zero
caps the root's direct children, element one caps grandchildren, and so on.
Depths omitted from the array have a limit of zero, so `[-1, 8]` is equivalent
to `[-1, 8, 0]`: direct children are unlimited, all children collectively
share eight grandchild slots, and grandchildren cannot spawn agents. Set the
field to `false` to disable admission protection while retaining launch
accounting. There is no environment, CLI, UI, or per-root override.

The limits are **shared per behavioral root at each depth**, not repeated for
every parent. This gives the root's whole swarm an absolute budget for
autonomous hiring: a recursive instruction propagated to 500 children can
create at most eight grandchildren under the default, rather than 4,000.

The budget follows the current family edge (`parent_session_id`), not immutable
launch provenance. Depth counts every family edge,
including an intervening shell/process session; only live semantic-agent
sessions consume slots at the resulting depth. Reparenting moves a live subtree
and its depth counts immediately. `gmux promote` clears the edge, making the
session a root with a fresh depth budget; reparenting it back under its former
parent rejoins that root's budget. Dead retained
sessions and remote projections do not consume slots. Independent top-level
`--new` launches create independent roots.

A slot is reserved atomically before gmux creates the runner, PTY, or durable
session row. It becomes a live slot when registration succeeds. Failures and
runner exits release it. "Active" means **live/resident semantic-agent
session**, not merely a turn producing output. A refusal exits non-zero with
the stable code `subagent_limit_reached`, identifies the root and depth, and
suggests `gmux ls`. The daemon owns only local admission; it does not coordinate
a distributed quota with network peers.

The bind address is not configurable here — it is the `GMUXD_LISTEN` environment variable (default `127.0.0.1`). See [Environment variables](/reference/environment/#bind-address).

### `[sessions]`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `scrollback_cache_mb` | `number` | `256` | Aggregate cache target for dead-session scrollback. `0` disables the limit. |

Session values must be non-negative.

### `[notifications.ntfy]`

Publishes a privacy-safe notification after gmux's existing completion grace period and presence checks. Publishing is **best effort**: gmux makes one asynchronous request with a short timeout. It does not retry, queue, persist, or replay notifications after restart. A network error, daemon shutdown, or busy publisher may lose a notification. Browser notifications continue independently.

ntfy is configured on the daemon that owns the session. An aggregation host does not publish notifications for sessions projected from another host or devcontainer.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | `boolean` | `false` | Enable ntfy publishing. When enabled, `host.toml` must be readable only by its owner (`0600` or stricter). |
| `server_url` | `string` | `"https://ntfy.sh"` | ntfy server origin. HTTP(S) only; no credentials, query, fragment, or sub-path. |
| `topic` | `string` | none | Required when enabled. Use a long random topic; on an open server it acts as a secret. Letters, digits, `_`, and `-`; maximum 64 characters. |
| `token` | `string` | none | Optional Bearer publish token. Mutually exclusive with Basic auth. HTTPS required. |
| `username` / `password` | `string` | none | Optional Basic auth pair. Both are required together; HTTPS required. |
| `priority` | `number` | `3` | ntfy priority from 1 to 5. |
| `tags` | `string[]` | `[]` | Up to eight ntfy tags. |
| `click_url` | `string` | none | Optional absolute HTTP(S) dashboard URL opened from the notification. Authentication must not be embedded in it. |
| `timeout` | duration string | `"5s"` | Total timeout for the single publish attempt; 1–30 seconds. |

The payload identifies only the host, adapter, and opaque session ID. gmux does not send prompts, transcript/output, commands, working directories, project names, or session titles. Credentials, the server URL, topic, payload, and response body are not logged.

Example with a dedicated publish token:

```toml
[notifications.ntfy]
enabled = true
server_url = "https://ntfy.example.net"
topic = "gmux_Q7f9x2mP4vN8kL3s"
token = "tk_REPLACE_WITH_A_PUBLISH_ONLY_TOKEN"
priority = 3
tags = ["gmux", "white_check_mark"]
click_url = "https://gmux.example.net/"
timeout = "5s"
```

Before restarting gmuxd:

```sh
chmod 600 ~/.config/gmux/host.toml
```

### `[tailscale]`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | `boolean` | `false` | Enable Tailscale remote access. |
| `allow` | `string[]` | `[]` | Additional Tailscale login names (e.g. `user@github`) or device tags (e.g. `tag:gmux`) to allow (owner is auto-whitelisted). Login entries must contain `@`; tag entries start with `tag:`. |

### `[discovery]`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `devcontainers` | `boolean` | `true` | Subscribe to Docker events and register any container with the gmux devcontainer feature **and** the `devcontainer.local_folder` label as a peer. Skipped if the Docker CLI is not installed. |

There is no `tailscale` discovery flag (removed in [ADR 0008](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0008-peer-authentication-via-token.md)). Tailnet autodiscovery was removed because auto-connecting peers without a token let a single compromised node drive the whole tailnet; add tailnet hosts manually via **Connect to host**.

## Strict validation

The config file is strictly validated at startup. gmuxd refuses to start if:

- **Unknown keys** are present, catching typos like `alow` instead of `allow`
- **`allow` entries don't contain `@` and don't start with `tag:`**, likely not a valid Tailscale login name or device tag
- **`allow` tag entries are malformed** — the name after `tag:` must start with a letter and contain only lowercase letters, digits, and hyphens
- **`port` is out of range** (must be 1–65535)
- **`agent.max_subagents_by_depth` is `true`, is not an integer array or `false`, is empty, has over eight entries, has an entry above 1024, or uses `-1` after the first entry**
- **A session limit is negative**, or a retention/cache value is too large to convert safely to its runtime duration or byte count
- **ntfy settings are unsafe or malformed** — including a missing/invalid topic, unsupported URL, mixed authentication modes, credentials over plaintext HTTP, priority/tag/timeout violations, or an enabled config file with group/other permissions
- **A TOML integer is outside the supported integer range**, or other TOML syntax is invalid

This is intentional. Silent fallback to defaults is dangerous for security settings. See [Security](/security) for the reasoning.

Three keys were **removed** (ADR 0007 / ADR 0008) and are now **ignored with a deprecation warning** (rather than failing startup), so upgrading a host that still has an old config doesn't brick the daemon. Remove them to silence the warning:

- **`tailscale.hostname`** (ADR 0007) — the node name now comes from Tailscale / the OS hostname.
- **`[[peers]]`** (ADR 0007) — manual peers are runtime state; add them via *Connect to host* (stored in `state.db`).
- **`discovery.tailscale`** (ADR 0008) — tailnet autodiscovery was removed; add tailnet hosts via *Connect to host*.
